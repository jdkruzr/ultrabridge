package taskdb

// pattern: Imperative Shell

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/sysop/ultrabridge/internal/taskstore"
)

// Store implements caldav.TaskStore against a local SQLite database.
type Store struct {
	db *sql.DB
}

// NewStore creates a new SQLite-backed task store.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

const taskColumns = `task_id, title, detail, status, importance, due_time,
	completed_time, last_modified, recurrence, is_reminder_on, links, is_deleted,
	ical_blob, created_at, forestnote_notebook_id, forestnote_page_id,
	forestnote_notebook_name, forestnote_source`

func (s *Store) List(ctx context.Context) ([]taskstore.Task, error) {
	return s.listRows(ctx, "SELECT "+taskColumns+" FROM tasks WHERE is_deleted = 'N'")
}

// ListIncludingDeleted returns every row — soft-deleted included. Powers
// MCP "what's in the trash" queries and the hard-purge tool's pre-flight
// inventory. The Deleted flag on the returned service.Task tells the
// caller which is which.
func (s *Store) ListIncludingDeleted(ctx context.Context) ([]taskstore.Task, error) {
	return s.listRows(ctx, "SELECT "+taskColumns+" FROM tasks")
}

func (s *Store) listRows(ctx context.Context, query string) ([]taskstore.Task, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	var tasks []taskstore.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func (s *Store) Get(ctx context.Context, taskID string) (*taskstore.Task, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+taskColumns+" FROM tasks WHERE task_id = ? AND is_deleted = 'N'",
		taskID)
	t, err := scanTaskRow(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, taskstore.ErrNotFound
		}
		return nil, fmt.Errorf("get task %s: %w", taskID, err)
	}
	return &t, nil
}

