package booxpipeline

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// filenameDatePrefixRE matches a YYYYMMDD prefix at the start of a filename.
var filenameDatePrefixRE = regexp.MustCompile(`^(\d{4})(\d{2})(\d{2})`)

// autoNamedNotebookRE matches Boox firmware-default filenames: "Notebook-"
// followed by a digit. Excludes user-renamed files like "Notebook-please
// transcribe me.note" (no leading digit after the dash).
var autoNamedNotebookRE = regexp.MustCompile(`^Notebook-\d`)

// Store manages Boox notes and jobs in SQLite.
type Store struct {
	db        *sql.DB
	notesRoot string
}

// NewStore creates a new Boox pipeline store.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// NewStoreWithRoot creates a new Boox pipeline store with a notes root for version discovery.
func NewStoreWithRoot(db *sql.DB, notesRoot string) *Store {
	return &Store{db: db, notesRoot: notesRoot}
}

// BooxNote represents a Boox note in the database.
type BooxNote struct {
	Path        string
	NoteID      string
	Title       string
	DeviceModel string
	NoteType    string
	Folder      string
	PageCount   int
	FileHash    string
	Version     int
	CreatedAt   int64
	UpdatedAt   int64
}

// BooxJob represents a processing job in the queue.
type BooxJob struct {
	ID           int64
	NotePath     string
	Status       string
	SkipReason   string
	OCRSource    string
	APIModel     string
	Attempts     int
	LastError    string
	QueuedAt     int64
	StartedAt    int64
	FinishedAt   int64
	RequeueAfter *int64
}

// BooxNoteEntry is a summary for web display.
type BooxNoteEntry struct {
	Path        string
	Title       string
	DeviceModel string
	NoteType    string
	Folder      string
	PageCount   int
	Version     int
	NoteID      string // top-level directory name from ZIP, used for cache paths
	CreatedAt   int64  // unix millis
	UpdatedAt   int64  // unix millis
	JobStatus   string // latest job status
}

// BooxVersion represents an archived version of a Boox note.
type BooxVersion struct {
	Path      string
	Timestamp string // formatted timestamp from filename
	SizeBytes int64
}

