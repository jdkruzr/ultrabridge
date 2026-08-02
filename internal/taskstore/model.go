package taskstore

import "database/sql"

// Task represents a row in t_schedule_task.
// Note: The DB table has 8 additional sort columns (sort, sort_completed,
// planer_sort, all_sort, all_sort_completed, sort_time, planer_sort_time,
// all_sort_time) that are NOT included here. Tasks created via CalDAV will
// have NULL for these columns. This is acceptable because:
// 1. The Supernote device populates sort columns when it syncs
// 2. All sort columns are NULLable with no NOT NULL constraints
// 3. Observed behavior: the device reassigns sort order on sync
// If device behavior differs, the Create method can set default sort values.
//
// The trailing ForestNote* fields are taskdb-only (UB's `tasks` table) and are
// always NULL in the SPC-side `t_schedule_task` table. They mirror the
// X-FORESTNOTE-* extension properties on the inbound VTODO so MCP / REST can
// answer queries like "tasks from notebook X" without re-parsing ical_blob on
// every read. The SPC mapping layer (internal/spcserver/mapping) ignores them.
type Task struct {
	TaskID        string
	TaskListID    sql.NullString
	UserID        int64
	Title         sql.NullString
	Detail        sql.NullString
	LastModified  sql.NullInt64
	Recurrence    sql.NullString
	IsReminderOn  string
	Status        sql.NullString
	Importance    sql.NullString
	DueTime       int64
	CompletedTime sql.NullInt64
	Links         sql.NullString
	IsDeleted     string
	ICalBlob      sql.NullString
	// CreatedAt mirrors the taskdb `tasks.created_at` column (ms UTC). Taskdb-only:
	// SPC's t_schedule_task has no created_at; mapping leaves it zero. Surfaced in
	// the REST API as `created_at` (was previously mis-mapped from DueTime).
	CreatedAt int64
	// UpdatedAt mirrors `tasks.updated_at` (ms UTC) — the store-owned modification
	// watermark, stamped by Create and every Update. It is the input to ComputeETag
	// and the SPC sync token. Taskdb-only; the SPC wire has no equivalent (the
	// device's `lastModified` is derived from this at the boundary).
	UpdatedAt int64
	// CompletedAt is the real completion instant (ms UTC), NULL unless the task is
	// completed. Caller-owned: the store persists it verbatim and never rewrites it.
	//
	// This is the native replacement for the Supernote convention where
	// `lastModified` doubles as the completion time (see docs/PRIVATE_CLOUD_REFERENCE.md
	// §Task Field Reference). That convention only holds because the device never
	// edits a task after completing it; UB does, so completion needs its own column.
	// The device-shaped value is reconstructed in internal/spcserver/mapping.
	CompletedAt sql.NullInt64
	// ForestNote provenance (taskdb-only; SPC ignores).
	ForestNoteNotebookID   sql.NullString
	ForestNotePageID       sql.NullString
	ForestNoteNotebookName sql.NullString
	ForestNoteSource       sql.NullString
}