func (s *Store) Create(ctx context.Context, t *taskstore.Task) error {
	now := time.Now().UnixMilli()
	if t.TaskID == "" {
		t.TaskID = taskstore.GenerateTaskID(taskstore.NullStr(t.Title), now)
	}
	if !t.CompletedTime.Valid {
		t.CompletedTime = sql.NullInt64{Int64: now, Valid: true}
	}
	if !t.LastModified.Valid {
		t.LastModified = sql.NullInt64{Int64: now, Valid: true}
	}
	if t.IsDeleted == "" {
		t.IsDeleted = "N"
	}
	if t.IsReminderOn == "" {
		t.IsReminderOn = "N"
	}
	if !t.Status.Valid {
		t.Status = sql.NullString{String: "needsAction", Valid: true}
	}

	_, err := s.db.ExecContext(ctx, `INSERT INTO tasks
		(task_id, title, detail, status, importance, due_time,
		 completed_time, last_modified, recurrence, is_reminder_on,
		 links, is_deleted, ical_blob, created_at, updated_at,
		 forestnote_notebook_id, forestnote_page_id, forestnote_notebook_name, forestnote_source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.TaskID, t.Title, t.Detail, t.Status, t.Importance, t.DueTime,
		t.CompletedTime, t.LastModified, t.Recurrence, t.IsReminderOn,
		t.Links, t.IsDeleted, t.ICalBlob, now, now,
		t.ForestNoteNotebookID, t.ForestNotePageID, t.ForestNoteNotebookName, t.ForestNoteSource)
	if err != nil {
		return fmt.Errorf("create task: %w", err)
	}
	return nil
}

func (s *Store) Update(ctx context.Context, t *taskstore.Task) error {
	now := time.Now().UnixMilli()
	t.LastModified = sql.NullInt64{Int64: now, Valid: true}

	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET
		title = ?, detail = ?, status = ?, importance = ?, due_time = ?,
		completed_time = ?, last_modified = ?, recurrence = ?,
		is_reminder_on = ?, links = ?, ical_blob = ?, updated_at = ?,
		forestnote_notebook_id = ?, forestnote_page_id = ?,
		forestnote_notebook_name = ?, forestnote_source = ?
		WHERE task_id = ?`,
		t.Title, t.Detail, t.Status, t.Importance, t.DueTime,
		t.CompletedTime, t.LastModified, t.Recurrence,
		t.IsReminderOn, t.Links, t.ICalBlob, now,
		t.ForestNoteNotebookID, t.ForestNotePageID,
		t.ForestNoteNotebookName, t.ForestNoteSource,
		t.TaskID)
	if err != nil {
		return fmt.Errorf("update task %s: %w", t.TaskID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update task %s rows affected: %w", t.TaskID, err)
	}
	if affected == 0 {
		return taskstore.ErrNotFound
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, taskID string) error {
	now := time.Now().UnixMilli()
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET
		is_deleted = 'Y', last_modified = ?, updated_at = ?
		WHERE task_id = ?`,
		now, now, taskID)
	if err != nil {
		return fmt.Errorf("delete task %s: %w", taskID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete task %s rows affected: %w", taskID, err)
	}
	if affected == 0 {
		return taskstore.ErrNotFound
	}
	return nil
}

// DeleteCompleted soft-deletes all completed tasks. Returns the number of tasks deleted.
func (s *Store) DeleteCompleted(ctx context.Context) (int64, error) {
	now := time.Now().UnixMilli()
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET
		is_deleted = 'Y', last_modified = ?, updated_at = ?
		WHERE is_deleted = 'N' AND status = 'completed'`,
		now, now)
	if err != nil {
		return 0, fmt.Errorf("delete completed tasks: %w", err)
	}
	return result.RowsAffected()
}

// HardDeleteOlderThan permanently removes soft-deleted rows whose
// last_modified is older than cutoffMs. This is the *only* path that does
// real DELETE FROM tasks — everything else in the system soft-deletes. The
// caller picks the cutoff; the service layer translates a user-facing
// "older_than_days" into a millisecond instant before calling here.
//
// Doesn't VACUUM — that's an explicit maintenance operation and we don't
// want to hold a write lock for it inside the request path. Space gets
// reclaimed by SQLite incrementally; the freed rows are gone immediately.
// HardDeleteOlderThan permanently removes soft-deleted rows older than the
// cutoff and returns (purged, skipped, error). Skipped is the count of rows
// that were eligible by virtue of being soft-deleted but were too recent to
// purge (inside the safety window) — surfaced so callers can distinguish
// "the gate works and there was nothing to do" from "the gate is broken
// and silently skipped everything." Pre-counted in the same transaction
// scope as the DELETE to avoid race-window drift between the two queries.
func (s *Store) HardDeleteOlderThan(ctx context.Context, cutoffMs int64) (purged, skipped int64, err error) {
	if err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks WHERE is_deleted = 'Y' AND last_modified >= ?`,
		cutoffMs).Scan(&skipped); err != nil {
		return 0, 0, fmt.Errorf("count skipped tasks at cutoff %d: %w", cutoffMs, err)
	}
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM tasks WHERE is_deleted = 'Y' AND last_modified < ?`,
		cutoffMs)
	if err != nil {
		return 0, 0, fmt.Errorf("hard delete tasks older than %d: %w", cutoffMs, err)
	}
	purged, err = result.RowsAffected()
	if err != nil {
		return 0, skipped, fmt.Errorf("rows affected: %w", err)
	}
	return purged, skipped, nil
}

// IsEmpty returns true if the task store has no tasks (including deleted ones).
// Used to detect first-run state for migration.
func (s *Store) IsEmpty(ctx context.Context) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM tasks LIMIT 1)").Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check tasks empty: %w", err)
	}
	return exists == 0, nil
}

func (s *Store) MaxLastModified(ctx context.Context) (int64, error) {
	var max sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		"SELECT MAX(last_modified) FROM tasks WHERE is_deleted = 'N'").Scan(&max)
	if err != nil {
		return 0, fmt.Errorf("max last_modified: %w", err)
	}
	if !max.Valid {
		return 0, nil
	}
	return max.Int64, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanTask(s scanner) (taskstore.Task, error) {
	var t taskstore.Task
	err := s.Scan(
		&t.TaskID, &t.Title, &t.Detail, &t.Status, &t.Importance,
		&t.DueTime, &t.CompletedTime, &t.LastModified, &t.Recurrence,
		&t.IsReminderOn, &t.Links, &t.IsDeleted, &t.ICalBlob,
		&t.CreatedAt,
		&t.ForestNoteNotebookID, &t.ForestNotePageID,
		&t.ForestNoteNotebookName, &t.ForestNoteSource,
	)
	return t, err
}

func scanTaskRow(row *sql.Row) (taskstore.Task, error) {
	return scanTask(row)
}
