package caldav

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	ical "github.com/emersion/go-ical"
	gocaldav "github.com/emersion/go-webdav/caldav"
	"github.com/sysop/ultrabridge/internal/taskstore"
)

// mockTaskStore implements TaskStore for testing
type mockTaskStore struct {
	tasks map[string]*taskstore.Task
}

func newMockTaskStore() *mockTaskStore {
	return &mockTaskStore{
		tasks: make(map[string]*taskstore.Task),
	}
}

func (m *mockTaskStore) List(ctx context.Context) ([]taskstore.Task, error) {
	var result []taskstore.Task
	for _, t := range m.tasks {
		result = append(result, *t)
	}
	return result, nil
}

func (m *mockTaskStore) Get(ctx context.Context, taskID string) (*taskstore.Task, error) {
	if t, ok := m.tasks[taskID]; ok {
		return t, nil
	}
	return nil, taskstore.ErrNotFound
}

func (m *mockTaskStore) Create(ctx context.Context, t *taskstore.Task) error {
	if t.TaskID == "" {
		t.TaskID = fmt.Sprintf("task-%d", len(m.tasks))
	}
	if !t.LastModified.Valid {
		t.LastModified = sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true}
	}
	// Only set CompletedTime when task status is "completed"
	if t.Status.Valid && t.Status.String == "completed" && !t.CompletedTime.Valid {
		t.CompletedTime = sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true}
	}
	m.tasks[t.TaskID] = t
	return nil
}

func (m *mockTaskStore) Update(ctx context.Context, t *taskstore.Task) error {
	if _, ok := m.tasks[t.TaskID]; !ok {
		return fmt.Errorf("task not found")
	}
	t.LastModified = sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true}
	m.tasks[t.TaskID] = t
	return nil
}

func (m *mockTaskStore) Delete(ctx context.Context, taskID string) error {
	delete(m.tasks, taskID)
	return nil
}

func (m *mockTaskStore) DeleteCompleted(ctx context.Context) (int64, error) {
	var count int64
	for id, t := range m.tasks {
		if t.Status.Valid && t.Status.String == "completed" {
			delete(m.tasks, id)
			count++
		}
	}
	return count, nil
}

func (m *mockTaskStore) MaxLastModified(ctx context.Context) (int64, error) {
	var max int64
	for _, t := range m.tasks {
		if t.LastModified.Valid && t.LastModified.Int64 > max {
			max = t.LastModified.Int64
		}
	}
	return max, nil
}

// TestCalendarHomeSetPath verifies AC2.1: CalendarHomeSetPath returns correct prefix path
func TestCalendarHomeSetPath(t *testing.T) {
	store := newMockTaskStore()
	backend := NewBackend(store, "/caldav", "Test Collection", "preserve", nil)
	ctx := context.Background()

	path, err := backend.CalendarHomeSetPath(ctx)
	if err != nil {
		t.Fatalf("CalendarHomeSetPath failed: %v", err)
	}

	expected := "/caldav/user/calendars/"
	if path != expected {
		t.Errorf("CalendarHomeSetPath: got %q, want %q", path, expected)
	}
}

func TestCurrentUserPrincipalAndGetCalendar(t *testing.T) {
	store := newMockTaskStore()
	backend := NewBackend(store, "/caldav/", "Test Collection", "preserve", nil)
	ctx := context.Background()

	principal, err := backend.CurrentUserPrincipal(ctx)
	if err != nil {
		t.Fatalf("CurrentUserPrincipal: %v", err)
	}
	if principal != "/caldav/user/" {
		t.Fatalf("principal = %q, want /caldav/user/", principal)
	}

	cal, err := backend.GetCalendar(ctx, "/caldav/user/calendars/tasks/")
	if err != nil {
		t.Fatalf("GetCalendar valid path: %v", err)
	}
	if cal.Path != "/caldav/user/calendars/tasks/" || cal.Name != "Test Collection" {
		t.Fatalf("calendar = %+v", cal)
	}
	if _, err := backend.GetCalendar(ctx, "/caldav/user/calendars/missing/"); err == nil {
		t.Fatal("GetCalendar missing path should error")
	}
}