// UpsertNote inserts or updates a boox_notes row, incrementing version on conflict.
// createdAt is only used for the initial INSERT; updates preserve the existing value.
func (s *Store) UpsertNote(ctx context.Context, path, noteID, title, deviceModel, noteType, folder string, pageCount int, fileHash string, createdAt int64) error {
	now := time.Now().UnixMilli()
	if createdAt <= 0 {
		createdAt = now
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO boox_notes (path, note_id, title, device_model, note_type, folder, page_count, file_hash, version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			note_id = excluded.note_id,
			title = excluded.title,
			device_model = excluded.device_model,
			note_type = excluded.note_type,
			folder = excluded.folder,
			page_count = excluded.page_count,
			file_hash = excluded.file_hash,
			version = version + 1,
			updated_at = excluded.updated_at`,
		path, noteID, title, deviceModel, noteType, folder, pageCount, fileHash, createdAt, now,
	)
	return err
}

// NoteCreatedAt extracts a creation timestamp (millis) from the title's
// YYYYMMDD prefix, falling back to the file's mtime on disk.
func NoteCreatedAt(title, filePath string) int64 {
	if t, ok := ParseTitleDate(title); ok {
		return t.UnixMilli()
	}
	if info, err := os.Stat(filePath); err == nil {
		return info.ModTime().UnixMilli()
	}
	return 0
}

// ParseTitleDate extracts a date from a YYYYMMDD prefix in the title.
func ParseTitleDate(title string) (time.Time, bool) {
	s := strings.TrimSpace(title)
	if len(s) < 8 {
		return time.Time{}, false
	}
	t, err := time.Parse("20060102", s[:8])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// EnqueueJob adds a job to the queue, first ensuring a boox_notes row exists with minimal defaults.
func (s *Store) EnqueueJob(ctx context.Context, notePath string) error {
	// First ensure a boox_notes row exists (INSERT OR IGNORE with minimal defaults).
	// This satisfies the FK constraint for the WebDAV callback before the worker
	// has parsed the note metadata.
	title := strings.TrimSuffix(filepath.Base(notePath), filepath.Ext(notePath))
	createdAt := NoteCreatedAt(title, notePath)
	now := time.Now().UnixMilli()
	if createdAt <= 0 {
		createdAt = now
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO boox_notes (path, note_id, title, device_model, note_type, folder, page_count, file_hash, version, created_at, updated_at)
		VALUES (?, '', '', '', '', '', 0, '', 1, ?, ?)`,
		notePath, createdAt, now,
	)
	if err != nil {
		return fmt.Errorf("ensure note row: %w", err)
	}

	// Now insert the job. Use Unix() (seconds) for consistency with ClaimNextJob and ReclaimStuckJobs.
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO boox_jobs (note_path, status, queued_at)
		VALUES (?, 'pending', ?)`,
		notePath, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("enqueue job: %w", err)
	}
	return nil
}

// ClaimNextJob atomically claims the oldest pending job using SQLite RETURNING.
// Returns nil, nil if no jobs are available.
//
// Each claim bumps `attempts` by 1, so the column reflects "number of times
// the worker has started this job." Previously attempts was only incremented
// by the watchdog (ReclaimStuckJobs / ReclaimAllInProgress), which made the
// column useless for the Details modal — successful-first-try jobs all read
// as 0 and even requeued-then-completed jobs usually read 0 unless they
// happened to time out.
func (s *Store) ClaimNextJob(ctx context.Context) (*BooxJob, error) {
	now := time.Now().Unix()
	var job BooxJob
	err := s.db.QueryRowContext(ctx, `
		UPDATE boox_jobs SET status = 'in_progress', started_at = ?, attempts = attempts + 1
		WHERE id = (SELECT id FROM boox_jobs WHERE status = 'pending'
			AND (requeue_after IS NULL OR requeue_after <= ?)
			ORDER BY queued_at ASC LIMIT 1)
		RETURNING id, note_path, status, attempts, last_error, queued_at, started_at`,
		now, now,
	).Scan(&job.ID, &job.NotePath, &job.Status, &job.Attempts, &job.LastError, &job.QueuedAt, &job.StartedAt)
	if err == sql.ErrNoRows {
		return nil, nil // no jobs available
	}
	if err != nil {
		return nil, fmt.Errorf("claim boox job: %w", err)
	}
	return &job, nil
}

// CompleteJob marks a job as done with optional OCR source and API model.
func (s *Store) CompleteJob(ctx context.Context, jobID int64, ocrSource, apiModel string) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		UPDATE boox_jobs SET status = 'done', ocr_source = ?, api_model = ?, finished_at = ?
		WHERE id = ?`,
		ocrSource, apiModel, now, jobID,
	)
	if err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	return nil
}

// FailJob marks a job as failed with an error message.
func (s *Store) FailJob(ctx context.Context, jobID int64, errMsg string) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		UPDATE boox_jobs SET status = 'failed', last_error = ?, finished_at = ?
		WHERE id = ?`,
		errMsg, now, jobID,
	)
	if err != nil {
		return fmt.Errorf("fail job: %w", err)
	}
	return nil
}

// GetLatestJob returns the most recent job for a note path.
func (s *Store) GetLatestJob(ctx context.Context, notePath string) (*BooxJob, error) {
	var job BooxJob
	err := s.db.QueryRowContext(ctx, `
		SELECT id, note_path, status, skip_reason, ocr_source, api_model,
		       attempts, last_error, queued_at, started_at, finished_at
		FROM boox_jobs WHERE note_path = ?
		ORDER BY id DESC LIMIT 1`,
		notePath,
	).Scan(&job.ID, &job.NotePath, &job.Status, &job.SkipReason, &job.OCRSource, &job.APIModel,
		&job.Attempts, &job.LastError, &job.QueuedAt, &job.StartedAt, &job.FinishedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get latest boox job: %w", err)
	}
	return &job, nil
}

// HasDoneJobWithHash returns true if the boox_notes row at path already has
// the given file_hash AND at least one done job. Used by the worker to skip
// re-processing when a WebDAV re-upload delivers identical content.
func (s *Store) HasDoneJobWithHash(ctx context.Context, path, fileHash string) (bool, error) {
	if fileHash == "" {
		return false, nil
	}
	var exists int
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1
			FROM boox_notes bn
			JOIN boox_jobs bj ON bj.note_path = bn.path
			WHERE bn.path = ? AND bn.file_hash = ? AND bj.status = 'done'
		)`,
		path, fileHash,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("has done job with hash: %w", err)
	}
	return exists == 1, nil
}

