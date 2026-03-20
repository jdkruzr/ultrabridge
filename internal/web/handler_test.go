package web

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/logging"
	"github.com/sysop/ultrabridge/internal/notestore"
	"github.com/sysop/ultrabridge/internal/processor"
	"github.com/sysop/ultrabridge/internal/search"
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
		if t.IsDeleted != "Y" {
			result = append(result, *t)
		}
	}
	return result, nil
}

func (m *mockTaskStore) Get(ctx context.Context, taskID string) (*taskstore.Task, error) {
	if t, ok := m.tasks[taskID]; ok {
		return t, nil
	}
	return nil, fmt.Errorf("task not found")
}

func (m *mockTaskStore) Create(ctx context.Context, t *taskstore.Task) error {
	if t.TaskID == "" {
		t.TaskID = fmt.Sprintf("task-%d", len(m.tasks))
	}
	if !t.LastModified.Valid {
		t.LastModified = sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true}
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
	if t, ok := m.tasks[taskID]; ok {
		t.IsDeleted = "Y"
		return nil
	}
	return fmt.Errorf("task not found")
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

// mockNotifier implements SyncNotifier for testing
type mockNotifier struct {
	called int
	lastErr error
}

func (m *mockNotifier) Notify(ctx context.Context) error {
	m.called++
	return m.lastErr
}

// mockNoteStore implements NoteStore for testing
type mockNoteStore struct {
	files map[string][]notestore.NoteFile
	err   error
}

func newMockNoteStore() *mockNoteStore {
	return &mockNoteStore{files: make(map[string][]notestore.NoteFile)}
}

func (m *mockNoteStore) List(ctx context.Context, relPath string) ([]notestore.NoteFile, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.files[relPath], nil
}

func (m *mockNoteStore) Get(ctx context.Context, path string) (*notestore.NoteFile, error) {
	return nil, notestore.ErrNotFound
}

func (m *mockNoteStore) Scan(ctx context.Context) ([]string, error) {
	return nil, nil
}

func (m *mockNoteStore) UpsertFile(_ context.Context, _ string) error { return nil }

func (m *mockNoteStore) SetHash(_ context.Context, _, _ string) error { return nil }

func (m *mockNoteStore) LookupByHash(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
}

// mockSearchIndex implements SearchIndex for testing
type mockSearchIndex struct{}

func (m *mockSearchIndex) Index(_ context.Context, _ search.NoteDocument) error { return nil }
func (m *mockSearchIndex) Search(_ context.Context, _ search.SearchQuery) ([]search.SearchResult, error) {
	return nil, nil
}
func (m *mockSearchIndex) Delete(_ context.Context, _ string) error { return nil }
func (m *mockSearchIndex) IndexPage(_ context.Context, _ string, _ int, _, _, _, _ string) error {
	return nil
}
func (m *mockSearchIndex) GetContent(_ context.Context, _ string) ([]search.NoteDocument, error) {
	return nil, nil
}

// mockProcessor implements Processor for testing
type mockProcessor struct {
	running bool
	jobs    map[string]string // path → status
	skips   map[string]string // path → skip reason
}

func newMockProcessor() *mockProcessor {
	return &mockProcessor{
		jobs:  make(map[string]string),
		skips: make(map[string]string),
	}
}

func (m *mockProcessor) Start(_ context.Context) error  { m.running = true; return nil }
func (m *mockProcessor) Stop() error                     { m.running = false; return nil }
func (m *mockProcessor) Status() processor.ProcessorStatus {
	return processor.ProcessorStatus{Running: m.running, Pending: len(m.jobs)}
}
func (m *mockProcessor) Enqueue(_ context.Context, path string) error {
	m.jobs[path] = processor.StatusPending
	return nil
}
func (m *mockProcessor) Skip(_ context.Context, path, reason string) error {
	m.jobs[path] = processor.StatusSkipped
	m.skips[path] = reason
	return nil
}
func (m *mockProcessor) Unskip(_ context.Context, path string) error {
	if m.jobs[path] == processor.StatusSkipped {
		m.jobs[path] = processor.StatusPending
		delete(m.skips, path)
	}
	return nil
}
func (m *mockProcessor) GetJob(_ context.Context, path string) (*processor.Job, error) {
	status, ok := m.jobs[path]
	if !ok {
		return nil, nil
	}
	return &processor.Job{NotePath: path, Status: status, SkipReason: m.skips[path]}, nil
}

// TestListTasksReturnsNonDeletedTasks verifies AC4.1: store returns list of all non-deleted tasks
func TestListTasksReturnsNonDeletedTasks(t *testing.T) {
	store := newMockTaskStore()

	// Pre-populate with tasks
	task1 := &taskstore.Task{
		TaskID:    "task-1",
		Title:     taskstore.SqlStr("Buy groceries"),
		Status:    taskstore.SqlStr("needsAction"),
		DueTime:   0,
		IsDeleted: "N",
	}
	task2 := &taskstore.Task{
		TaskID:    "task-2",
		Title:     taskstore.SqlStr("Write report"),
		Status:    taskstore.SqlStr("completed"),
		IsDeleted: "N",
	}
	store.tasks[task1.TaskID] = task1
	store.tasks[task2.TaskID] = task2

	// List tasks
	ctx := context.Background()
	tasks, err := store.List(ctx)

	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(tasks) != 2 {
		t.Errorf("Expected 2 tasks, got %d", len(tasks))
	}

	titles := make(map[string]bool)
	for _, task := range tasks {
		titles[taskstore.NullStr(task.Title)] = true
	}

	if !titles["Buy groceries"] {
		t.Errorf("Task title 'Buy groceries' not in list")
	}
	if !titles["Write report"] {
		t.Errorf("Task title 'Write report' not in list")
	}
}

// TestListTasksEmpty verifies store.List with empty store
func TestListTasksEmpty(t *testing.T) {
	store := newMockTaskStore()

	ctx := context.Background()
	tasks, err := store.List(ctx)

	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(tasks) != 0 {
		t.Errorf("Expected 0 tasks, got %d", len(tasks))
	}
}

// TestGetIndexResponseBodyVerifiesAC41 verifies AC4.1: HTTP response contains task titles
func TestGetIndexResponseBodyVerifiesAC41(t *testing.T) {
	store := newMockTaskStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := NewHandler(store, nil, nil, nil, nil, logger, broadcaster)

	// Pre-populate with multiple tasks
	task1 := &taskstore.Task{
		TaskID:    "task-1",
		Title:     taskstore.SqlStr("Buy groceries"),
		Status:    taskstore.SqlStr("needsAction"),
		IsDeleted: "N",
	}
	task2 := &taskstore.Task{
		TaskID:    "task-2",
		Title:     taskstore.SqlStr("Write report"),
		Status:    taskstore.SqlStr("completed"),
		IsDeleted: "N",
	}
	store.tasks[task1.TaskID] = task1
	store.tasks[task2.TaskID] = task2

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Verify HTTP response is 200 OK
	if w.Code != http.StatusOK {
		t.Errorf("GET / returned status %d, want 200", w.Code)
	}

	// Verify response body contains both task titles
	body := w.Body.String()
	if !strings.Contains(body, "Buy groceries") {
		t.Errorf("Response should contain 'Buy groceries', got:\n%s", body)
	}
	if !strings.Contains(body, "Write report") {
		t.Errorf("Response should contain 'Write report', got:\n%s", body)
	}

	// Verify response contains the task statuses
	if !strings.Contains(body, "Needs Action") {
		t.Errorf("Response should contain 'Needs Action' status")
	}
	if !strings.Contains(body, "Completed") {
		t.Errorf("Response should contain 'Completed' status")
	}
}

// TestGetIndexFiltersDeletedTasks verifies that deleted tasks (IsDeleted="Y") are not shown
func TestGetIndexFiltersDeletedTasks(t *testing.T) {
	store := newMockTaskStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := NewHandler(store, nil, nil, nil, nil, logger, broadcaster)

	// Add a non-deleted task
	task1 := &taskstore.Task{
		TaskID:    "task-1",
		Title:     taskstore.SqlStr("Active task"),
		Status:    taskstore.SqlStr("needsAction"),
		IsDeleted: "N",
	}
	store.tasks[task1.TaskID] = task1

	// Add a deleted task
	task2 := &taskstore.Task{
		TaskID:    "task-2",
		Title:     taskstore.SqlStr("Deleted task"),
		Status:    taskstore.SqlStr("needsAction"),
		IsDeleted: "Y",
	}
	store.tasks[task2.TaskID] = task2

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Verify HTTP response is 200 OK
	if w.Code != http.StatusOK {
		t.Errorf("GET / returned status %d, want %d", w.Code, http.StatusOK)
	}

	// Verify response body contains the active task
	body := w.Body.String()
	if !strings.Contains(body, "Active task") {
		t.Errorf("Response should contain 'Active task', got:\n%s", body)
	}

	// Verify response body does NOT contain the deleted task
	if strings.Contains(body, "Deleted task") {
		t.Errorf("Response should NOT contain 'Deleted task', got:\n%s", body)
	}
}

// TestPostCreateTaskMinimal verifies AC4.2: POST /tasks with form data creates a task
func TestPostCreateTaskMinimal(t *testing.T) {
	store := newMockTaskStore()
	notifier := &mockNotifier{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := NewHandler(store, notifier, nil, nil, nil, logger, broadcaster)

	// Create task via form POST
	form := url.Values{}
	form.Set("title", "Test Task")
	req := httptest.NewRequest("POST", "/tasks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Verify redirect to /
	if w.Code != http.StatusSeeOther {
		t.Errorf("POST /tasks returned status %d, want %d", w.Code, http.StatusSeeOther)
	}
	if w.Header().Get("Location") != "/" {
		t.Errorf("Redirect location is %s, want /", w.Header().Get("Location"))
	}

	// Verify task was created in store
	if len(store.tasks) != 1 {
		t.Errorf("Expected 1 task in store, got %d", len(store.tasks))
	}

	// Verify the task has correct data
	for _, task := range store.tasks {
		if taskstore.NullStr(task.Title) != "Test Task" {
			t.Errorf("Task title is %q, want %q", taskstore.NullStr(task.Title), "Test Task")
		}
		if taskstore.NullStr(task.Status) != "needsAction" {
			t.Errorf("Task status is %q, want %q", taskstore.NullStr(task.Status), "needsAction")
		}
		if task.IsDeleted != "N" {
			t.Errorf("Task IsDeleted is %q, want %q", task.IsDeleted, "N")
		}
	}

	// Verify notifier was called
	if notifier.called != 1 {
		t.Errorf("Notifier was called %d times, want 1", notifier.called)
	}
}

// TestPostCreateTaskWithDueDate verifies POST /tasks with optional due date
func TestPostCreateTaskWithDueDate(t *testing.T) {
	store := newMockTaskStore()
	notifier := &mockNotifier{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := NewHandler(store, notifier, nil, nil, nil, logger, broadcaster)

	// Create task with due date
	form := url.Values{}
	form.Set("title", "Task with deadline")
	form.Set("due_date", "2026-12-25")
	req := httptest.NewRequest("POST", "/tasks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("POST /tasks returned status %d, want %d", w.Code, http.StatusSeeOther)
	}

	// Verify task has correct due date
	if len(store.tasks) != 1 {
		t.Fatalf("Expected 1 task in store, got %d", len(store.tasks))
	}

	for _, task := range store.tasks {
		// 2026-12-25 in UTC
		expectedTime := time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC)
		expectedMs := expectedTime.UnixMilli()

		if task.DueTime != expectedMs {
			t.Errorf("Task DueTime is %d, want %d", task.DueTime, expectedMs)
		}
	}
}

// TestPostCreateTaskNoTitle verifies that POST /tasks without title returns error
func TestPostCreateTaskNoTitle(t *testing.T) {
	store := newMockTaskStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := NewHandler(store, nil, nil, nil, nil, logger, broadcaster)

	form := url.Values{}
	form.Set("title", "")
	req := httptest.NewRequest("POST", "/tasks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("POST /tasks with no title returned status %d, want %d", w.Code, http.StatusBadRequest)
	}

	if len(store.tasks) != 0 {
		t.Errorf("Expected 0 tasks in store, got %d", len(store.tasks))
	}
}

// TestPostCreateTaskInvalidDueDate verifies that invalid due date returns error
func TestPostCreateTaskInvalidDueDate(t *testing.T) {
	store := newMockTaskStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := NewHandler(store, nil, nil, nil, nil, logger, broadcaster)

	form := url.Values{}
	form.Set("title", "Task")
	form.Set("due_date", "not-a-date")
	req := httptest.NewRequest("POST", "/tasks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("POST /tasks with invalid due date returned status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestPostCompleteTaskUpdatesStatus verifies AC4.3: POST /tasks/{id}/complete marks task complete
func TestPostCompleteTaskUpdatesStatus(t *testing.T) {
	store := newMockTaskStore()
	notifier := &mockNotifier{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := NewHandler(store, notifier, nil, nil, nil, logger, broadcaster)

	// Create a task
	task := &taskstore.Task{
		TaskID:    "task-1",
		Title:     taskstore.SqlStr("Incomplete task"),
		Status:    taskstore.SqlStr("needsAction"),
		IsDeleted: "N",
	}
	store.tasks[task.TaskID] = task

	// Complete the task
	req := httptest.NewRequest("POST", "/tasks/task-1/complete", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Verify redirect
	if w.Code != http.StatusSeeOther {
		t.Errorf("POST /tasks/{id}/complete returned status %d, want %d", w.Code, http.StatusSeeOther)
	}

	// Verify task status is now completed
	completedTask := store.tasks["task-1"]
	if taskstore.NullStr(completedTask.Status) != "completed" {
		t.Errorf("Task status is %q, want 'completed'", taskstore.NullStr(completedTask.Status))
	}

	// Verify completedTime is set
	if !completedTask.CompletedTime.Valid || completedTask.CompletedTime.Int64 == 0 {
		t.Errorf("Task CompletedTime should be set")
	}

	// Verify notifier was called
	if notifier.called != 1 {
		t.Errorf("Notifier was called %d times, want 1", notifier.called)
	}
}

// TestPostCompleteTaskAlreadyCompleted verifies completing an already-completed task
func TestPostCompleteTaskAlreadyCompleted(t *testing.T) {
	store := newMockTaskStore()
	notifier := &mockNotifier{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := NewHandler(store, notifier, nil, nil, nil, logger, broadcaster)

	// Create an already-completed task
	completionTime := time.Now().UnixMilli()
	task := &taskstore.Task{
		TaskID:        "task-1",
		Title:         taskstore.SqlStr("Completed task"),
		Status:        taskstore.SqlStr("completed"),
		CompletedTime: sql.NullInt64{Int64: completionTime, Valid: true},
		IsDeleted:     "N",
	}
	store.tasks[task.TaskID] = task

	// Try to complete it again
	req := httptest.NewRequest("POST", "/tasks/task-1/complete", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Should still succeed with redirect
	if w.Code != http.StatusSeeOther {
		t.Errorf("POST /tasks/{id}/complete returned status %d, want %d", w.Code, http.StatusSeeOther)
	}

	// Status should remain completed
	if taskstore.NullStr(store.tasks["task-1"].Status) != "completed" {
		t.Errorf("Task status should remain 'completed'")
	}
}

// TestPostCompleteTaskNotFound verifies completing a non-existent task
func TestPostCompleteTaskNotFound(t *testing.T) {
	store := newMockTaskStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := NewHandler(store, nil, nil, nil, nil, logger, broadcaster)

	req := httptest.NewRequest("POST", "/tasks/nonexistent-id/complete", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("POST /tasks/nonexistent/complete returned status %d, want %d", w.Code, http.StatusNotFound)
	}
}

// TestPostCompleteTaskNoID verifies that missing task ID returns error
func TestPostCompleteTaskNoID(t *testing.T) {
	store := newMockTaskStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := NewHandler(store, nil, nil, nil, nil, logger, broadcaster)

	// Note: This test verifies the Go 1.22 route pattern parsing
	// In practice, /tasks/{id}/complete always extracts an id (could be empty)
	req := httptest.NewRequest("POST", "/tasks//complete", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Empty ID should be handled (either 400 or 404)
	if w.Code == http.StatusOK {
		t.Errorf("POST /tasks//complete should not return 200")
	}
}

// TestHandlerNotifierNil verifies that handler works when notifier is nil
func TestHandlerNotifierNil(t *testing.T) {
	store := newMockTaskStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := NewHandler(store, nil, nil, nil, nil, logger, broadcaster)

	form := url.Values{}
	form.Set("title", "Task without notifier")
	req := httptest.NewRequest("POST", "/tasks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Should succeed even without notifier
	if w.Code != http.StatusSeeOther {
		t.Errorf("POST /tasks without notifier returned status %d, want %d", w.Code, http.StatusSeeOther)
	}

	if len(store.tasks) != 1 {
		t.Errorf("Task should be created even without notifier")
	}
}

// TestPostCreateTaskWithWhitespace verifies title trimming
func TestPostCreateTaskWithWhitespace(t *testing.T) {
	store := newMockTaskStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := NewHandler(store, nil, nil, nil, nil, logger, broadcaster)

	form := url.Values{}
	form.Set("title", "  Task with spaces  ")
	req := httptest.NewRequest("POST", "/tasks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("POST /tasks with whitespace returned status %d, want %d", w.Code, http.StatusSeeOther)
	}

	if len(store.tasks) != 1 {
		t.Errorf("Task should be created with trimmed title")
	}

	for _, task := range store.tasks {
		if taskstore.NullStr(task.Title) != "Task with spaces" {
			t.Errorf("Title should be trimmed, got %q", taskstore.NullStr(task.Title))
		}
	}
}

func TestBulkCompleteMultipleTasks(t *testing.T) {
	store := newMockTaskStore()
	store.tasks["t1"] = &taskstore.Task{TaskID: "t1", Title: taskstore.SqlStr("Task 1"), Status: taskstore.SqlStr("needsAction"), IsDeleted: "N"}
	store.tasks["t2"] = &taskstore.Task{TaskID: "t2", Title: taskstore.SqlStr("Task 2"), Status: taskstore.SqlStr("needsAction"), IsDeleted: "N"}
	store.tasks["t3"] = &taskstore.Task{TaskID: "t3", Title: taskstore.SqlStr("Task 3"), Status: taskstore.SqlStr("needsAction"), IsDeleted: "N"}
	handler := NewHandler(store, nil, nil, nil, nil, slog.Default(), logging.NewLogBroadcaster())

	form := url.Values{}
	form.Set("action", "complete")
	form.Add("task_ids", "t1")
	form.Add("task_ids", "t3")
	req := httptest.NewRequest("POST", "/tasks/bulk", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", w.Code)
	}
	if store.tasks["t1"].Status.String != "completed" {
		t.Errorf("t1 should be completed, got %q", store.tasks["t1"].Status.String)
	}
	if store.tasks["t2"].Status.String != "needsAction" {
		t.Errorf("t2 should be unchanged, got %q", store.tasks["t2"].Status.String)
	}
	if store.tasks["t3"].Status.String != "completed" {
		t.Errorf("t3 should be completed, got %q", store.tasks["t3"].Status.String)
	}
}

func TestBulkDeleteMultipleTasks(t *testing.T) {
	store := newMockTaskStore()
	store.tasks["t1"] = &taskstore.Task{TaskID: "t1", Title: taskstore.SqlStr("Task 1"), IsDeleted: "N"}
	store.tasks["t2"] = &taskstore.Task{TaskID: "t2", Title: taskstore.SqlStr("Task 2"), IsDeleted: "N"}
	store.tasks["t3"] = &taskstore.Task{TaskID: "t3", Title: taskstore.SqlStr("Task 3"), IsDeleted: "N"}
	handler := NewHandler(store, nil, nil, nil, nil, slog.Default(), logging.NewLogBroadcaster())

	form := url.Values{}
	form.Set("action", "delete")
	form.Add("task_ids", "t1")
	form.Add("task_ids", "t2")
	req := httptest.NewRequest("POST", "/tasks/bulk", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", w.Code)
	}
	if store.tasks["t1"].IsDeleted != "Y" {
		t.Errorf("t1 should be deleted")
	}
	if store.tasks["t2"].IsDeleted != "Y" {
		t.Errorf("t2 should be deleted")
	}
	if store.tasks["t3"].IsDeleted != "N" {
		t.Errorf("t3 should be unchanged")
	}
}

func TestBulkActionNoSelection(t *testing.T) {
	store := newMockTaskStore()
	handler := NewHandler(store, nil, nil, nil, nil, slog.Default(), logging.NewLogBroadcaster())

	form := url.Values{}
	form.Set("action", "complete")
	// no task_ids
	req := httptest.NewRequest("POST", "/tasks/bulk", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", w.Code)
	}
}

func TestBulkActionUnknown(t *testing.T) {
	store := newMockTaskStore()
	store.tasks["t1"] = &taskstore.Task{TaskID: "t1", IsDeleted: "N"}
	handler := NewHandler(store, nil, nil, nil, nil, slog.Default(), logging.NewLogBroadcaster())

	form := url.Values{}
	form.Set("action", "explode")
	form.Add("task_ids", "t1")
	req := httptest.NewRequest("POST", "/tasks/bulk", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown action, got %d", w.Code)
	}
}

// TestHandleFiles_NoteStoreNil verifies AC1.6: missing UB_NOTES_PATH renders error, not crash
func TestHandleFiles_NoteStoreNil(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := NewHandler(newMockTaskStore(), nil, nil, nil, nil, logger, broadcaster)

	req := httptest.NewRequest("GET", "/files", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "UB_NOTES_PATH") {
		t.Error("expected UB_NOTES_PATH error message in response")
	}
}

// TestHandleFiles_PathTraversal verifies AC1.5: traversal attempts return 400
func TestHandleFiles_PathTraversal(t *testing.T) {
	ns := newMockNoteStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := NewHandler(newMockTaskStore(), nil, ns, nil, nil, logger, broadcaster)

	for _, badPath := range []string{"../../etc", "../secrets", "/etc/passwd"} {
		req := httptest.NewRequest("GET", "/files?path="+badPath, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("path %q: status = %d, want 400", badPath, w.Code)
		}
	}
}

// TestHandleFiles_TopLevel verifies AC1.1, AC1.3, AC1.4
func TestHandleFiles_TopLevel(t *testing.T) {
	ns := newMockNoteStore()
	ns.files[""] = []notestore.NoteFile{
		{Name: "test.note", FileType: notestore.FileTypeNote, RelPath: "test.note"},
		{Name: "readme.pdf", FileType: notestore.FileTypePDF, RelPath: "readme.pdf"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := NewHandler(newMockTaskStore(), nil, ns, nil, nil, logger, broadcaster)

	req := httptest.NewRequest("GET", "/files", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "test.note") {
		t.Error("expected test.note in response")
	}
	if !strings.Contains(body, "unprocessed") {
		t.Error("expected unprocessed badge for .note file with empty JobStatus")
	}
	if !strings.Contains(body, "unsupported") {
		t.Error("expected unsupported badge for pdf")
	}
}

// TestHandleFiles_WithPath verifies AC1.2: subdirectory path shows contents and breadcrumb
func TestHandleFiles_WithPath(t *testing.T) {
	ns := newMockNoteStore()
	ns.files["Note/Folder"] = []notestore.NoteFile{
		{Name: "deep.note", FileType: notestore.FileTypeNote, RelPath: "Note/Folder/deep.note"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := NewHandler(newMockTaskStore(), nil, ns, nil, nil, logger, broadcaster)

	req := httptest.NewRequest("GET", "/files?path=Note/Folder", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "deep.note") {
		t.Error("expected deep.note in response")
	}
	if !strings.Contains(body, "Folder") {
		t.Error("expected breadcrumb Folder in response")
	}
}

// Helper functions for C&C tests
func makeFilesHandler(t *testing.T, proc *mockProcessor) *Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	return NewHandler(newMockTaskStore(), nil, nil, nil, proc, logger, broadcaster)
}

func postFiles(handler *Handler, route, path, back string) *httptest.ResponseRecorder {
	form := url.Values{}
	form.Set("path", path)
	form.Set("back", back)
	req := httptest.NewRequest("POST", route, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// AC7.1: Queue creates pending job
func TestHandleFilesQueue(t *testing.T) {
	proc := newMockProcessor()
	h := makeFilesHandler(t, proc)
	w := postFiles(h, "/files/queue", "/test.note", "")
	if w.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", w.Code)
	}
	if proc.jobs["/test.note"] != processor.StatusPending {
		t.Errorf("job = %q, want pending", proc.jobs["/test.note"])
	}
}

// AC7.3: Skip sets skipped with manual reason
func TestHandleFilesSkip(t *testing.T) {
	proc := newMockProcessor()
	h := makeFilesHandler(t, proc)
	postFiles(h, "/files/skip", "/test.note", "")
	if proc.jobs["/test.note"] != processor.StatusSkipped {
		t.Errorf("job = %q, want skipped", proc.jobs["/test.note"])
	}
	if proc.skips["/test.note"] != processor.SkipReasonManual {
		t.Errorf("reason = %q, want manual", proc.skips["/test.note"])
	}
}

// AC7.4: Unskip re-enables queuing
func TestHandleFilesUnskip(t *testing.T) {
	proc := newMockProcessor()
	h := makeFilesHandler(t, proc)
	postFiles(h, "/files/skip", "/test.note", "")
	postFiles(h, "/files/unskip", "/test.note", "")
	if proc.jobs["/test.note"] != processor.StatusPending {
		t.Errorf("after unskip = %q, want pending", proc.jobs["/test.note"])
	}
}

// AC7.5: Force overrides size_limit skip
func TestHandleFilesForce(t *testing.T) {
	proc := newMockProcessor()
	proc.jobs["/big.note"] = processor.StatusSkipped
	proc.skips["/big.note"] = processor.SkipReasonSizeLimit
	h := makeFilesHandler(t, proc)
	postFiles(h, "/files/force", "/big.note", "")
	if proc.jobs["/big.note"] != processor.StatusPending {
		t.Errorf("after force = %q, want pending", proc.jobs["/big.note"])
	}
}

// AC2.5: Status endpoint returns JSON with running state and queue depth
func TestHandleFilesStatus(t *testing.T) {
	proc := newMockProcessor()
	proc.running = true
	proc.jobs["/a.note"] = processor.StatusPending

	h := makeFilesHandler(t, proc)
	req := httptest.NewRequest("GET", "/files/status", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
		t.Error("expected JSON content type")
	}
	body := w.Body.String()
	if !strings.Contains(body, "true") {
		t.Errorf("expected Running:true in JSON body: %s", body)
	}
}

// AC7.2: Requeue resets a failed or done job to pending
func TestHandleFilesRequeue(t *testing.T) {
	proc := newMockProcessor()
	// Pre-seed with a failed job
	proc.jobs["/test.note"] = processor.StatusFailed
	h := makeFilesHandler(t, proc)

	// Call /files/queue to requeue
	w := postFiles(h, "/files/queue", "/test.note", "")

	if w.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", w.Code)
	}
	if proc.jobs["/test.note"] != processor.StatusPending {
		t.Errorf("job status = %q, want pending", proc.jobs["/test.note"])
	}
}