func TestSetCollectionNameAffectsRuntimeCalendars(t *testing.T) {
	store := newMockTaskStore()
	backend := NewBackend(store, "/caldav", "Old Name", "preserve", nil)

	backend.SetCollectionName("Fresh Name")
	if got := backend.CollectionName(); got != "Fresh Name" {
		t.Fatalf("CollectionName = %q, want Fresh Name", got)
	}
	calendars, err := backend.ListCalendars(context.Background())
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	if len(calendars) != 1 || calendars[0].Name != "Fresh Name" {
		t.Fatalf("runtime calendar name not updated: %+v", calendars)
	}
}

// TestListCalendarsSupportedComponents verifies AC2.2: ListCalendars returns collection with VTODO support
func TestListCalendarsSupportedComponents(t *testing.T) {
	store := newMockTaskStore()
	backend := NewBackend(store, "/caldav", "Test Collection", "preserve", nil)
	ctx := context.Background()

	calendars, err := backend.ListCalendars(ctx)
	if err != nil {
		t.Fatalf("ListCalendars failed: %v", err)
	}

	if len(calendars) != 1 {
		t.Fatalf("ListCalendars: got %d calendars, want 1", len(calendars))
	}

	cal := calendars[0]
	if len(cal.SupportedComponentSet) == 0 {
		t.Errorf("ListCalendars: SupportedComponentSet is empty")
	}

	hasVTODO := false
	for _, comp := range cal.SupportedComponentSet {
		if comp == "VTODO" {
			hasVTODO = true
			break
		}
	}
	if !hasVTODO {
		t.Errorf("ListCalendars: VTODO not in SupportedComponentSet: %v", cal.SupportedComponentSet)
	}
}

// TestListCalendarsName verifies AC2.3: ListCalendars returns collection with configured name
func TestListCalendarsName(t *testing.T) {
	store := newMockTaskStore()
	collectionName := "My Custom Tasks"
	backend := NewBackend(store, "/caldav", collectionName, "preserve", nil)
	ctx := context.Background()

	calendars, err := backend.ListCalendars(ctx)
	if err != nil {
		t.Fatalf("ListCalendars failed: %v", err)
	}

	if len(calendars) != 1 {
		t.Fatalf("ListCalendars: got %d calendars, want 1", len(calendars))
	}

	if calendars[0].Name != collectionName {
		t.Errorf("ListCalendars: name got %q, want %q", calendars[0].Name, collectionName)
	}
}

// TestPutCalendarObjectCreateAndUpdateCTag verifies AC2.4: CTag changes on write operations
func TestPutCalendarObjectCreateAndUpdateCTag(t *testing.T) {
	store := newMockTaskStore()
	backend := NewBackend(store, "/caldav", "Test Collection", "preserve", nil)
	ctx := context.Background()

	// Create a task via VTODO
	cal := ical.NewCalendar()
	cal.Props.SetText("PRODID", "-//Test//Test//EN")
	cal.Props.SetText("VERSION", "2.0")

	todo := ical.NewComponent("VTODO")
	todo.Props.SetText("UID", "test-task-1")
	todo.Props.SetText("SUMMARY", "Test Task")
	todo.Props.SetText("STATUS", "NEEDS-ACTION")
	cal.Children = append(cal.Children, todo)

	_, err := backend.PutCalendarObject(ctx, "/caldav/user/calendars/tasks/test-task-1.ics", cal, nil)
	if err != nil {
		t.Fatalf("PutCalendarObject (create) failed: %v", err)
	}

	// Get initial CTag
	tasks1, _ := store.List(ctx)
	ctag1 := taskstore.ComputeCTag(tasks1)

	// Update the task (change status)
	todo2 := ical.NewComponent("VTODO")
	todo2.Props.SetText("UID", "test-task-1")
	todo2.Props.SetText("SUMMARY", "Test Task Updated")
	todo2.Props.SetText("STATUS", "COMPLETED")
	cal2 := ical.NewCalendar()
	cal2.Props.SetText("PRODID", "-//Test//Test//EN")
	cal2.Props.SetText("VERSION", "2.0")
	cal2.Children = append(cal2.Children, todo2)

	// Sleep 1ms to ensure the updated task's last_modified timestamp differs from the original.
	// Since Create sets LastModified to time.Now().UnixMilli(), we need a visible time difference
	// before Update runs to get a different millisecond value for CTag computation.
	time.Sleep(1 * time.Millisecond)
	_, err = backend.PutCalendarObject(ctx, "/caldav/user/calendars/tasks/test-task-1.ics", cal2, nil)
	if err != nil {
		t.Fatalf("PutCalendarObject (update) failed: %v", err)
	}

	// Get updated CTag
	tasks2, _ := store.List(ctx)
	ctag2 := taskstore.ComputeCTag(tasks2)

	if ctag1 == ctag2 {
		t.Errorf("CTag did not change after update: ctag1=%s, ctag2=%s", ctag1, ctag2)
	}
}