// GetNote retrieves a boox_notes row by path.
func (s *Store) GetNote(ctx context.Context, path string) (*BooxNote, error) {
	var note BooxNote
	err := s.db.QueryRowContext(ctx, `
		SELECT path, note_id, title, device_model, note_type, folder, page_count, file_hash, version, created_at, updated_at
		FROM boox_notes WHERE path = ?`,
		path,
	).Scan(&note.Path, &note.NoteID, &note.Title, &note.DeviceModel, &note.NoteType, &note.Folder, &note.PageCount, &note.FileHash, &note.Version, &note.CreatedAt, &note.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get note: %w", err)
	}
	return &note, nil
}

// GetNoteID returns the note_id for a given note path, used for cache path resolution.
func (s *Store) GetNoteID(ctx context.Context, path string) (string, error) {
	var noteID string
	err := s.db.QueryRowContext(ctx, `
		SELECT note_id FROM boox_notes WHERE path = ?`,
		path,
	).Scan(&noteID)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("note not found")
	}
	if err != nil {
		return "", fmt.Errorf("get note id: %w", err)
	}
	return noteID, nil
}

// CountNotesWithPrefix returns how many boox_notes paths start with the given prefix.
func (s *Store) CountNotesWithPrefix(ctx context.Context, prefix string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM boox_notes WHERE path LIKE ?`, prefix+"%").Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count notes with prefix: %w", err)
	}
	return count, nil
}

// ListNotesWithPrefix returns all boox_notes paths that start with the given prefix.
func (s *Store) ListNotesWithPrefix(ctx context.Context, prefix string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM boox_notes WHERE path LIKE ?`, prefix+"%")
	if err != nil {
		return nil, fmt.Errorf("list notes with prefix: %w", err)
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// UpdateNotePath atomically updates all references from oldPath to newPath
// across boox_notes, boox_jobs, and note_content tables.
// FK checks are disabled for the duration since we're updating the PK
// that the FK references (PRAGMA must be set outside a transaction in SQLite).
func (s *Store) UpdateNotePath(ctx context.Context, oldPath, newPath string) error {
	// Disable FK checks — must happen outside a transaction in SQLite.
	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable fk: %w", err)
	}
	defer s.db.ExecContext(ctx, `PRAGMA foreign_keys = ON`)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Update boox_notes (PK).
	if _, err := tx.ExecContext(ctx, `UPDATE boox_notes SET path = ? WHERE path = ?`, newPath, oldPath); err != nil {
		return fmt.Errorf("update boox_notes: %w", err)
	}

	// Update boox_jobs (FK child).
	if _, err := tx.ExecContext(ctx, `UPDATE boox_jobs SET note_path = ? WHERE note_path = ?`, newPath, oldPath); err != nil {
		return fmt.Errorf("update boox_jobs: %w", err)
	}

	// Update note_content (search index — triggers update FTS5).
	if _, err := tx.ExecContext(ctx, `UPDATE note_content SET note_path = ? WHERE note_path = ?`, newPath, oldPath); err != nil {
		return fmt.Errorf("update note_content: %w", err)
	}

	return tx.Commit()
}

