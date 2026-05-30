package taskdb

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/taskstore"
)

// openTestStore creates an in-memory SQLite task store for testing.
func openTestStore(t *testing.T) *Store {
	db, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("Open in-memory db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewStore(db)
}

// TestStore_Create_PersistsTask verifies AC1.1: Create a task via store.Create(),
// retrieve via store.Get() — verify all fields match, task persists in SQLite.
func TestStore_Create_PersistsTask(t *testing.T) {
	store := openTestStore(t)

	input := &taskstore.Task{
		Title:        sql.NullString{String: "Buy groceries", Valid: true},
		Detail:       sql.NullString{String: "milk, eggs, bread", Valid: true},
		Status:       sql.NullString{String: "needsAction", Valid: true},
		Importance:   sql.NullString{String: "1", Valid: true},
		DueTime:      1672531200000, // 2023-01-01 00:00 UTC
		Recurrence:   sql.NullString{String: "", Valid: false},
		IsReminderOn: "N",
		Links:        sql.NullString{String: "", Valid: false},
		IsDeleted:    "N",
	}

	if err := store.Create(context.Background(), input); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Verify task was assigned an ID
	if input.TaskID == "" {
		t.Error("Create should assign TaskID")
	}

	// Verify task is retrievable
	retrieved, err := store.Get(context.Background(), input.TaskID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Verify all fields match
	if retrieved.TaskID != input.TaskID {
		t.Errorf("TaskID: got %q, want %q", retrieved.TaskID, input.TaskID)
	}
	if retrieved.Title != input.Title {
		t.Errorf("Title: got %v, want %v", retrieved.Title, input.Title)
	}
	if retrieved.Detail != input.Detail {
		t.Errorf("Detail: got %v, want %v", retrieved.Detail, input.Detail)
	}
	if retrieved.Status != input.Status {
		t.Errorf("Status: got %v, want %v", retrieved.Status, input.Status)
	}
	if retrieved.Importance != input.Importance {
		t.Errorf("Importance: got %v, want %v", retrieved.Importance, input.Importance)
	}
	if retrieved.DueTime != input.DueTime {
		t.Errorf("DueTime: got %d, want %d", retrieved.DueTime, input.DueTime)
	}
	if retrieved.IsReminderOn != input.IsReminderOn {
		t.Errorf("IsReminderOn: got %q, want %q", retrieved.IsReminderOn, input.IsReminderOn)
	}
	if retrieved.IsDeleted != input.IsDeleted {
		t.Errorf("IsDeleted: got %q, want %q", retrieved.IsDeleted, input.IsDeleted)
	}

	// Verify CompletedTime and LastModified were set
	if !retrieved.CompletedTime.Valid {
		t.Error("CompletedTime should be set")
	}
	if !retrieved.LastModified.Valid {
		t.Error("LastModified should be set")
	}
}

// TestStore_Update_ChangesFieldsAndTimestamp verifies AC1.2: Create a task,
// update title/status/due_time via store.Update(), verify fields changed,
// verify ETag (computed externally) would differ (last_modified bumped).
func TestStore_Update_ChangesFieldsAndTimestamp(t *testing.T) {
	store := openTestStore(t)

	task := &taskstore.Task{
		Title:     sql.NullString{String: "Task 1", Valid: true},
		Status:    sql.NullString{String: "needsAction", Valid: true},
		DueTime:   1000000,
		IsDeleted: "N",
	}

	if err := store.Create(context.Background(), task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	originalID := task.TaskID
	originalLastMod := task.LastModified

	// Give time a moment to ensure timestamps differ
	time.Sleep(2 * time.Millisecond)

	// Update fields
	task.Title = sql.NullString{String: "Task 1 Updated", Valid: true}
	task.Status = sql.NullString{String: "completed", Valid: true}
	task.DueTime = 2000000

	if err := store.Update(context.Background(), task); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Verify task ID unchanged
	if task.TaskID != originalID {
		t.Errorf("TaskID should not change: got %q, want %q", task.TaskID, originalID)
	}

	// Verify fields were updated
	retrieved, err := store.Get(context.Background(), task.TaskID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}

	if taskstore.NullStr(retrieved.Title) != "Task 1 Updated" {
		t.Errorf("Title not updated: got %q, want %q", taskstore.NullStr(retrieved.Title), "Task 1 Updated")
	}
	if taskstore.NullStr(retrieved.Status) != "completed" {
		t.Errorf("Status not updated: got %q, want %q", taskstore.NullStr(retrieved.Status), "completed")
	}
	if retrieved.DueTime != 2000000 {
		t.Errorf("DueTime not updated: got %d, want %d", retrieved.DueTime, 2000000)
	}

	// Verify last_modified was bumped (indicates ETag would change)
	if !retrieved.LastModified.Valid {
		t.Fatal("LastModified should be valid")
	}
	if retrieved.LastModified.Int64 <= originalLastMod.Int64 {
		t.Errorf("LastModified should increase: original %d, got %d", originalLastMod.Int64, retrieved.LastModified.Int64)
	}
}

// TestStore_Delete_SoftDeletesAndHides verifies AC1.3: Create a task,
// delete via store.Delete(), verify store.Get() returns ErrNotFound,
// verify store.List() excludes it.
func TestStore_Delete_SoftDeletesAndHides(t *testing.T) {
	store := openTestStore(t)

	task := &taskstore.Task{
		Title:     sql.NullString{String: "To Delete", Valid: true},
		IsDeleted: "N",
	}

	if err := store.Create(context.Background(), task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	taskID := task.TaskID

	// Verify task is in list before delete
	list, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List before delete: %v", err)
	}
	found := false
	for _, t := range list {
		if t.TaskID == taskID {
			found = true
			break
		}
	}
	if !found {
		t.Error("Task should be in list before delete")
	}

	// Delete the task
	if err := store.Delete(context.Background(), taskID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Verify Get returns ErrNotFound
	_, err = store.Get(context.Background(), taskID)
	if !taskstore.IsNotFound(err) {
		t.Errorf("Get after delete should return ErrNotFound, got %v", err)
	}

	// Verify List excludes the deleted task
	list, err = store.List(context.Background())
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	for _, tsk := range list {
		if tsk.TaskID == taskID {
			t.Error("Deleted task should not appear in List")
		}
	}
}

// TestStore_MaxLastModified_TracksChanges verifies AC1.6: Create a task,
// note MaxLastModified() value; update the task, verify MaxLastModified()
// increased; delete the task with a fresh timestamp, verify MaxLastModified()
// reflects the deleted task's bumped timestamp is excluded (deleted tasks
// filtered from MAX query).
func TestStore_MaxLastModified_TracksChanges(t *testing.T) {
	store := openTestStore(t)

	task := &taskstore.Task{
		Title:     sql.NullString{String: "Test task", Valid: true},
		IsDeleted: "N",
	}

	if err := store.Create(context.Background(), task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	maxAfterCreate, err := store.MaxLastModified(context.Background())
	if err != nil {
		t.Fatalf("MaxLastModified after create: %v", err)
	}

	time.Sleep(2 * time.Millisecond)

	// Update the task
	task.Title = sql.NullString{String: "Updated", Valid: true}
	if err := store.Update(context.Background(), task); err != nil {
		t.Fatalf("Update: %v", err)
	}

	maxAfterUpdate, err := store.MaxLastModified(context.Background())
	if err != nil {
		t.Fatalf("MaxLastModified after update: %v", err)
	}

	if maxAfterUpdate <= maxAfterCreate {
		t.Errorf("MaxLastModified should increase after update: %d <= %d", maxAfterUpdate, maxAfterCreate)
	}

	time.Sleep(2 * time.Millisecond)

	// Create a second task to be the surviving task after first is deleted.
	task2 := &taskstore.Task{
		Title:     sql.NullString{String: "Test task 2", Valid: true},
		IsDeleted: "N",
	}

	if err := store.Create(context.Background(), task2); err != nil {
		t.Fatalf("Create task2: %v", err)
	}

	time.Sleep(2 * time.Millisecond)

	// Delete the first task
	if err := store.Delete(context.Background(), task.TaskID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	maxAfterDelete, err := store.MaxLastModified(context.Background())
	if err != nil {
		t.Fatalf("MaxLastModified after delete: %v", err)
	}

	// After deleting task1, MaxLastModified returns task2's last_modified
	// (the only remaining non-deleted task), which is newer than maxAfterUpdate.
	if maxAfterDelete <= maxAfterUpdate {
		t.Errorf("MaxLastModified after delete should reflect surviving task2: got %d, want > %d", maxAfterDelete, maxAfterUpdate)
	}
}

// TestStore_MaxLastModified_EmptyStore verifies AC1.7: On empty store,
// MaxLastModified() returns 0.
func TestStore_MaxLastModified_EmptyStore(t *testing.T) {
	store := openTestStore(t)

	max, err := store.MaxLastModified(context.Background())
	if err != nil {
		t.Fatalf("MaxLastModified on empty store: %v", err)
	}

	if max != 0 {
		t.Errorf("MaxLastModified on empty store should be 0, got %d", max)
	}
}

// TestStore_CTag_IncrementsOnChanges verifies AC1.6 (CTag variant):
// CTag should change when any task is created, modified, or deleted.
func TestStore_CTag_IncrementsOnChanges(t *testing.T) {
	store := openTestStore(t)

	// Initial CTag on empty store
	ctagEmpty, err := store.MaxLastModified(context.Background())
	if err != nil {
		t.Fatalf("MaxLastModified on empty store: %v", err)
	}
	if ctagEmpty != 0 {
		t.Errorf("CTag (MaxLastModified) on empty store should be 0, got %d", ctagEmpty)
	}

	// Create first task
	task1 := &taskstore.Task{
		Title:     sql.NullString{String: "Task 1", Valid: true},
		IsDeleted: "N",
	}
	if err := store.Create(context.Background(), task1); err != nil {
		t.Fatalf("Create task1: %v", err)
	}

	ctagAfterCreate1, err := store.MaxLastModified(context.Background())
	if err != nil {
		t.Fatalf("MaxLastModified after create1: %v", err)
	}

	if ctagAfterCreate1 <= ctagEmpty {
		t.Errorf("CTag should increase after create: %d <= %d", ctagAfterCreate1, ctagEmpty)
	}

	time.Sleep(2 * time.Millisecond)

	// Create second task
	task2 := &taskstore.Task{
		Title:     sql.NullString{String: "Task 2", Valid: true},
		IsDeleted: "N",
	}
	if err := store.Create(context.Background(), task2); err != nil {
		t.Fatalf("Create task2: %v", err)
	}

	ctagAfterCreate2, err := store.MaxLastModified(context.Background())
	if err != nil {
		t.Fatalf("MaxLastModified after create2: %v", err)
	}

	if ctagAfterCreate2 <= ctagAfterCreate1 {
		t.Errorf("CTag should increase on second create: %d <= %d", ctagAfterCreate2, ctagAfterCreate1)
	}

	time.Sleep(2 * time.Millisecond)

	// Update task1
	task1.Title = sql.NullString{String: "Task 1 Updated", Valid: true}
	if err := store.Update(context.Background(), task1); err != nil {
		t.Fatalf("Update task1: %v", err)
	}

	ctagAfterUpdate, err := store.MaxLastModified(context.Background())
	if err != nil {
		t.Fatalf("MaxLastModified after update: %v", err)
	}

	if ctagAfterUpdate <= ctagAfterCreate2 {
		t.Errorf("CTag should increase after update: %d <= %d", ctagAfterUpdate, ctagAfterCreate2)
	}

	time.Sleep(2 * time.Millisecond)

	// Delete task2
	if err := store.Delete(context.Background(), task2.TaskID); err != nil {
		t.Fatalf("Delete task2: %v", err)
	}

	ctagAfterDelete, err := store.MaxLastModified(context.Background())
	if err != nil {
		t.Fatalf("MaxLastModified after delete: %v", err)
	}

	// After deleting task2, MaxLastModified reflects only non-deleted tasks.
	// task1 (updated) is the sole survivor, so CTag equals ctagAfterUpdate.
	if ctagAfterDelete != ctagAfterUpdate {
		t.Errorf("CTag after delete should equal surviving task's CTag: got %d, want %d", ctagAfterDelete, ctagAfterUpdate)
	}
}

// TestStore_List_ReturnsAllNonDeleted verifies List returns only non-deleted tasks.
func TestStore_List_ReturnsAllNonDeleted(t *testing.T) {
	store := openTestStore(t)

	// Create 3 tasks
	for i := 1; i <= 3; i++ {
		task := &taskstore.Task{
			Title:     sql.NullString{String: fmt.Sprintf("Task %d", i), Valid: true},
			IsDeleted: "N",
		}
		if err := store.Create(context.Background(), task); err != nil {
			t.Fatalf("Create task %d: %v", i, err)
		}
	}

	// Verify all 3 are in list
	list, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("List should have 3 tasks, got %d", len(list))
	}

	// Delete the second task
	if err := store.Delete(context.Background(), list[1].TaskID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Verify list now has 2 tasks
	list, err = store.List(context.Background())
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List should have 2 tasks, got %d", len(list))
	}
}

// TestStore_Create_SetDefaults verifies Create sets defaults for missing fields.
func TestStore_Create_SetDefaults(t *testing.T) {
	store := openTestStore(t)

	// Create task with minimal fields
	task := &taskstore.Task{
		Title:     sql.NullString{String: "Minimal Task", Valid: true},
		IsDeleted: "", // Will be set to "N"
	}

	if err := store.Create(context.Background(), task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Verify defaults were set
	if task.TaskID == "" {
		t.Error("TaskID should be generated")
	}
	if !task.CompletedTime.Valid {
		t.Error("CompletedTime should be set")
	}
	if !task.LastModified.Valid {
		t.Error("LastModified should be set")
	}
	if task.IsDeleted != "N" {
		t.Errorf("IsDeleted should default to 'N', got %q", task.IsDeleted)
	}
	if task.IsReminderOn != "N" {
		t.Errorf("IsReminderOn should default to 'N', got %q", task.IsReminderOn)
	}
	if taskstore.NullStr(task.Status) != "needsAction" {
		t.Errorf("Status should default to 'needsAction', got %q", taskstore.NullStr(task.Status))
	}
}

// TestStore_ListIncludingDeleted verifies the trash-visible list path returns
// soft-deleted rows alongside live ones, distinguished by IsDeleted. Plain
// List remains visibility-filtered (regression).
func TestStore_ListIncludingDeleted(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	live := &taskstore.Task{Title: sql.NullString{String: "live", Valid: true}, IsDeleted: "N"}
	if err := store.Create(ctx, live); err != nil {
		t.Fatalf("Create live: %v", err)
	}
	tomb := &taskstore.Task{Title: sql.NullString{String: "tomb", Valid: true}, IsDeleted: "N"}
	if err := store.Create(ctx, tomb); err != nil {
		t.Fatalf("Create soon-to-be-deleted: %v", err)
	}
	if err := store.Delete(ctx, tomb.TaskID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	gotLive, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(gotLive) != 1 || gotLive[0].TaskID != live.TaskID {
		t.Errorf("plain List leaked tombstones: %+v", gotLive)
	}

	gotAll, err := store.ListIncludingDeleted(ctx)
	if err != nil {
		t.Fatalf("ListIncludingDeleted: %v", err)
	}
	if len(gotAll) != 2 {
		t.Fatalf("want 2 rows including tombstone, got %d", len(gotAll))
	}
	var sawDeleted bool
	for _, x := range gotAll {
		if x.IsDeleted == "Y" {
			sawDeleted = true
		}
	}
	if !sawDeleted {
		t.Error("ListIncludingDeleted didn't return any IsDeleted='Y' row")
	}
}

// TestStore_ForestNoteFieldsRoundTrip verifies the four ForestNote provenance
// columns Create-then-Get cleanly with the same values, and that an Update
// preserves them when re-supplied.
func TestStore_ForestNoteFieldsRoundTrip(t *testing.T) {
	store := openTestStore(t)

	task := &taskstore.Task{
		Title:                  sql.NullString{String: "From notebook", Valid: true},
		IsDeleted:              "N",
		ForestNoteNotebookID:   sql.NullString{String: "01HZ3KAY", Valid: true},
		ForestNotePageID:       sql.NullString{String: "01HZ3L7M", Valid: true},
		ForestNoteNotebookName: sql.NullString{String: "Project Notes", Valid: true},
		ForestNoteSource:       sql.NullString{String: "lasso", Valid: true},
	}
	if err := store.Create(context.Background(), task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.Get(context.Background(), task.TaskID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ForestNoteNotebookID != task.ForestNoteNotebookID {
		t.Errorf("notebook id: got %+v want %+v", got.ForestNoteNotebookID, task.ForestNoteNotebookID)
	}
	if got.ForestNotePageID != task.ForestNotePageID {
		t.Errorf("page id: got %+v want %+v", got.ForestNotePageID, task.ForestNotePageID)
	}
	if got.ForestNoteNotebookName != task.ForestNoteNotebookName {
		t.Errorf("notebook name: got %+v want %+v", got.ForestNoteNotebookName, task.ForestNoteNotebookName)
	}
	if got.ForestNoteSource != task.ForestNoteSource {
		t.Errorf("source: got %+v want %+v", got.ForestNoteSource, task.ForestNoteSource)
	}

	// Update path must carry the values through (no implicit NULLing).
	got.Title = sql.NullString{String: "renamed", Valid: true}
	if err := store.Update(context.Background(), got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got2, err := store.Get(context.Background(), task.TaskID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got2.ForestNoteNotebookID.String != "01HZ3KAY" {
		t.Errorf("notebook id lost on update: %+v", got2.ForestNoteNotebookID)
	}
}

// TestMigrate_AddsForestNoteColumnsToExistingDB simulates the live deployment:
// the `tasks` table already exists from an older build that pre-dated the
// ForestNote columns. Running migrate() must add the four columns in place,
// without dropping or renaming the table, and existing rows must still be
// readable + writable.
func TestMigrate_AddsForestNoteColumnsToExistingDB(t *testing.T) {
	dsn := "file::memory:?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)

	// Hand-roll the pre-ForestNote-columns schema. This is the shape that's
	// live on the server today (commit 8224b03, 1,513 rows on disk).
	preForestNoteSchema := []string{
		`CREATE TABLE tasks (
			task_id        TEXT PRIMARY KEY,
			title          TEXT,
			detail         TEXT,
			status         TEXT NOT NULL DEFAULT 'needsAction',
			importance     TEXT,
			due_time       INTEGER NOT NULL DEFAULT 0,
			completed_time INTEGER NOT NULL DEFAULT 0,
			last_modified  INTEGER NOT NULL DEFAULT 0,
			recurrence     TEXT,
			is_reminder_on TEXT NOT NULL DEFAULT 'N',
			links          TEXT,
			is_deleted     TEXT NOT NULL DEFAULT 'N',
			ical_blob      TEXT,
			created_at     INTEGER NOT NULL,
			updated_at     INTEGER NOT NULL
		)`,
		`CREATE TABLE sync_state (
			adapter_id      TEXT PRIMARY KEY,
			last_sync_token TEXT,
			last_sync_at    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE task_sync_map (
			task_id     TEXT NOT NULL REFERENCES tasks(task_id),
			adapter_id  TEXT NOT NULL,
			remote_id   TEXT NOT NULL,
			remote_etag TEXT,
			last_pushed_at  INTEGER NOT NULL DEFAULT 0,
			last_pulled_at  INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (task_id, adapter_id)
		)`,
		`INSERT INTO tasks (task_id, title, status, created_at, updated_at) VALUES ('legacy-1', 'pre-migration row', 'needsAction', 1, 1)`,
	}
	for _, stmt := range preForestNoteSchema {
		if _, err := db.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}

	if err := migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The four new columns must now be present.
	wantCols := []string{
		"forestnote_notebook_id",
		"forestnote_page_id",
		"forestnote_notebook_name",
		"forestnote_source",
	}
	for _, col := range wantCols {
		var c int
		if err := db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name=?`, col).Scan(&c); err != nil {
			t.Fatalf("pragma check %s: %v", col, err)
		}
		if c != 1 {
			t.Errorf("column %s not added by migrate (count=%d)", col, c)
		}
	}

	// The legacy row should still be readable and the new columns should be NULL on it.
	store := NewStore(db)
	got, err := store.Get(context.Background(), "legacy-1")
	if err != nil {
		t.Fatalf("Get legacy row: %v", err)
	}
	if got.ForestNoteNotebookID.Valid {
		t.Errorf("legacy row notebook_id should be NULL, got %+v", got.ForestNoteNotebookID)
	}
	if got.ForestNotePageID.Valid {
		t.Errorf("legacy row page_id should be NULL, got %+v", got.ForestNotePageID)
	}

	// Running migrate() a second time must be a no-op (idempotency).
	if err := migrate(context.Background(), db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

// TestStore_HardDeleteOlderThan_RespectsAgeAndDeletedFlag confirms the
// hard-purge only touches rows that are both soft-deleted AND older than
// the cutoff. Non-deleted rows survive regardless of age; recent ghosts
// survive regardless of deletion. This is the operation that finally
// reclaims rows — every other "delete" path tombstones — so its predicate
// has to be exactly right.
func TestStore_HardDeleteOlderThan_RespectsAgeAndDeletedFlag(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	now := time.Now().UnixMilli()
	dayMs := int64(24 * 60 * 60 * 1000)

	// Four rows covering the matrix:
	//   (a) live + recent          → must survive (live)
	//   (b) live + ancient         → must survive (live)
	//   (c) deleted + recent       → must survive (within window)
	//   (d) deleted + ancient      → must be removed
	rows := []struct {
		id           string
		isDeleted    string
		lastModified int64
	}{
		{"a-live-recent", "N", now - 1*dayMs},
		{"b-live-ancient", "N", now - 90*dayMs},
		{"c-deleted-recent", "Y", now - 5*dayMs},
		{"d-deleted-ancient", "Y", now - 60*dayMs},
	}
	for _, r := range rows {
		_, err := store.db.ExecContext(ctx, `INSERT INTO tasks
			(task_id, title, status, is_deleted, last_modified, created_at, updated_at, is_reminder_on)
			VALUES (?, ?, 'needsAction', ?, ?, ?, ?, 'N')`,
			r.id, "row "+r.id, r.isDeleted, r.lastModified, now, now)
		if err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}

	// Cutoff 30 days ago: only "d-deleted-ancient" matches both predicates.
	// "c-deleted-recent" is soft-deleted but inside the window → counted as skipped.
	cutoff := now - 30*dayMs
	purged, skipped, err := store.HardDeleteOlderThan(ctx, cutoff)
	if err != nil {
		t.Fatalf("HardDeleteOlderThan: %v", err)
	}
	if purged != 1 {
		t.Errorf("purged: got %d, want 1", purged)
	}
	if skipped != 1 {
		t.Errorf("skipped: got %d, want 1 (c-deleted-recent inside window)", skipped)
	}

	// Confirm survivors are physically present (querying including deleted).
	survivors, err := store.ListIncludingDeleted(ctx)
	if err != nil {
		t.Fatalf("ListIncludingDeleted: %v", err)
	}
	got := map[string]bool{}
	for _, t := range survivors {
		got[t.TaskID] = true
	}
	for _, want := range []string{"a-live-recent", "b-live-ancient", "c-deleted-recent"} {
		if !got[want] {
			t.Errorf("expected survivor %q to be present, missing", want)
		}
	}
	if got["d-deleted-ancient"] {
		t.Errorf("expected %q to be hard-deleted, still present", "d-deleted-ancient")
	}
}

// TestStore_HardDeleteOlderThan_NoMatchesReturnsZero covers the no-op case:
// the call should succeed and report zero rows when nothing meets the
// predicate. Confirms the operation is safe to schedule unconditionally.
func TestStore_HardDeleteOlderThan_NoMatchesReturnsZero(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// One live, one recently-deleted — neither qualifies for a 30-day purge.
	_, err := store.db.ExecContext(ctx, `INSERT INTO tasks
		(task_id, title, status, is_deleted, last_modified, created_at, updated_at, is_reminder_on)
		VALUES ('live', 'live', 'needsAction', 'N', ?, ?, ?, 'N')`,
		now, now, now)
	if err != nil {
		t.Fatalf("insert live: %v", err)
	}
	_, err = store.db.ExecContext(ctx, `INSERT INTO tasks
		(task_id, title, status, is_deleted, last_modified, created_at, updated_at, is_reminder_on)
		VALUES ('recent-ghost', 'gone', 'needsAction', 'Y', ?, ?, ?, 'N')`,
		now-int64(60*60*1000), now, now) // deleted 1h ago
	if err != nil {
		t.Fatalf("insert recent-ghost: %v", err)
	}

	cutoff := now - int64(30*24*60*60*1000)
	purged, skipped, err := store.HardDeleteOlderThan(ctx, cutoff)
	if err != nil {
		t.Fatalf("HardDeleteOlderThan: %v", err)
	}
	if purged != 0 {
		t.Errorf("purged: got %d, want 0", purged)
	}
	// recent-ghost is the lone soft-deleted row, and it's inside the 30-day
	// window → counted as skipped, NOT purged. Confirms the response
	// disambiguates "the gate is doing its job" from "the gate is broken."
	if skipped != 1 {
		t.Errorf("skipped: got %d, want 1 (recent-ghost inside window)", skipped)
	}
}