// TestPutCalendarObjectRejectVEVENT verifies AC2.5: VEVENTs are rejected
func TestPutCalendarObjectRejectVEVENT(t *testing.T) {
	store := newMockTaskStore()
	backend := NewBackend(store, "/caldav", "Test Collection", "preserve", nil)
	ctx := context.Background()

	// Create a calendar with VEVENT (not VTODO)
	cal := ical.NewCalendar()
	cal.Props.SetText("PRODID", "-//Test//Test//EN")
	cal.Props.SetText("VERSION", "2.0")

	event := ical.NewComponent("VEVENT")
	event.Props.SetText("UID", "event-1")
	event.Props.SetText("SUMMARY", "Test Event")
	cal.Children = append(cal.Children, event)

	_, err := backend.PutCalendarObject(ctx, "/caldav/user/calendars/tasks/event-1.ics", cal, nil)
	if err == nil {
		t.Errorf("PutCalendarObject should reject VEVENT, but succeeded")
	}
}

// TestGetCalendarObjectExists verifies GetCalendarObject returns correct CalendarObject for known task
func TestGetCalendarObjectExists(t *testing.T) {
	store := newMockTaskStore()
	backend := NewBackend(store, "/caldav", "Test Collection", "preserve", nil)
	ctx := context.Background()

	// Create a task directly in the store
	task := &taskstore.Task{
		TaskID:        "test-task-1",
		Title:         taskstore.SqlStr("Test Task"),
		Status:        taskstore.SqlStr("needsAction"),
		LastModified:  sql.NullInt64{Int64: 1000, Valid: true},
		CompletedTime: sql.NullInt64{Int64: 1000, Valid: true},
	}
	store.tasks[task.TaskID] = task

	obj, err := backend.GetCalendarObject(ctx, "/caldav/user/calendars/tasks/test-task-1.ics", nil)
	if err != nil {
		t.Fatalf("GetCalendarObject failed: %v", err)
	}

	if obj.Path != "/caldav/user/calendars/tasks/test-task-1.ics" {
		t.Errorf("GetCalendarObject: path got %q, want %q", obj.Path, "/caldav/user/calendars/tasks/test-task-1.ics")
	}

	if obj.Data == nil {
		t.Errorf("GetCalendarObject: data is nil")
	}
}

// TestGetCalendarObjectNotFound verifies GetCalendarObject returns error for missing task
func TestGetCalendarObjectNotFound(t *testing.T) {
	store := newMockTaskStore()
	backend := NewBackend(store, "/caldav", "Test Collection", "preserve", nil)
	ctx := context.Background()

	_, err := backend.GetCalendarObject(ctx, "/caldav/user/calendars/tasks/nonexistent.ics", nil)
	if err == nil {
		t.Errorf("GetCalendarObject should return error for missing task, but succeeded")
	}
}