// MoveNote atomically updates a note's path (across boox_notes, boox_jobs,
// note_content) AND its folder field. Used by the Boox Move-to-folder
// operation. The caller is responsible for moving the underlying file +
// .versions archive on disk before calling this; this method only updates
// database state.
func (s *Store) MoveNote(ctx context.Context, oldPath, newPath, newFolder string) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable fk: %w", err)
	}
	defer s.db.ExecContext(ctx, `PRAGMA foreign_keys = ON`)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx,
		`UPDATE boox_notes SET path = ?, folder = ?, updated_at = ? WHERE path = ?`,
		newPath, newFolder, now, oldPath); err != nil {
		return fmt.Errorf("update boox_notes: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE boox_jobs SET note_path = ? WHERE note_path = ?`,
		newPath, oldPath); err != nil {
		return fmt.Errorf("update boox_jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE note_content SET note_path = ? WHERE note_path = ?`,
		newPath, oldPath); err != nil {
		return fmt.Errorf("update note_content: %w", err)
	}
	return tx.Commit()
}

// SkipNote marks a note's latest job as skipped with a reason.
func (s *Store) SkipNote(ctx context.Context, path, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE boox_jobs SET status = 'skipped', skip_reason = ?
		WHERE note_path = ? AND id = (SELECT id FROM boox_jobs WHERE note_path = ? ORDER BY id DESC LIMIT 1)`,
		reason, path, path,
	)
	if err != nil {
		return fmt.Errorf("skip note: %w", err)
	}
	return nil
}

// UnskipNote resets a skipped job back to pending.
func (s *Store) UnskipNote(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE boox_jobs SET status = 'pending', skip_reason = '', queued_at = ?
		WHERE note_path = ? AND status = 'skipped'
		AND id = (SELECT id FROM boox_jobs WHERE note_path = ? ORDER BY id DESC LIMIT 1)`,
		time.Now().Unix(), path, path,
	)
	if err != nil {
		return fmt.Errorf("unskip note: %w", err)
	}
	return nil
}

// DeleteNote removes a boox_notes record and all associated boox_jobs and note_content rows.
func (s *Store) DeleteNote(ctx context.Context, path string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM boox_jobs WHERE note_path = ?`, path); err != nil {
		return fmt.Errorf("delete jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM note_content WHERE note_path = ?`, path); err != nil {
		return fmt.Errorf("delete content: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM boox_notes WHERE path = ?`, path); err != nil {
		return fmt.Errorf("delete note: %w", err)
	}

	return tx.Commit()
}

// QueueStatus represents the current state of the processing queue.
type QueueStatus struct {
	ActiveTitle     string `json:"active_title,omitempty"` // title of currently processing note
	ActivePages     int    `json:"active_pages,omitempty"` // page count of active note
	Pending         int    `json:"pending"`
	InProgress      int    `json:"in_progress"`
	Done            int    `json:"done"`
	Failed          int    `json:"failed"`
	UnmigratedCount int    `json:"unmigrated_count,omitempty"` // files still at import path
}

// GetQueueStatus returns a summary of the processing queue.
func (s *Store) GetQueueStatus(ctx context.Context) (QueueStatus, error) {
	var qs QueueStatus
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE((SELECT COUNT(*) FROM boox_jobs WHERE status = 'pending'), 0),
			COALESCE((SELECT COUNT(*) FROM boox_jobs WHERE status = 'in_progress'), 0),
			COALESCE((SELECT COUNT(*) FROM boox_jobs WHERE status = 'done'), 0),
			COALESCE((SELECT COUNT(*) FROM boox_jobs WHERE status = 'failed'), 0)
	`).Scan(&qs.Pending, &qs.InProgress, &qs.Done, &qs.Failed)
	if err != nil {
		return qs, fmt.Errorf("queue status: %w", err)
	}
	// Get active job details.
	if qs.InProgress > 0 {
		s.db.QueryRowContext(ctx, `
			SELECT bn.title, bn.page_count
			FROM boox_jobs bj JOIN boox_notes bn ON bn.path = bj.note_path
			WHERE bj.status = 'in_progress' LIMIT 1
		`).Scan(&qs.ActiveTitle, &qs.ActivePages)
	}
	return qs, nil
}

// RetryAllFailed resets all failed jobs back to pending.
// Returns the number of jobs reset.
func (s *Store) RetryAllFailed(ctx context.Context) (int64, error) {
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `
		UPDATE boox_jobs SET status = 'pending', last_error = '', queued_at = ?
		WHERE status = 'failed'`,
		now,
	)
	if err != nil {
		return 0, fmt.Errorf("retry failed jobs: %w", err)
	}
	return result.RowsAffected()
}

// ReclaimAllInProgress resets all in_progress jobs back to pending.
// Called on startup to recover from crashes/restarts that left orphaned jobs.
func (s *Store) ReclaimAllInProgress(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE boox_jobs SET status = 'pending', attempts = attempts + 1
		WHERE status = 'in_progress'`,
	)
	if err != nil {
		return fmt.Errorf("reclaim in_progress jobs: %w", err)
	}
	return nil
}

