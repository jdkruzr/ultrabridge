package taskstore

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newSQLStore(t *testing.T, userID int64) (*Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE t_schedule_task (
		task_id TEXT PRIMARY KEY,
		task_list_id TEXT,
		user_id INTEGER NOT NULL,
		title TEXT,
		detail TEXT,
		last_modified INTEGER,
		recurrence TEXT,
		is_reminder_on TEXT,
		status TEXT,
		importance TEXT,
		due_time INTEGER NOT NULL DEFAULT 0,
		completed_time INTEGER,
		links TEXT,
		is_deleted TEXT NOT NULL DEFAULT 'N'
	)`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	return New(db, userID), db
}

func TestStoreCreateDefaultsAndScopesUser(t *testing.T) {
	store, _ := newSQLStore(t, 42)
	ctx := context.Background()
	task := &Task{Title: SqlStr("Write tests")}

	if err := store.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if task.TaskID == "" {
		t.Fatal("Create should generate task id")
	}
	if task.UserID != 42 {
		t.Fatalf("UserID = %d, want 42", task.UserID)
	}
	if task.IsDeleted != "N" || task.IsReminderOn != "N" {
		t.Fatalf("defaults not applied: deleted=%q reminder=%q", task.IsDeleted, task.IsReminderOn)
	}
	if NullStr(task.Status) != "needsAction" {
		t.Fatalf("status default = %q, want needsAction", NullStr(task.Status))
	}
	if !task.LastModified.Valid || !task.CompletedTime.Valid {
		t.Fatalf("timestamps should be stamped: last=%v completed=%v", task.LastModified, task.CompletedTime)
	}

	got, err := store.Get(ctx, task.TaskID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.UserID != 42 || NullStr(got.Title) != "Write tests" {
		t.Fatalf("unexpected fetched task: %+v", got)
	}
	if _, err := New(store.db, 7).Get(ctx, task.TaskID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user Get: got %v, want ErrNotFound", err)
	}
}

func TestStoreListFiltersDeletedAndUser(t *testing.T) {
	store, db := newSQLStore(t, 42)
	ctx := context.Background()
	rows := []struct {
		id      string
		userID  int64
		deleted string
	}{
		{"live", 42, "N"},
		{"deleted", 42, "Y"},
		{"other-user", 7, "N"},
	}
	for _, r := range rows {
		_, err := db.Exec(`INSERT INTO t_schedule_task
			(task_id, user_id, title, last_modified, is_reminder_on, status, due_time, completed_time, is_deleted)
			VALUES (?, ?, ?, ?, 'N', 'needsAction', 0, 100, ?)`,
			r.id, r.userID, r.id, int64(100), r.deleted)
		if err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}

	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].TaskID != "live" {
		t.Fatalf("List returned %+v, want only live task", got)
	}
}

func TestStoreUpdateBumpsTimestampAndDoesNotCrossUsers(t *testing.T) {
	store, _ := newSQLStore(t, 42)
	ctx := context.Background()
	task := &Task{TaskID: "t1", Title: SqlStr("old"), Status: SqlStr("needsAction")}
	if err := store.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}
	original := task.LastModified.Int64
	time.Sleep(2 * time.Millisecond)

	task.Title = SqlStr("new")
	task.Status = SqlStr("completed")
	task.DueTime = 1234
	task.Recurrence = SqlStr("FREQ=DAILY")
	if err := store.Update(ctx, task); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := store.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if NullStr(got.Title) != "new" || NullStr(got.Status) != "completed" || got.DueTime != 1234 || NullStr(got.Recurrence) != "FREQ=DAILY" {
		t.Fatalf("update fields not applied: %+v", got)
	}
	if got.LastModified.Int64 <= original {
		t.Fatalf("LastModified = %d, want > %d", got.LastModified.Int64, original)
	}

	other := New(store.db, 7)
	task.Title = SqlStr("wrong user")
	if err := other.Update(ctx, task); err != nil {
		t.Fatalf("cross-user Update should be a no-op, got error: %v", err)
	}
	got, _ = store.Get(ctx, "t1")
	if NullStr(got.Title) == "wrong user" {
		t.Fatal("cross-user Update changed the task")
	}
}

func TestStoreDeleteCompletedDeleteAndMaxLastModified(t *testing.T) {
	store, _ := newSQLStore(t, 42)
	ctx := context.Background()
	open := &Task{TaskID: "open", Title: SqlStr("open"), Status: SqlStr("needsAction"), LastModified: sql.NullInt64{Int64: 100, Valid: true}}
	done := &Task{TaskID: "done", Title: SqlStr("done"), Status: SqlStr("completed"), LastModified: sql.NullInt64{Int64: 200, Valid: true}}
	otherDone := &Task{TaskID: "other", Title: SqlStr("other"), Status: SqlStr("completed"), LastModified: sql.NullInt64{Int64: 300, Valid: true}}
	for _, task := range []*Task{open, done} {
		if err := store.Create(ctx, task); err != nil {
			t.Fatalf("Create %s: %v", task.TaskID, err)
		}
	}
	if err := New(store.db, 7).Create(ctx, otherDone); err != nil {
		t.Fatalf("Create other: %v", err)
	}

	max, err := store.MaxLastModified(ctx)
	if err != nil {
		t.Fatalf("MaxLastModified: %v", err)
	}
	if max != 200 {
		t.Fatalf("MaxLastModified = %d, want 200", max)
	}

	n, err := store.DeleteCompleted(ctx)
	if err != nil {
		t.Fatalf("DeleteCompleted: %v", err)
	}
	if n != 1 {
		t.Fatalf("DeleteCompleted affected %d, want 1", n)
	}
	if _, err := store.Get(ctx, "done"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted completed Get: got %v, want ErrNotFound", err)
	}
	if _, err := New(store.db, 7).Get(ctx, "other"); err != nil {
		t.Fatalf("other user's completed task should survive: %v", err)
	}

	max, err = store.MaxLastModified(ctx)
	if err != nil {
		t.Fatalf("MaxLastModified after delete completed: %v", err)
	}
	if max != 100 {
		t.Fatalf("MaxLastModified after delete completed = %d, want 100", max)
	}

	if err := store.Delete(ctx, "open"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, "open"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("soft-deleted Get: got %v, want ErrNotFound", err)
	}
	max, err = store.MaxLastModified(ctx)
	if err != nil {
		t.Fatalf("MaxLastModified empty: %v", err)
	}
	if max != 0 {
		t.Fatalf("MaxLastModified empty = %d, want 0", max)
	}
}

func TestStoreGetMissingReturnsErrNotFound(t *testing.T) {
	store, _ := newSQLStore(t, 42)
	if _, err := store.Get(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: got %v, want ErrNotFound", err)
	}
}