// TestListCalendarObjectsEmpty verifies ListCalendarObjects returns empty list when no tasks exist
func TestListCalendarObjectsEmpty(t *testing.T) {
	store := newMockTaskStore()
	backend := NewBackend(store, "/caldav", "Test Collection", "preserve", nil)
	ctx := context.Background()

	objects, err := backend.ListCalendarObjects(ctx, "/caldav/user/calendars/tasks/", nil)
	if err != nil {
		t.Fatalf("ListCalendarObjects failed: %v", err)
	}

	if len(objects) != 0 {
		t.Errorf("ListCalendarObjects: got %d objects, want 0", len(objects))
	}
}

// TestListCalendarObjectsWithTasks verifies ListCalendarObjects returns all tasks
func TestListCalendarObjectsWithTasks(t *testing.T) {
	store := newMockTaskStore()
	backend := NewBackend(store, "/caldav", "Test Collection", "preserve", nil)
	ctx := context.Background()

	// Create multiple tasks
	for i := 1; i <= 3; i++ {
		task := &taskstore.Task{
			TaskID:        fmt.Sprintf("task-%d", i),
			Title:         taskstore.SqlStr(fmt.Sprintf("Task %d", i)),
			Status:        taskstore.SqlStr("needsAction"),
			LastModified:  sql.NullInt64{Int64: int64(1000 * i), Valid: true},
			CompletedTime: sql.NullInt64{Int64: int64(1000 * i), Valid: true},
		}
		store.tasks[task.TaskID] = task
	}

	objects, err := backend.ListCalendarObjects(ctx, "/caldav/user/calendars/tasks/", nil)
	if err != nil {
		t.Fatalf("ListCalendarObjects failed: %v", err)
	}

	if len(objects) != 3 {
		t.Errorf("ListCalendarObjects: got %d objects, want 3", len(objects))
	}
}

// TestDeleteCalendarObject verifies DeleteCalendarObject marks task deleted
func TestDeleteCalendarObject(t *testing.T) {
	store := newMockTaskStore()
	backend := NewBackend(store, "/caldav", "Test Collection", "preserve", nil)
	ctx := context.Background()

	// Create a task
	task := &taskstore.Task{
		TaskID:        "test-task-1",
		Title:         taskstore.SqlStr("Test Task"),
		Status:        taskstore.SqlStr("needsAction"),
		LastModified:  sql.NullInt64{Int64: 1000, Valid: true},
		CompletedTime: sql.NullInt64{Int64: 1000, Valid: true},
	}
	store.tasks[task.TaskID] = task

	// Delete it
	err := backend.DeleteCalendarObject(ctx, "/caldav/user/calendars/tasks/test-task-1.ics")
	if err != nil {
		t.Fatalf("DeleteCalendarObject failed: %v", err)
	}

	// Verify it's deleted
	if _, ok := store.tasks["test-task-1"]; ok {
		t.Errorf("DeleteCalendarObject: task still exists after delete")
	}
}

// TestPutCalendarObjectCreateNewTask verifies PutCalendarObject with new VTODO creates task
func TestPutCalendarObjectCreateNewTask(t *testing.T) {
	store := newMockTaskStore()
	backend := NewBackend(store, "/caldav", "Test Collection", "preserve", nil)
	ctx := context.Background()

	cal := ical.NewCalendar()
	cal.Props.SetText("PRODID", "-//Test//Test//EN")
	cal.Props.SetText("VERSION", "2.0")

	todo := ical.NewComponent("VTODO")
	todo.Props.SetText("UID", "new-task")
	todo.Props.SetText("SUMMARY", "New Task")
	todo.Props.SetText("STATUS", "NEEDS-ACTION")
	cal.Children = append(cal.Children, todo)

	obj, err := backend.PutCalendarObject(ctx, "/caldav/user/calendars/tasks/new-task.ics", cal, nil)
	if err != nil {
		t.Fatalf("PutCalendarObject failed: %v", err)
	}

	if obj == nil {
		t.Fatalf("PutCalendarObject returned nil object")
	}

	// Verify task exists in store
	task, err := store.Get(ctx, "new-task")
	if err != nil {
		t.Fatalf("Task not created: %v", err)
	}

	if taskstore.NullStr(task.Title) != "New Task" {
		t.Errorf("Task title: got %q, want %q", taskstore.NullStr(task.Title), "New Task")
	}
}