// ReclaimStuckJobs reclaims jobs that have been in_progress for longer than the timeout.
// Sets status back to pending and increments the attempts counter.
func (s *Store) ReclaimStuckJobs(ctx context.Context, timeout time.Duration) error {
	cutoff := time.Now().Add(-timeout).Unix()
	_, err := s.db.ExecContext(ctx, `
		UPDATE boox_jobs SET status = 'pending', attempts = attempts + 1
		WHERE status = 'in_progress' AND started_at < ?`,
		cutoff,
	)
	if err != nil {
		return fmt.Errorf("reclaim stuck jobs: %w", err)
	}
	return nil
}

// ListNotes returns all Boox notes with their latest job status.
func (s *Store) ListNotes(ctx context.Context) ([]BooxNoteEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT bn.path, bn.title, bn.device_model, bn.note_type, bn.folder,
		       bn.page_count, bn.version, bn.note_id, bn.created_at, bn.updated_at,
		       COALESCE((SELECT status FROM boox_jobs WHERE note_path = bn.path
		                ORDER BY id DESC LIMIT 1), '') as job_status
		FROM boox_notes bn
		ORDER BY bn.updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list notes query: %w", err)
	}
	defer rows.Close()

	var entries []BooxNoteEntry
	for rows.Next() {
		var e BooxNoteEntry
		if err := rows.Scan(&e.Path, &e.Title, &e.DeviceModel, &e.NoteType, &e.Folder,
			&e.PageCount, &e.Version, &e.NoteID, &e.CreatedAt, &e.UpdatedAt, &e.JobStatus); err != nil {
			return nil, fmt.Errorf("scan note row: %w", err)
		}
		entries = append(entries, e)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list notes iteration: %w", err)
	}
	return entries, nil
}

// FolderCount is one row in the Boox folder facet — the on-device folder
// string and how many notes live in it.
type FolderCount struct {
	Folder string
	Count  int
}

// ListFolders returns unique Boox folders with note counts, sorted by
// folder name. Empty-folder rows (catalog entries without a folder) are
// reported under a zero-length Folder string.
func (s *Store) ListFolders(ctx context.Context) ([]FolderCount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT folder, COUNT(*) FROM boox_notes GROUP BY folder ORDER BY folder`)
	if err != nil {
		return nil, fmt.Errorf("list folders query: %w", err)
	}
	defer rows.Close()

	var out []FolderCount
	for rows.Next() {
		var fc FolderCount
		if err := rows.Scan(&fc.Folder, &fc.Count); err != nil {
			return nil, fmt.Errorf("scan folder row: %w", err)
		}
		out = append(out, fc)
	}
	return out, rows.Err()
}

// DeviceCount is one row in the Boox device facet — the on-device model
// string and how many notes are attributed to it.
type DeviceCount struct {
	DeviceModel string
	Count       int
}

// ListDevices returns unique Boox device_model values with note counts,
// sorted by device name. Rows whose device_model is the ".." sentinel
// (field-swap artifact from a legacy import path) are filtered out so
// they don't clutter the UI; see docs/follow-ups-2026-04-13.md item 18.
func (s *Store) ListDevices(ctx context.Context) ([]DeviceCount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT device_model, COUNT(*) FROM boox_notes
		WHERE device_model != '..'
		GROUP BY device_model ORDER BY device_model`)
	if err != nil {
		return nil, fmt.Errorf("list devices query: %w", err)
	}
	defer rows.Close()

	var out []DeviceCount
	for rows.Next() {
		var dc DeviceCount
		if err := rows.Scan(&dc.DeviceModel, &dc.Count); err != nil {
			return nil, fmt.Errorf("scan device row: %w", err)
		}
		out = append(out, dc)
	}
	return out, rows.Err()
}

