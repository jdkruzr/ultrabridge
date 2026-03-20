package notestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrNotFound is returned when a requested file is not in the notes table.
var ErrNotFound = errors.New("note file not found")

// IsNotFound reports whether err is ErrNotFound.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// NoteStore reads and maintains file state in the SQLite notes table.
type NoteStore interface {
	// Scan walks the notes root, upserts file state, and returns absolute paths
	// of new or mtime-changed files that should be queued for processing.
	Scan(ctx context.Context) ([]string, error)
	// List returns the direct children of relPath (one level deep).
	// relPath="" returns the top-level contents.
	List(ctx context.Context, relPath string) ([]NoteFile, error)
	// Get returns the state of a single file by absolute path.
	Get(ctx context.Context, path string) (*NoteFile, error)
	// UpsertFile ensures a single file is present in the notes table.
	// Used by the pipeline watcher to satisfy the jobs FK constraint before enqueueing.
	UpsertFile(ctx context.Context, path string) error
	// SetHash stores the SHA-256 hex digest for the file at path.
	// Called by the worker after successful job completion and by the pipeline
	// after a job is transferred to a new path.
	SetHash(ctx context.Context, path, hash string) error
	// LookupByHash returns the path and job status of any note whose sha256 matches hash
	// and which has a completed (done) job. Returns found=false if no match exists.
	// Used at enqueue time to detect moved or renamed files.
	LookupByHash(ctx context.Context, hash string) (path string, found bool, err error)
	// TransferJob moves the job record for oldPath to newPath by updating the FK.
	// Used when move detection identifies that newPath contains the same content
	// as an already-processed oldPath. newPath must already exist in the notes table.
	// Returns an error if no job exists for oldPath or the FK constraint is violated.
	TransferJob(ctx context.Context, oldPath, newPath string) error
}

// Store implements NoteStore against a SQLite database.
type Store struct {
	db        *sql.DB
	notesPath string
}

// New creates a Store. notesPath is the absolute root directory to scan.
func New(db *sql.DB, notesPath string) *Store {
	return &Store{db: db, notesPath: notesPath}
}

// Get returns the state of a single file by absolute path.
func (s *Store) Get(ctx context.Context, path string) (*NoteFile, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT n.path, n.rel_path, n.file_type, n.size_bytes, n.mtime,
		       COALESCE(j.status, '') AS job_status
		FROM notes n
		LEFT JOIN (
			SELECT note_path, status
			FROM jobs
			WHERE id = (SELECT MAX(id) FROM jobs j2 WHERE j2.note_path = jobs.note_path)
		) j ON j.note_path = n.path
		WHERE n.path = ?`, path)

	return scanRow(row)
}

// List returns the direct children of relPath: subdirectories (from live filesystem)
// followed by files (from the notes DB with job status joined).
// Directories are returned first with IsDir=true and FileType="" — they are not
// stored in the notes table and have no job status.
func (s *Store) List(ctx context.Context, relPath string) ([]NoteFile, error) {
	var result []NoteFile

	// Subdirectories from live filesystem (not stored in notes table).
	if s.notesPath != "" {
		dirPath := filepath.Join(s.notesPath, relPath)
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			// Distinguish "not found" (acceptable, dir may not exist yet) from real errors
			if !os.IsNotExist(err) {
				// Real error like permission denied
				slog.Warn("notestore list: failed to read directory", "path", dirPath, "err", err)
			}
		} else {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				childRel := filepath.Join(relPath, e.Name())
				info, err := e.Info()
				if err != nil {
					// Log Info() errors but continue with next entry
					slog.Warn("notestore list: failed to get dir entry info", "name", e.Name(), "err", err)
					continue
				}
				var mtime time.Time
				if info != nil {
					mtime = info.ModTime().UTC()
				}
				result = append(result, NoteFile{
					Path:    filepath.Join(s.notesPath, childRel),
					RelPath: childRel,
					Name:    e.Name(),
					IsDir:   true,
					MTime:   mtime,
				})
			}
		}
	}

	// Files from the notes DB with latest job status.
	prefix := relPath
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT n.path, n.rel_path, n.file_type, n.size_bytes, n.mtime,
		       COALESCE(j.status, '') AS job_status
		FROM notes n
		LEFT JOIN (
			SELECT note_path, status
			FROM jobs
			WHERE id = (SELECT MAX(id) FROM jobs j2 WHERE j2.note_path = jobs.note_path)
		) j ON j.note_path = n.path
		WHERE n.rel_path LIKE ? AND n.rel_path NOT LIKE ?
		ORDER BY n.rel_path`,
		prefix+"%",
		prefix+"%/%",
	)
	if err != nil {
		return nil, fmt.Errorf("notestore list: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var path2, relPath2, fileType, jobStatus string
		var sizeBytes, mtimeUnix int64
		if err := rows.Scan(&path2, &relPath2, &fileType, &sizeBytes, &mtimeUnix, &jobStatus); err != nil {
			return nil, fmt.Errorf("notestore list scan: %w", err)
		}
		result = append(result, NoteFile{
			Path:      path2,
			RelPath:   relPath2,
			Name:      filepath.Base(relPath2),
			FileType:  FileType(fileType),
			SizeBytes: sizeBytes,
			MTime:     time.Unix(mtimeUnix, 0).UTC(),
			JobStatus: jobStatus,
		})
	}
	return result, rows.Err()
}

// SetHash stores the SHA-256 hex digest for the file at path.
func (s *Store) SetHash(ctx context.Context, path, hash string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE notes SET sha256=? WHERE path=?", hash, path)
	return err
}

// LookupByHash returns the path of any note whose sha256 matches hash and which has
// a done job. Returns found=false with nil error if no match exists.
func (s *Store) LookupByHash(ctx context.Context, hash string) (path string, found bool, err error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT n.path
		FROM notes n
		JOIN jobs j ON j.note_path = n.path
		WHERE n.sha256 = ? AND j.status = 'done'
		LIMIT 1`, hash)
	err = row.Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return path, true, nil
}

// TransferJob moves the job record for oldPath to newPath.
// The notes row for newPath must already exist (caller's responsibility).
func (s *Store) TransferJob(ctx context.Context, oldPath, newPath string) error {
	result, err := s.db.ExecContext(ctx,
		"UPDATE jobs SET note_path=? WHERE note_path=?", newPath, oldPath)
	if err != nil {
		return fmt.Errorf("transfer job %s → %s: %w", oldPath, newPath, err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("no job found for path %s", oldPath)
	}
	return nil
}

func scanRow(row *sql.Row) (*NoteFile, error) {
	var path2, relPath, fileType, jobStatus string
	var sizeBytes, mtimeUnix int64
	if err := row.Scan(&path2, &relPath, &fileType, &sizeBytes, &mtimeUnix, &jobStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("notestore get: %w", err)
	}
	return &NoteFile{
		Path:      path2,
		RelPath:   relPath,
		Name:      filepath.Base(relPath),
		FileType:  FileType(fileType),
		SizeBytes: sizeBytes,
		MTime:     time.Unix(mtimeUnix, 0).UTC(),
		JobStatus: jobStatus,
	}, nil
}