// TestPutCalendarObjectUpdateExistingTask verifies PutCalendarObject with existing task ID updates task
func TestPutCalendarObjectUpdateExistingTask(t *testing.T) {
	store := newMockTaskStore()
	backend := NewBackend(store, "/caldav", "Test Collection", "preserve", nil)
	ctx := context.Background()

	// Create initial task
	task := &taskstore.Task{
		TaskID:        "existing-task",
		Title:         taskstore.SqlStr("Original Title"),
		Status:        taskstore.SqlStr("needsAction"),
		LastModified:  sql.NullInt64{Int64: 1000, Valid: true},
		CompletedTime: sql.NullInt64{Int64: 1000, Valid: true},
	}
	store.tasks[task.TaskID] = task

	// Update via PutCalendarObject
	cal := ical.NewCalendar()
	cal.Props.SetText("PRODID", "-//Test//Test//EN")
	cal.Props.SetText("VERSION", "2.0")

	todo := ical.NewComponent("VTODO")
	todo.Props.SetText("UID", "existing-task")
	todo.Props.SetText("SUMMARY", "Updated Title")
	todo.Props.SetText("STATUS", "COMPLETED")
	cal.Children = append(cal.Children, todo)

	time.Sleep(1 * time.Millisecond)
	_, err := backend.PutCalendarObject(ctx, "/caldav/user/calendars/tasks/existing-task.ics", cal, nil)
	if err != nil {
		t.Fatalf("PutCalendarObject failed: %v", err)
	}

	// Verify update
	updated, err := store.Get(ctx, "existing-task")
	if err != nil {
		t.Fatalf("Task not found after update: %v", err)
	}

	if taskstore.NullStr(updated.Title) != "Updated Title" {
		t.Errorf("Task title: got %q, want %q", taskstore.NullStr(updated.Title), "Updated Title")
	}
	if taskstore.NullStr(updated.Status) != "completed" {
		t.Errorf("Task status: got %q, want %q", taskstore.NullStr(updated.Status), "completed")
	}
}

// TestQueryCalendarObjects verifies QueryCalendarObjects returns all non-deleted tasks
func TestQueryCalendarObjects(t *testing.T) {
	store := newMockTaskStore()
	backend := NewBackend(store, "/caldav", "Test Collection", "preserve", nil)
	ctx := context.Background()

	// Create multiple tasks
	for i := 1; i <= 2; i++ {
		task := &taskstore.Task{
			TaskID:        fmt.Sprintf("query-task-%d", i),
			Title:         taskstore.SqlStr(fmt.Sprintf("Query Task %d", i)),
			Status:        taskstore.SqlStr("needsAction"),
			LastModified:  sql.NullInt64{Int64: int64(1000 * i), Valid: true},
			CompletedTime: sql.NullInt64{Int64: int64(1000 * i), Valid: true},
		}
		store.tasks[task.TaskID] = task
	}

	// Query with nil CompFilter
	query := &gocaldav.CalendarQuery{
		CompFilter: gocaldav.CompFilter{},
	}
	objects, err := backend.QueryCalendarObjects(ctx, "/caldav/user/calendars/tasks/", query)
	if err != nil {
		t.Fatalf("QueryCalendarObjects failed: %v", err)
	}

	if len(objects) != 2 {
		t.Errorf("QueryCalendarObjects: got %d objects, want 2", len(objects))
	}
}