// GetVersions returns archived versions of a Boox note.
// Version files live at {notesRoot}/.versions/{relDir}/{nameNoExt}/{timestamp}.note
func (s *Store) GetVersions(ctx context.Context, path string) ([]BooxVersion, error) {
	// Derive the version directory from the note path.
	relPath, err := filepath.Rel(s.notesRoot, path)
	if err != nil {
		return nil, fmt.Errorf("compute rel path: %w", err)
	}

	relDir := filepath.Dir(relPath)
	baseName := filepath.Base(relPath)
	nameNoExt := strings.TrimSuffix(baseName, filepath.Ext(baseName))
	versionDir := filepath.Join(s.notesRoot, ".versions", relDir, nameNoExt)

	entries, err := os.ReadDir(versionDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no versions yet
		}
		return nil, fmt.Errorf("read version dir: %w", err)
	}

	var versions []BooxVersion
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// Parse timestamp from filename: e.g., "20260404T120000.note"
		name := e.Name()
		ts := strings.TrimSuffix(name, filepath.Ext(name))
		versions = append(versions, BooxVersion{
			Path:      filepath.Join(versionDir, name),
			Timestamp: ts,
			SizeBytes: info.Size(),
		})
	}
	return versions, nil
}

// ReconcileCreatedAtFromFilename updates boox_notes.created_at to match the
// YYYYMMDD prefix in the filename, for any rows where the parsed date doesn't
// already match the stored created_at's UTC date. Returns the number of rows
// updated. Rows whose basename has no date prefix are left alone.
func (s *Store) ReconcileCreatedAtFromFilename(ctx context.Context) (int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, created_at FROM boox_notes`)
	if err != nil {
		return 0, fmt.Errorf("query boox_notes: %w", err)
	}
	defer rows.Close()

	type update struct {
		path     string
		targetMs int64
	}
	var updates []update
	for rows.Next() {
		var path string
		var createdAt int64
		if err := rows.Scan(&path, &createdAt); err != nil {
			return 0, fmt.Errorf("scan: %w", err)
		}
		base := filepath.Base(path)
		m := filenameDatePrefixRE.FindStringSubmatch(base)
		if m == nil {
			continue
		}
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		if y < 1970 || y > 2999 || mo < 1 || mo > 12 || d < 1 || d > 31 {
			continue
		}
		target := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
		if createdAt > 0 {
			cur := time.UnixMilli(createdAt).UTC()
			if cur.Year() == target.Year() && cur.Month() == target.Month() && cur.Day() == target.Day() {
				continue
			}
		}
		updates = append(updates, update{path: path, targetMs: target.UnixMilli()})
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("rows: %w", err)
	}
	if len(updates) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	var count int64
	for _, u := range updates {
		res, err := tx.ExecContext(ctx,
			`UPDATE boox_notes SET created_at = ?, updated_at = ? WHERE path = ?`,
			u.targetMs, now, u.path)
		if err != nil {
			return 0, fmt.Errorf("update %s: %w", u.path, err)
		}
		n, _ := res.RowsAffected()
		count += n
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return count, nil
}

// ListAutoNamedNotebooks returns rows whose basename matches the Boox firmware
// auto-naming pattern ("Notebook-" followed by a digit). Only path and note_id
// are populated.
func (s *Store) ListAutoNamedNotebooks(ctx context.Context) ([]BooxNote, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, note_id FROM boox_notes WHERE path LIKE '%/Notebook-%'`)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	var out []BooxNote
	for rows.Next() {
		var bn BooxNote
		if err := rows.Scan(&bn.Path, &bn.NoteID); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if autoNamedNotebookRE.MatchString(filepath.Base(bn.Path)) {
			out = append(out, bn)
		}
	}
	return out, rows.Err()
}
