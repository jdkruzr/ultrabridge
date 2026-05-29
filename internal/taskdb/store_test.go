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
