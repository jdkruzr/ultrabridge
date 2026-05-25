package web

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/appconfig"
	"github.com/sysop/ultrabridge/internal/chat"
	"github.com/sysop/ultrabridge/internal/logging"
	"github.com/sysop/ultrabridge/internal/mcpauth"
	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/notestore"
	"github.com/sysop/ultrabridge/internal/processor"
	"github.com/sysop/ultrabridge/internal/rag"
	"github.com/sysop/ultrabridge/internal/search"
	"github.com/sysop/ultrabridge/internal/service"
	"github.com/sysop/ultrabridge/internal/taskstore"
)

// RAGDisplayConfig is a legacy type for tests
type RAGDisplayConfig struct {
	OllamaURL   string
	OllamaModel string
	ChatAPIURL  string
	ChatModel   string
}

// LegacyNewHandler bridges the old 22-argument signature to the new 7-argument one.
func LegacyNewHandler(
	store service.TaskStore,
	notifier service.SyncNotifier,
	noteStore notestore.NoteStore,
	searchIndex search.SearchIndex,
	proc processor.Processor,
	scanner service.FileScanner,
	_ interface{}, // formerly syncProvider; sync-status surface removed
	booxStore service.BooxStore,
	booxImporter service.BooxImporter,
	booxNotesPath, notesPathPrefix string,
	noteDB *sql.DB,
	logger *slog.Logger,
	broadcaster *logging.LogBroadcaster,
	embedder rag.Embedder,
	embedStore *rag.Store,
	embedModel string,
	retriever rag.SearchRetriever,
	chatHandler *chat.Handler,
	chatStore *chat.Store,
	ragDisplay RAGDisplayConfig,
	runningConfig *appconfig.Config,
) *Handler {
	tasks := service.NewTaskService(store, notifier)
	booxCachePath := ""
	if booxNotesPath != "" {
		booxCachePath = filepath.Join(booxNotesPath, ".cache")
	}
	
	// Create actual service with mocks
	noteSvc := service.NewNoteService(noteStore, proc, booxStore, booxImporter, nil, searchIndex, scanner, noteDB, booxCachePath, booxNotesPath, logger)
	searchSvc := service.NewSearchService(searchIndex, retriever, embedder, embedStore, embedModel, chatStore, ragDisplay.ChatAPIURL, ragDisplay.ChatModel, logger)
	config := service.NewConfigService(noteDB, runningConfig)

	return NewHandler(tasks, noteSvc, searchSvc, config, noteDB, notesPathPrefix, booxNotesPath, logger, broadcaster)
}

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
	return nil, sql.ErrNoRows
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
func (m *mockTaskStore) DeleteCompleted(ctx context.Context) (int64, error) {
	var count int64
	for id, t := range m.tasks {
		if t.Status.Valid && t.Status.String == "completed" && t.IsDeleted == "N" {
			m.tasks[id].IsDeleted = "Y"
			count++
		}
	}
	return count, nil
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

func (m *mockNoteStore) GetHash(_ context.Context, _ string) (string, error) { return "", nil }

func (m *mockNoteStore) LookupByHash(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
}

func (m *mockNoteStore) TransferJob(_ context.Context, _, _ string) error { return nil }

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
func (m *mockSearchIndex) ListFolders(_ context.Context) ([]string, error) {
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
func (m *mockProcessor) Enqueue(_ context.Context, path string, _ ...processor.EnqueueOption) error {
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
	
	tasks := service.NewTaskService(store, nil)
	handler := NewHandler(tasks, nil, nil, nil, nil, "", "", logger, broadcaster)

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
	
	tasks := service.NewTaskService(store, nil)
	handler := NewHandler(tasks, nil, nil, nil, nil, "", "", logger, broadcaster)

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
	
	tasks := service.NewTaskService(store, notifier)
	handler := NewHandler(tasks, nil, nil, nil, nil, "", "", logger, broadcaster)

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

// TestEmptyStateRendersTaskTable verifies that GET / with zero tasks still
// renders the #task-table skeleton (including tbody) so that the create form's
// hx-target="#task-table tbody" swap works on the first-ever task creation.
// Prior to this structural fix, an empty task list rendered a bare
// .empty-state div with no table, so HTMX silently failed to swap the first
// created row into the DOM and the UI appeared to do nothing.
func TestEmptyStateRendersTaskTable(t *testing.T) {
	handler := newTestHandler()
	// newTestHandler's mockTaskService starts empty.
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET / returned %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="task-table"`) {
		t.Errorf("empty-state page missing #task-table; body:\n%s", body)
	}
	if !strings.Contains(body, `<tbody>`) {
		t.Errorf("empty-state page missing <tbody>; body:\n%s", body)
	}
	if !strings.Contains(body, `id="empty-state-row"`) {
		t.Errorf("empty-state page missing empty-state-row placeholder; body:\n%s", body)
	}
	if !strings.Contains(body, `No tasks yet`) {
		t.Errorf("empty-state page missing user-facing message; body:\n%s", body)
	}
}

// TestPostCreateTaskHXReturnsRow verifies AC1.6: an HX-Request POST to
// /tasks returns 200 with a single <tr id="task-{newID}"> fragment carrying
// the submitted title; non-HX behavior (redirect) covered by sibling tests.
func TestPostCreateTaskHXReturnsRow(t *testing.T) {
	store := newMockTaskStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()

	tasks := service.NewTaskService(store, nil)
	handler := NewHandler(tasks, nil, nil, nil, nil, "", "", logger, broadcaster)

	form := url.Values{}
	form.Set("title", "HX Task")
	req := httptest.NewRequest("POST", "/tasks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HX-Request POST returned %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `HX Task`) {
		t.Errorf("response missing submitted title; body:\n%s", body)
	}
	// Deterministic id comes from the TaskID generator; we only know the prefix.
	if !strings.Contains(body, `id="task-`) {
		t.Errorf("response missing id=\"task-…\" prefix; body:\n%s", body)
	}
	if strings.Contains(body, `<nav class="sidebar">`) {
		t.Errorf("response leaked layout shell; body:\n%s", body)
	}
}

// TestPostCreateTaskWithDueDate verifies POST /tasks with optional due date
func TestPostCreateTaskWithDueDate(t *testing.T) {
	store := newMockTaskStore()
	notifier := &mockNotifier{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	
	tasks := service.NewTaskService(store, notifier)
	handler := NewHandler(tasks, nil, nil, nil, nil, "", "", logger, broadcaster)

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
	
	tasks := service.NewTaskService(store, nil)
	handler := NewHandler(tasks, nil, nil, nil, nil, "", "", logger, broadcaster)

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
	
	tasks := service.NewTaskService(store, nil)
	handler := NewHandler(tasks, nil, nil, nil, nil, "", "", logger, broadcaster)

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
	
	tasks := service.NewTaskService(store, notifier)
	handler := NewHandler(tasks, nil, nil, nil, nil, "", "", logger, broadcaster)

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

// TestPostCompleteTaskHXReturnsRow verifies AC1.1 and AC1.3: an HX-Request
// to POST /tasks/{id}/complete returns 200 with a single <tr id="task-{id}">
// carrying data-status="completed" so the client-side toggleCompleted filter
// keeps working.
func TestPostCompleteTaskHXReturnsRow(t *testing.T) {
	store := newMockTaskStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()

	tasks := service.NewTaskService(store, nil)
	handler := NewHandler(tasks, nil, nil, nil, nil, "", "", logger, broadcaster)

	store.tasks["task-hx"] = &taskstore.Task{
		TaskID:    "task-hx",
		Title:     taskstore.SqlStr("Complete me"),
		Status:    taskstore.SqlStr("needsAction"),
		IsDeleted: "N",
	}

	req := httptest.NewRequest("POST", "/tasks/task-hx/complete", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HX-Request POST returned %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="task-task-hx"`) {
		t.Errorf("response missing id=\"task-task-hx\"; body:\n%s", body)
	}
	if !strings.Contains(body, `data-status="completed"`) {
		t.Errorf("response missing data-status=\"completed\"; body:\n%s", body)
	}
	if strings.Contains(body, `<nav class="sidebar">`) {
		t.Errorf("response leaked layout shell (should be a bare row fragment); body:\n%s", body)
	}
}

// TestPostCompleteTaskAlreadyCompleted verifies completing an already-completed task
func TestPostCompleteTaskAlreadyCompleted(t *testing.T) {
	store := newMockTaskStore()
	notifier := &mockNotifier{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := LegacyNewHandler(store, notifier, nil, nil, nil, nil, nil, nil, nil, "", "", nil, logger, broadcaster, nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

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
	handler := LegacyNewHandler(store, nil, nil, nil, nil, nil, nil, nil, nil, "", "", nil, logger, broadcaster, nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

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
	handler := LegacyNewHandler(store, nil, nil, nil, nil, nil, nil, nil, nil, "", "", nil, logger, broadcaster, nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

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
	handler := LegacyNewHandler(store, nil, nil, nil, nil, nil, nil, nil, nil, "", "", nil, logger, broadcaster, nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

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
	handler := LegacyNewHandler(store, nil, nil, nil, nil, nil, nil, nil, nil, "", "", nil, logger, broadcaster, nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

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

// TestBulkCompleteHXReturnsRowFragments verifies AC1.4: HX POST /tasks/bulk with
// action=complete and two task IDs returns 200 with one <tr> per task, concatenated.
func TestBulkCompleteHXReturnsRowFragments(t *testing.T) {
	store := newMockTaskStore()
	store.tasks["a"] = &taskstore.Task{TaskID: "a", Title: taskstore.SqlStr("A"), Status: taskstore.SqlStr("needsAction"), IsDeleted: "N"}
	store.tasks["b"] = &taskstore.Task{TaskID: "b", Title: taskstore.SqlStr("B"), Status: taskstore.SqlStr("needsAction"), IsDeleted: "N"}
	handler := LegacyNewHandler(store, nil, nil, nil, nil, nil, nil, nil, nil, "", "", nil, slog.Default(), logging.NewLogBroadcaster(), nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

	form := url.Values{}
	form.Set("action", "complete")
	form.Add("task_ids", "a")
	form.Add("task_ids", "b")
	req := httptest.NewRequest("POST", "/tasks/bulk", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HX bulk complete returned %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="task-a"`) {
		t.Errorf("response missing id=\"task-a\"; body:\n%s", body)
	}
	if !strings.Contains(body, `id="task-b"`) {
		t.Errorf("response missing id=\"task-b\"; body:\n%s", body)
	}
	if got := strings.Count(body, `data-status="completed"`); got != 2 {
		t.Errorf("response contains %d completed rows, want 2; body:\n%s", got, body)
	}
}

// TestBulkDeleteHXReturnsEmptyBody verifies AC1.5: HX POST /tasks/bulk with
// action=delete returns 200 with an empty body (client removes rows via JS).
func TestBulkDeleteHXReturnsEmptyBody(t *testing.T) {
	store := newMockTaskStore()
	store.tasks["a"] = &taskstore.Task{TaskID: "a", Title: taskstore.SqlStr("A"), IsDeleted: "N"}
	handler := LegacyNewHandler(store, nil, nil, nil, nil, nil, nil, nil, nil, "", "", nil, slog.Default(), logging.NewLogBroadcaster(), nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

	form := url.Values{}
	form.Set("action", "delete")
	form.Add("task_ids", "a")
	req := httptest.NewRequest("POST", "/tasks/bulk", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HX bulk delete returned %d, want %d", w.Code, http.StatusOK)
	}
	if body := w.Body.String(); body != "" {
		t.Errorf("expected empty body, got %q", body)
	}
	if store.tasks["a"].IsDeleted != "Y" {
		t.Errorf("task a should be soft-deleted")
	}
}

// TestPurgeCompletedHXReturnsEmptyBody verifies AC1.7: HX POST /tasks/purge-completed
// returns 200 with an empty body; client-side JS sweeps completed rows from the DOM.
func TestPurgeCompletedHXReturnsEmptyBody(t *testing.T) {
	store := newMockTaskStore()
	store.tasks["done"] = &taskstore.Task{TaskID: "done", Title: taskstore.SqlStr("Done"), Status: taskstore.SqlStr("completed"), IsDeleted: "N"}
	store.tasks["open"] = &taskstore.Task{TaskID: "open", Title: taskstore.SqlStr("Open"), Status: taskstore.SqlStr("needsAction"), IsDeleted: "N"}
	handler := LegacyNewHandler(store, nil, nil, nil, nil, nil, nil, nil, nil, "", "", nil, slog.Default(), logging.NewLogBroadcaster(), nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

	req := httptest.NewRequest("POST", "/tasks/purge-completed", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HX purge returned %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if body := w.Body.String(); body != "" {
		t.Errorf("expected empty body, got %q", body)
	}
	if store.tasks["done"].IsDeleted != "Y" {
		t.Error("completed task should be purged (IsDeleted='Y')")
	}
	if store.tasks["open"].IsDeleted != "N" {
		t.Error("non-completed task should remain")
	}
}

// TestPurgeCompletedNonHXRedirects verifies the non-HX path still redirects to /.
func TestPurgeCompletedNonHXRedirects(t *testing.T) {
	store := newMockTaskStore()
	handler := LegacyNewHandler(store, nil, nil, nil, nil, nil, nil, nil, nil, "", "", nil, slog.Default(), logging.NewLogBroadcaster(), nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

	req := httptest.NewRequest("POST", "/tasks/purge-completed", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("non-HX purge returned %d, want %d", w.Code, http.StatusSeeOther)
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Errorf("redirect location is %q, want /", loc)
	}
}

func TestBulkCompleteMultipleTasks(t *testing.T) {
	store := newMockTaskStore()
	store.tasks["t1"] = &taskstore.Task{TaskID: "t1", Title: taskstore.SqlStr("Task 1"), Status: taskstore.SqlStr("needsAction"), IsDeleted: "N"}
	store.tasks["t2"] = &taskstore.Task{TaskID: "t2", Title: taskstore.SqlStr("Task 2"), Status: taskstore.SqlStr("needsAction"), IsDeleted: "N"}
	store.tasks["t3"] = &taskstore.Task{TaskID: "t3", Title: taskstore.SqlStr("Task 3"), Status: taskstore.SqlStr("needsAction"), IsDeleted: "N"}
	handler := LegacyNewHandler(store, nil, nil, nil, nil, nil, nil, nil, nil, "", "", nil, slog.Default(), logging.NewLogBroadcaster(), nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

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
	handler := LegacyNewHandler(store, nil, nil, nil, nil, nil, nil, nil, nil, "", "", nil, slog.Default(), logging.NewLogBroadcaster(), nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

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
	handler := LegacyNewHandler(store, nil, nil, nil, nil, nil, nil, nil, nil, "", "", nil, slog.Default(), logging.NewLogBroadcaster(), nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

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
	handler := LegacyNewHandler(store, nil, nil, nil, nil, nil, nil, nil, nil, "", "", nil, slog.Default(), logging.NewLogBroadcaster(), nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

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

// TestHandleFiles_NoteStoreNil verifies AC1.6: missing Supernote source renders error, not crash
func TestHandleFiles_NoteStoreNil(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	notes := &mockNoteService{pipelineConfigured: false}
	handler := NewHandler(nil, notes, nil, nil, nil, "", "", logger, broadcaster)

	req := httptest.NewRequest("GET", "/files/supernote", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "No Supernote source configured") {
		t.Error("expected 'No Supernote source configured' error message in response")
	}
}

// TestHandleFiles_PathTraversal verifies that traversal attempts on the
// Supernote tab return 400. The legacy /files route is now a redirect,
// so traversal checks happen on the destination handler.
func TestHandleFiles_PathTraversal(t *testing.T) {
	ns := newMockNoteStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := LegacyNewHandler(newMockTaskStore(), nil, ns, nil, nil, nil, nil, nil, nil, "", "", nil, logger, broadcaster, nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

	for _, badPath := range []string{"../../etc", "../secrets", "/etc/passwd"} {
		req := httptest.NewRequest("GET", "/files/supernote?path="+badPath, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("path %q: status = %d, want 400", badPath, w.Code)
		}
	}
}

// TestHandleFiles_TopLevel verifies Supernote-tab rendering of top-level files.
func TestHandleFiles_TopLevel(t *testing.T) {
	handler := newTestHandler()
	notes := handler.notes.(*mockNoteService)
	notes.files = []service.NoteFile{
		{Name: "test.note", FileType: "note", RelPath: "test.note", Source: "supernote"},
		{Name: "readme.pdf", FileType: "pdf", RelPath: "readme.pdf", Source: "supernote"},
	}

	req := httptest.NewRequest("GET", "/files/supernote", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "test.note") {
		t.Error("expected test.note in response")
	}
	if !strings.Contains(body, "badge badge-unprocessed") {
		t.Error("expected unprocessed badge for .note file")
	}
	if !strings.Contains(body, "badge badge-unsupported") {
		t.Error("expected unsupported badge for pdf")
	}
}

// TestHandleFiles_WithPath verifies Supernote-tab subdirectory navigation.
func TestHandleFiles_WithPath(t *testing.T) {
	handler := newTestHandler()
	notes := handler.notes.(*mockNoteService)
	notes.files = []service.NoteFile{
		{Name: "deep.note", FileType: "note", RelPath: "Note/Folder/deep.note", Source: "supernote"},
	}

	req := httptest.NewRequest("GET", "/files/supernote?path=Note/Folder", nil)
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

// TestHandleFilesRedirect verifies the legacy /files route redirects to the
// right tab based on configured sources, preserving any query string.
func TestHandleFilesRedirect(t *testing.T) {
	t.Run("supernote_configured", func(t *testing.T) {
		h := newTestHandler()
		h.notes.(*mockNoteService).pipelineConfigured = true
		h.notes.(*mockNoteService).booxEnabled = false

		req := httptest.NewRequest(http.MethodGet, "/files?path=Personal", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusSeeOther {
			t.Fatalf("status=%d, want 303", w.Code)
		}
		if loc := w.Header().Get("Location"); loc != "/files/supernote?path=Personal" {
			t.Errorf("Location=%q", loc)
		}
	})

	t.Run("boox_only", func(t *testing.T) {
		h := newTestHandler()
		h.notes.(*mockNoteService).pipelineConfigured = false
		h.notes.(*mockNoteService).booxEnabled = true

		req := httptest.NewRequest(http.MethodGet, "/files", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusSeeOther {
			t.Fatalf("status=%d, want 303", w.Code)
		}
		if loc := w.Header().Get("Location"); loc != "/files/boox" {
			t.Errorf("Location=%q", loc)
		}
	})

	t.Run("neither_renders_placeholder", func(t *testing.T) {
		h := newTestHandler()
		h.notes.(*mockNoteService).pipelineConfigured = false
		h.notes.(*mockNoteService).booxEnabled = false

		req := httptest.NewRequest(http.MethodGet, "/files", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200", w.Code)
		}
		if !strings.Contains(w.Body.String(), "No note sources configured") {
			t.Errorf("expected placeholder message; body:\n%s", w.Body.String())
		}
	})
}

// Helper functions for C&C tests
func makeFilesHandler(t *testing.T, proc *mockProcessor) *Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	return LegacyNewHandler(newMockTaskStore(), nil, nil, nil, proc, nil, nil, nil, nil, "", "", nil, logger, broadcaster, nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})
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

// mockScanner implements FileScanner for testing.
type mockScanner struct {
	called int
}

func (m *mockScanner) ScanNow(_ context.Context) {
	m.called++
}

// TestHandleFilesScan verifies POST /files/scan triggers a filesystem scan.
func TestHandleFilesScan(t *testing.T) {
	scanner := &mockScanner{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := LegacyNewHandler(newMockTaskStore(), nil, nil, nil, nil, scanner, nil, nil, nil, "", "", nil, logger, broadcaster, nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

	req := httptest.NewRequest("POST", "/files/scan", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", w.Code)
	}
	if scanner.called != 1 {
		t.Errorf("scanner called %d times, want 1", scanner.called)
	}
}

// TestHandleFilesScan_NilScanner verifies POST /files/scan doesn't crash when scanner is nil.
func TestHandleFilesScan_NilScanner(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := LegacyNewHandler(newMockTaskStore(), nil, nil, nil, nil, nil, nil, nil, nil, "", "", nil, logger, broadcaster, nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

	req := httptest.NewRequest("POST", "/files/scan", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", w.Code)
	}
}

// mockEmbedder is a simple embedder for testing
type mockEmbedder struct {
	embedFn func(ctx context.Context, text string) ([]float32, error)
}

func (m *mockEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if m.embedFn != nil {
		return m.embedFn(ctx, text)
	}
	return []float32{0.1, 0.2, 0.3}, nil
}

// TestHandleBackfillEmbeddings verifies POST /settings/backfill-embeddings returns 303 redirect.
func TestHandleBackfillEmbeddings(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("Open test db: %v", err)
	}
	defer db.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()

	embedder := &mockEmbedder{}
	embedStore := rag.NewStore(db, logger)

	// Create handler with embedder and embedStore
	handler := LegacyNewHandler(newMockTaskStore(), nil, nil, nil, nil, nil, nil, nil, nil, "", "", nil, logger, broadcaster, embedder, embedStore, "test-model", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

	req := httptest.NewRequest("POST", "/settings/backfill-embeddings", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Should return 303 See Other redirect
	if w.Code != http.StatusSeeOther {
		t.Errorf("POST /settings/backfill-embeddings status = %d, want %d", w.Code, http.StatusSeeOther)
	}

	// Should redirect to /settings
	location := w.Header().Get("Location")
	if location != "/settings" {
		t.Errorf("redirect location = %q, want /settings", location)
	}
}

func TestHandleBackfillEmbeddings_NotRegisteredWhenDisabled(t *testing.T) {
	handler := newTestHandler()
	handler.search.(*mockSearchService).embeddingPipelineConfigured = false

	req := httptest.NewRequest("POST", "/settings/backfill-embeddings", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Should return 404 since route is guarded
	if w.Code != http.StatusNotFound {
		t.Errorf("POST /settings/backfill-embeddings status = %d, want 404", w.Code)
	}
}

// TestHandleMCPTokenCreate_Success verifies AC3.1: POST to create token displays raw token once
func TestHandleMCPTokenCreate_Success(t *testing.T) {
	ctx := context.Background()
	testDB, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("notedb open failed: %v", err)
	}
	defer testDB.Close()

	// Ensure mcp_tokens table exists
	if err := mcpauth.Migrate(ctx, testDB); err != nil {
		t.Fatalf("mcpauth migrate failed: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	config := service.NewConfigService(testDB, &appconfig.Config{})
	notes := &mockNoteService{}
	handler := NewHandler(nil, notes, nil, config, testDB, "", "", logger, broadcaster)

	// POST to create token
	form := url.Values{}
	form.Set("label", "test-client")
	req := httptest.NewRequest("POST", "/settings/mcp-tokens/create", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Should redirect with 303 to /settings?new_token=...#mcp-tokens
	if w.Code != http.StatusSeeOther {
		t.Errorf("POST /settings/mcp-tokens/create status = %d, want 303", w.Code)
	}

	location := w.Header().Get("Location")
	if !strings.Contains(location, "/settings?new_token=") || !strings.Contains(location, "#mcp-tokens") {
		t.Errorf("unexpected redirect location: %s", location)
	}

	// Extract the raw token from the query param
	u, err := url.Parse(location)
	if err != nil {
		t.Fatalf("failed to parse redirect location: %v", err)
	}
	rawToken := u.Query().Get("new_token")
	if rawToken == "" {
		t.Fatalf("new_token query param not found in redirect")
	}

	// Verify token is 43 chars (URL-safe base64 of 32 bytes)
	if len(rawToken) != 43 {
		t.Errorf("raw token length = %d, want 43 (base64url of 32 bytes)", len(rawToken))
	}

	// Verify the token is valid by validating it
	label, err := mcpauth.ValidateToken(ctx, testDB, rawToken)
	if err != nil {
		t.Errorf("ValidateToken failed: %v", err)
	}
	if label != "test-client" {
		t.Errorf("token label = %s, want test-client", label)
	}

	// Verify the token appears in settings page when flash param is provided
	req2 := httptest.NewRequest("GET", "/settings?new_token="+url.QueryEscape(rawToken), nil)
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Errorf("GET /settings?new_token=... status = %d, want 200", w2.Code)
	}

	// Verify the raw token is in the response HTML
	body := w2.Body.String()
	if !strings.Contains(body, rawToken) {
		t.Errorf("raw token not found in settings response body")
	}
}

// TestHandleSettings_TokenList verifies AC3.2: token list shows label, hash prefix, dates
func TestHandleSettings_TokenList(t *testing.T) {
	ctx := context.Background()
	testDB, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("notedb open failed: %v", err)
	}
	defer testDB.Close()

	// Ensure mcp_tokens table exists and create two tokens
	if err := mcpauth.Migrate(ctx, testDB); err != nil {
		t.Fatalf("mcpauth migrate failed: %v", err)
	}

	rawToken1, hash1, err := mcpauth.CreateToken(ctx, testDB, "token-1")
	if err != nil {
		t.Fatalf("CreateToken failed: %v", err)
	}

	rawToken2, hash2, err := mcpauth.CreateToken(ctx, testDB, "token-2")
	if err != nil {
		t.Fatalf("CreateToken failed: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	config := service.NewConfigService(testDB, &appconfig.Config{})
	notes := &mockNoteService{}
	handler := NewHandler(nil, notes, nil, config, testDB, "", "", logger, broadcaster)

	// GET /settings
	req := httptest.NewRequest("GET", "/settings", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /settings status = %d, want 200", w.Code)
	}

	body := w.Body.String()

	// Verify both labels are present
	if !strings.Contains(body, "token-1") {
		t.Errorf("token label 'token-1' not found in response")
	}
	if !strings.Contains(body, "token-2") {
		t.Errorf("token label 'token-2' not found in response")
	}

	// Verify truncated hashes (first 8 chars) are present
	hash1Prefix := hash1[:8]
	hash2Prefix := hash2[:8]
	if !strings.Contains(body, hash1Prefix) {
		t.Errorf("token hash prefix for token-1 (%s) not found in response", hash1Prefix)
	}
	if !strings.Contains(body, hash2Prefix) {
		t.Errorf("token hash prefix for token-2 (%s) not found in response", hash2Prefix)
	}

	// Verify "Never" appears for last_used (since tokens haven't been validated)
	// Count occurrences — should have at least 2 for the two tokens
	neverCount := strings.Count(body, "Never")
	if neverCount < 2 {
		t.Errorf("'Never' appears %d times, want at least 2 for last_used dates", neverCount)
	}

	_ = rawToken1
	_ = rawToken2
}

// TestHandleMCPTokenRevoke_Success verifies AC3.3: revoke removes token and invalidates it
func TestHandleMCPTokenRevoke_Success(t *testing.T) {
	ctx := context.Background()
	testDB, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("notedb open failed: %v", err)
	}
	defer testDB.Close()

	// Ensure mcp_tokens table exists
	if err := mcpauth.Migrate(ctx, testDB); err != nil {
		t.Fatalf("mcpauth migrate failed: %v", err)
	}

	// Create a token
	rawToken, tokenHash, err := mcpauth.CreateToken(ctx, testDB, "test-revoke")
	if err != nil {
		t.Fatalf("CreateToken failed: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	config := service.NewConfigService(testDB, &appconfig.Config{})
	notes := &mockNoteService{}
	handler := NewHandler(nil, notes, nil, config, testDB, "", "", logger, broadcaster)

	// POST to revoke token
	form := url.Values{}
	form.Set("token_hash", tokenHash)
	req := httptest.NewRequest("POST", "/settings/mcp-tokens/revoke", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Should redirect with 303 to /settings#mcp-tokens
	if w.Code != http.StatusSeeOther {
		t.Errorf("POST /settings/mcp-tokens/revoke status = %d, want 303", w.Code)
	}

	location := w.Header().Get("Location")
	if location != "/settings#mcp-tokens" {
		t.Errorf("unexpected redirect location: %s, want /settings#mcp-tokens", location)
	}

	// Verify the token is now invalid
	_, err = mcpauth.ValidateToken(ctx, testDB, rawToken)
	if !errors.Is(err, mcpauth.ErrInvalidToken) {
		t.Errorf("ValidateToken after revoke returned %v, want ErrInvalidToken", err)
	}
}

// TestHandleMCPTokenCreate_EmptyLabel verifies AC3.4: empty label is rejected
func TestHandleMCPTokenCreate_EmptyLabel(t *testing.T) {
	ctx := context.Background()
	testDB, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("notedb open failed: %v", err)
	}
	defer testDB.Close()

	// Ensure mcp_tokens table exists
	if err := mcpauth.Migrate(ctx, testDB); err != nil {
		t.Fatalf("mcpauth migrate failed: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	config := service.NewConfigService(testDB, &appconfig.Config{})
	notes := &mockNoteService{}
	handler := NewHandler(nil, notes, nil, config, testDB, "", "", logger, broadcaster)

	// POST with empty label
	form := url.Values{}
	form.Set("label", "")
	req := httptest.NewRequest("POST", "/settings/mcp-tokens/create", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Should return 400
	if w.Code != http.StatusBadRequest {
		t.Errorf("POST with empty label status = %d, want 400", w.Code)
	}

	// Verify error message
	body := w.Body.String()
	if !strings.Contains(body, "token label is required") {
		t.Errorf("error message not found in response: %s", body)
	}

	// Also test with whitespace-only label
	form2 := url.Values{}
	form2.Set("label", "   ")
	req2 := httptest.NewRequest("POST", "/settings/mcp-tokens/create", strings.NewReader(form2.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusBadRequest {
		t.Errorf("POST with whitespace-only label status = %d, want 400", w2.Code)
	}
}

// TestRenderFragmentAC41 verifies htmx-fragment-mutations.AC4.1: renderFragment
// renders a named template block without the layout shell. Uses the real
// _task_row fragment (introduced in Phase 2) as the probe; the earlier in-test
// _test_fixture_row bootstrap was removed in Phase 6.
// AC4.3 (fragments loaded via the existing embed.FS) is demonstrated by the
// same test — _task_row is picked up via ParseFS in NewHandler, no new
// filesystem plumbing.
func TestRenderFragmentAC41(t *testing.T) {
	h := newTestHandler()
	task := service.Task{
		ID:        "phase6-probe",
		Title:     "Phase-6 regression fixture",
		Status:    service.StatusNeedsAction,
		CreatedAt: time.Now().UTC(),
	}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.renderFragment(w, r, "_task_row", task)

	body := w.Body.String()
	if !strings.Contains(body, `id="task-phase6-probe"`) {
		t.Errorf("response missing id=\"task-phase6-probe\"; body:\n%s", body)
	}
	if !strings.Contains(body, `Phase-6 regression fixture`) {
		t.Errorf("response missing title text; body:\n%s", body)
	}
	if strings.Contains(body, `<nav class="sidebar">`) {
		t.Errorf("response leaked layout shell; body:\n%s", body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html…", ct)
	}
}

// TestRenderTemplate verifies AC4.2: renderTemplate continues to branch on HX-Request
// without regression. Tests the tasks tab both with and without HX-Request.
func TestRenderTemplate(t *testing.T) {
	t.Run("without HX-Request includes layout", func(t *testing.T) {
		h := newTestHandler()
		data := map[string]interface{}{
			"tasks": []service.Task{},
		}

		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/", nil)
		// No HX-Request header

		h.renderTemplate(w, r, "tasks", data)

		// Layout marker: should be present when NOT an HTMX request
		body := w.Body.String()
		if !strings.Contains(body, `<nav class="sidebar">`) {
			t.Errorf("Without HX-Request, response should contain layout marker '<nav class=\"sidebar\">', got:\n%s", body)
		}

		// Task tab marker: should still be present
		if !strings.Contains(body, `Create New Task`) {
			t.Errorf("Without HX-Request, response should contain task tab marker 'Create New Task', got:\n%s", body)
		}
	})

	t.Run("with HX-Request omits layout", func(t *testing.T) {
		h := newTestHandler()
		data := map[string]interface{}{
			"tasks": []service.Task{},
		}

		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("HX-Request", "true")

		h.renderTemplate(w, r, "tasks", data)

		body := w.Body.String()

		// Layout marker: should NOT be present when HX-Request is true
		if strings.Contains(body, `<nav class="sidebar">`) {
			t.Errorf("With HX-Request: true, response should NOT contain layout marker '<nav class=\"sidebar\">', got:\n%s", body)
		}

		// Task tab marker: should still be present
		if !strings.Contains(body, `Create New Task`) {
			t.Errorf("With HX-Request: true, response should contain task tab marker 'Create New Task', got:\n%s", body)
		}
	})
}

// TestTaskRowFragmentIdentity verifies htmx-fragment-mutations.AC3.3: a task rendered
// via renderFragment("_task_row", task) produces the same load-bearing HTML tokens as
// the same task rendered inside tasks.html via {{range .tasks}}{{template "_task_row" .}}{{end}}.
// Substring equivalence on the load-bearing tokens is sufficient — whitespace normalization
// by html/template may differ between the two paths.
func TestTaskRowFragmentIdentity(t *testing.T) {
	h := newTestHandler()
	task := service.Task{ID: "abc", Title: "Fixture Row", Status: service.StatusNeedsAction}
	h.tasks.(*mockTaskService).tasks = []service.Task{task}

	// Path 1: renderFragment directly.
	w1 := httptest.NewRecorder()
	r1 := httptest.NewRequest(http.MethodGet, "/", nil)
	h.renderFragment(w1, r1, "_task_row", task)
	body1 := w1.Body.String()

	// Path 2: HTMX GET / — handleIndex renders tasks.html which invokes
	// {{template "_task_row" .}} inside {{range .tasks}}.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.Header.Set("HX-Request", "true")
	h.ServeHTTP(w2, r2)
	body2 := w2.Body.String()

	markers := []string{
		`id="task-abc"`,
		`data-status="needsAction"`,
		`Fixture Row`,
		`hx-post="/tasks/abc/complete"`,
	}
	for _, m := range markers {
		if !strings.Contains(body1, m) {
			t.Errorf("renderFragment output missing marker %q\nbody:\n%s", m, body1)
		}
		if !strings.Contains(body2, m) {
			t.Errorf("tasks.html render missing marker %q\nbody:\n%s", m, body2)
		}
	}
}

// TestFileRowFragmentIdentity verifies AC3.4 post-split: rows rendered inside
// the tab template (files_supernote.html / files_boox.html) carry the same
// load-bearing HTML tokens as rows rendered via h.renderFragment after a
// mutation. Ensures a swapped row matches the shape of the row it replaces.
func TestFileRowFragmentIdentity(t *testing.T) {
	// fileRowID formula from handler.go — kept in-test to avoid exporting.
	fileRowID := func(path string) string {
		sum := sha1.Sum([]byte(path))
		return "file-" + hex.EncodeToString(sum[:])[:12]
	}

	t.Run("supernote", func(t *testing.T) {
		file := service.NoteFile{
			Path:      "/notes/foo.note",
			RelPath:   "foo.note",
			Name:      "foo.note",
			FileType:  "note",
			JobStatus: "done",
			Source:    "supernote",
		}

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		broadcaster := logging.NewLogBroadcaster()
		notes := &mockNoteService{
			files:              []service.NoteFile{file},
			docs:               make(map[string][]service.SearchResult),
			contents:           make(map[string]interface{}),
			pipelineConfigured: true,
		}
		h := NewHandler(
			&mockTaskService{}, notes,
			&mockSearchService{}, &mockConfigService{},
			nil, "", "", logger, broadcaster,
		)

		// Path 1: render through files_supernote.html via HX GET /files/supernote.
		w1 := httptest.NewRecorder()
		r1 := httptest.NewRequest(http.MethodGet, "/files/supernote", nil)
		r1.Header.Set("HX-Request", "true")
		h.ServeHTTP(w1, r1)
		if w1.Code != http.StatusOK {
			t.Fatalf("GET /files/supernote returned %d; body=%s", w1.Code, w1.Body.String())
		}
		body1 := w1.Body.String()

		// Path 2: renderFragment directly with the same ctx shape.
		w2 := httptest.NewRecorder()
		r2 := httptest.NewRequest(http.MethodGet, "/", nil)
		h.renderFragment(w2, r2, "_sn_file_row", fileRowCtx{File: file, RelPath: ""})
		body2 := w2.Body.String()

		expectedID := fileRowID(file.Path)
		markers := []string{
			`id="` + expectedID + `"`,
			file.Name,
			`hx-target="closest tr"`,
		}
		for _, m := range markers {
			if !strings.Contains(body1, m) {
				t.Errorf("files_supernote.html render missing marker %q; body:\n%s", m, body1)
			}
			if !strings.Contains(body2, m) {
				t.Errorf("_sn_file_row renderFragment missing marker %q; body:\n%s", m, body2)
			}
		}
	})

	t.Run("boox", func(t *testing.T) {
		summary := service.BooxNoteSummary{
			Path:      "/boox/bar.note",
			Filename:  "bar.note",
			Title:     "Bar Notes",
			Folder:    "Personal",
			JobStatus: "done",
		}

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		broadcaster := logging.NewLogBroadcaster()
		notes := &mockNoteService{
			booxNotes:   []service.BooxNoteSummary{summary},
			docs:        make(map[string][]service.SearchResult),
			contents:    make(map[string]interface{}),
			booxEnabled: true,
		}
		h := NewHandler(
			&mockTaskService{}, notes,
			&mockSearchService{}, &mockConfigService{},
			nil, "", "/boox", logger, broadcaster,
		)

		// Path 1: render through files_boox.html via HX GET /files/boox.
		w1 := httptest.NewRecorder()
		r1 := httptest.NewRequest(http.MethodGet, "/files/boox", nil)
		r1.Header.Set("HX-Request", "true")
		h.ServeHTTP(w1, r1)
		if w1.Code != http.StatusOK {
			t.Fatalf("GET /files/boox returned %d; body=%s", w1.Code, w1.Body.String())
		}
		body1 := w1.Body.String()

		// Path 2: renderFragment directly with the same BooxNoteSummary.
		w2 := httptest.NewRecorder()
		r2 := httptest.NewRequest(http.MethodGet, "/", nil)
		h.renderFragment(w2, r2, "_boox_file_row", summary)
		body2 := w2.Body.String()

		expectedID := fileRowID(summary.Path)
		markers := []string{
			`id="` + expectedID + `"`,
			summary.Title,
			summary.Folder,
			`hx-target="closest tr"`,
		}
		for _, m := range markers {
			if !strings.Contains(body1, m) {
				t.Errorf("files_boox.html render missing marker %q; body:\n%s", m, body1)
			}
			if !strings.Contains(body2, m) {
				t.Errorf("_boox_file_row renderFragment missing marker %q; body:\n%s", m, body2)
			}
		}
	})
}

// fileRowIDFor is a test-only helper mirroring handler.go's fileRowID FuncMap
// entry. Duplicated deliberately; if the production formula changes, these
// tests will fail and force the update.
func fileRowIDFor(path string) string {
	sum := sha1.Sum([]byte(path))
	return "file-" + hex.EncodeToString(sum[:])[:12]
}

// TestHandleFilesSingleRowMutations covers AC2.1 (HX fragment) and AC2.5
// (non-HX redirect preserving back) for the four single-row file handlers.
// The mockNoteService's Enqueue/Skip/Unskip mutate JobStatus on the matching
// file, so the subsequent GetFile returns the post-mutation state and the
// fragment reflects the correct badge class.
func TestHandleFilesSingleRowMutations(t *testing.T) {
	type mutationCase struct {
		name       string
		endpoint   string
		initialJob string
		wantBadge  string
	}
	cases := []mutationCase{
		{name: "queue", endpoint: "/files/queue", initialJob: "done", wantBadge: `badge-pending`},
		{name: "skip", endpoint: "/files/skip", initialJob: "", wantBadge: `badge-skipped`},
		{name: "unskip", endpoint: "/files/unskip", initialJob: "skipped", wantBadge: `badge-unprocessed`},
		{name: "force", endpoint: "/files/force", initialJob: "done", wantBadge: `badge-pending`},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/HX_returns_fragment", func(t *testing.T) {
			h := newTestHandler()
			filePath := "/notes/foo.note"
			h.notes.(*mockNoteService).files = []service.NoteFile{
				{Path: filePath, Name: "foo.note", FileType: "note", JobStatus: tc.initialJob, Source: "supernote"},
			}

			form := url.Values{}
			form.Set("path", filePath)
			form.Set("back", "subdir")
			req := httptest.NewRequest("POST", tc.endpoint, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("HX-Request", "true")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("HX %s returned %d, want 200; body=%s", tc.endpoint, w.Code, w.Body.String())
			}
			body := w.Body.String()
			wantID := `id="` + fileRowIDFor(filePath) + `"`
			if !strings.Contains(body, wantID) {
				t.Errorf("response missing %s; body:\n%s", wantID, body)
			}
			if !strings.Contains(body, tc.wantBadge) {
				t.Errorf("response missing badge %s; body:\n%s", tc.wantBadge, body)
			}
			if strings.Contains(body, `<nav class="sidebar">`) {
				t.Errorf("response leaked layout shell; body:\n%s", body)
			}
		})

		t.Run(tc.name+"/non-HX_redirects_preserving_back", func(t *testing.T) {
			h := newTestHandler()
			h.notes.(*mockNoteService).files = []service.NoteFile{
				{Path: "/notes/foo.note", Name: "foo.note", FileType: "note", JobStatus: tc.initialJob, Source: "supernote"},
			}

			form := url.Values{}
			form.Set("path", "/notes/foo.note")
			form.Set("back", "sub/dir with space")
			req := httptest.NewRequest("POST", tc.endpoint, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			// no HX-Request header
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != http.StatusSeeOther {
				t.Fatalf("non-HX %s returned %d, want 303; body=%s", tc.endpoint, w.Code, w.Body.String())
			}
			loc := w.Header().Get("Location")
			want := "/files/supernote?path=" + url.QueryEscape("sub/dir with space")
			if loc != want {
				t.Errorf("Location=%q, want %q", loc, want)
			}
		})
	}
}

// TestHandleBroadFileMutations verifies the broad file-mutation handlers
// return 200 OK + empty body on HX-Request and 303 redirect to the
// source-appropriate tab on non-HX. Each endpoint is source-specific, so
// its redirect target is now the tab that owns it (scan → supernote,
// import/migrate/retry → boox, processor start/stop split by source).
func TestHandleBroadFileMutations(t *testing.T) {
	endpoints := []struct {
		path     string
		wantLoc  string
	}{
		{"/files/scan", "/files/supernote"},
		{"/files/import", "/files/boox"},
		{"/files/retry-failed", "/files/boox"},
		{"/files/migrate-imports", "/files/boox"},
		{"/processor/supernote/start", "/files/supernote"},
		{"/processor/supernote/stop", "/files/supernote"},
		{"/processor/boox/start", "/files/boox"},
		{"/processor/boox/stop", "/files/boox"},
	}
	for _, tc := range endpoints {
		t.Run(tc.path+"/HX_empty_body", func(t *testing.T) {
			h := newTestHandler()
			req := httptest.NewRequest("POST", tc.path, nil)
			req.Header.Set("HX-Request", "true")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("HX %s returned %d, want 200; body=%q", tc.path, w.Code, w.Body.String())
			}
			if body := w.Body.String(); body != "" {
				t.Errorf("%s expected empty body, got %q", tc.path, body)
			}
		})
		t.Run(tc.path+"/non-HX_redirects", func(t *testing.T) {
			h := newTestHandler()
			req := httptest.NewRequest("POST", tc.path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusSeeOther {
				t.Fatalf("non-HX %s returned %d, want 303", tc.path, w.Code)
			}
			if loc := w.Header().Get("Location"); loc != tc.wantLoc {
				t.Errorf("%s Location=%q, want %q", tc.path, loc, tc.wantLoc)
			}
		})
	}
}

// TestBooxProcessorControls verifies the new Boox-specific start/stop
// endpoints call through to the service and that the mock tracks state
// independently from the Supernote processor.
func TestBooxProcessorControls(t *testing.T) {
	h := newTestHandler()
	notes := h.notes.(*mockNoteService)

	// Start Boox processor
	req := httptest.NewRequest("POST", "/processor/boox/start", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("start returned %d", w.Code)
	}
	if !notes.booxProcessorStarted {
		t.Errorf("booxProcessorStarted should be true after /processor/boox/start")
	}
	if notes.processorStarted {
		t.Errorf("supernote processorStarted should remain false")
	}

	// Stop Boox processor
	req = httptest.NewRequest("POST", "/processor/boox/stop", nil)
	req.Header.Set("HX-Request", "true")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("stop returned %d", w.Code)
	}
	if notes.booxProcessorStarted {
		t.Errorf("booxProcessorStarted should be false after /processor/boox/stop")
	}
}

// TestHandleFilesDeleteNoteHXEmptyBody verifies AC2.2: HX-Request POST
// /files/delete-note calls the service DeleteNote and returns 200 OK with
// empty body; client-side JS removes the row from the DOM.
func TestHandleFilesDeleteNoteHXEmptyBody(t *testing.T) {
	h := newTestHandler()
	notes := h.notes.(*mockNoteService)
	notes.files = []service.NoteFile{{Path: "/notes/foo.note", Name: "foo.note"}}

	form := url.Values{}
	form.Set("path", "/notes/foo.note")
	req := httptest.NewRequest("POST", "/files/delete-note", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HX delete-note returned %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if body := w.Body.String(); body != "" {
		t.Errorf("expected empty body, got %q", body)
	}
	if len(notes.deletedPaths) != 1 || notes.deletedPaths[0] != "/notes/foo.note" {
		t.Errorf("expected DeleteNote called with /notes/foo.note, got %v", notes.deletedPaths)
	}
}

// TestHandleFilesDeleteBulkHXEmptyBody verifies AC2.3: HX-Request POST
// /files/delete-bulk with multiple paths returns 200 + empty body and the
// service BulkDelete was invoked with the posted paths.
func TestHandleFilesDeleteBulkHXEmptyBody(t *testing.T) {
	h := newTestHandler()
	notes := h.notes.(*mockNoteService)

	form := url.Values{}
	form.Add("paths", "/notes/a.note")
	form.Add("paths", "/notes/b.note")
	req := httptest.NewRequest("POST", "/files/delete-bulk", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HX delete-bulk returned %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if body := w.Body.String(); body != "" {
		t.Errorf("expected empty body, got %q", body)
	}
	wantDeleted := []string{"/notes/a.note", "/notes/b.note"}
	if len(notes.deletedPaths) != 2 || notes.deletedPaths[0] != wantDeleted[0] || notes.deletedPaths[1] != wantDeleted[1] {
		t.Errorf("expected BulkDelete called with %v, got %v", wantDeleted, notes.deletedPaths)
	}
}

// TestHandlerRenderPathsDoNotPoisonEachOther is a regression guard for cross-path
// state coupling on h.tmpl. html/template permanently locks a tree against Clone
// once ExecuteTemplate has run, so any method that executes h.tmpl directly would
// brick every subsequent Clone-based render. This test exercises both paths in
// sequence on one Handler; a regression surfaces as a 5xx body or a non-HTML
// response on any call past the first.
func TestHandlerRenderPathsDoNotPoisonEachOther(t *testing.T) {
	h := newTestHandler()
	h.tasks.(*mockTaskService).tasks = []service.Task{
		{ID: "x", Title: "x task", Status: service.StatusNeedsAction},
	}
	task := service.Task{ID: "x", Title: "x task", Status: service.StatusNeedsAction}

	renderTab := func() string {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("HX-Request", "true")
		h.ServeHTTP(w, r)
		return w.Body.String()
	}
	renderRow := func() string {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		h.renderFragment(w, r, "_task_row", task)
		return w.Body.String()
	}

	steps := []struct {
		name string
		call func() string
	}{
		{"tab-1", renderTab},
		{"row-1", renderRow},
		{"tab-2-after-row", renderTab},
		{"row-2", renderRow},
		{"tab-3-after-row", renderTab},
	}
	for _, s := range steps {
		body := s.call()
		if strings.Contains(body, "internal error") || strings.Contains(body, "template error") || strings.Contains(body, "template not found") {
			t.Fatalf("step %s produced error body:\n%s", s.name, body)
		}
		if !strings.Contains(body, "x task") {
			t.Errorf("step %s missing task content; body:\n%s", s.name, body)
		}
	}
}

// TestHandleFilesSupernoteFiltersBoox verifies the /files/supernote route
// renders only Supernote files — Boox-sourced files from the mock are
// filtered out by ListSupernoteFiles.
func TestHandleFilesSupernoteFiltersBoox(t *testing.T) {
	h := newTestHandler()
	h.notes.(*mockNoteService).files = []service.NoteFile{
		{Path: "/notes/sn-only.note", Name: "sn-only.note", FileType: "note", Source: "supernote"},
		{Path: "/boox/hidden.note", Name: "hidden.note", FileType: "note", Source: "boox"},
	}

	req := httptest.NewRequest(http.MethodGet, "/files/supernote", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /files/supernote returned %d; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "sn-only.note") {
		t.Errorf("supernote row missing; body:\n%s", body)
	}
	if strings.Contains(body, "hidden.note") {
		t.Errorf("boox row leaked into supernote view; body:\n%s", body)
	}
}

// TestHandleFilesBooxRendersBooxColumns verifies /files/boox surfaces the
// BooxNoteSummary fields (Title, Folder, DeviceModel, PageCount) that the
// merged view hides.
func TestHandleFilesBooxRendersBooxColumns(t *testing.T) {
	h := newTestHandler()
	h.notes.(*mockNoteService).booxNotes = []service.BooxNoteSummary{
		{
			Path:        "/boox/project.note",
			Filename:    "project.note",
			Title:       "Project Plan",
			DeviceModel: "NoteAir5C",
			NoteType:    "Notebooks",
			Folder:      "Personal",
			PageCount:   42,
			JobStatus:   "done",
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/files/boox", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /files/boox returned %d; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"Project Plan", "NoteAir5C", "Notebooks", "Personal", "42"} {
		if !strings.Contains(body, want) {
			t.Errorf("boox view missing %q; body:\n%s", want, body)
		}
	}
}

// TestHandleFilesBoox_Filters verifies folder + device pills render and
// that ?folder= and ?device= narrow the list independently and together.
// Rows with device_model=".." are filtered out of the device pill row so
// the legacy-import field-swap artifact doesn't clutter the UI.
func TestHandleFilesBoox_Filters(t *testing.T) {
	h := newTestHandler()
	h.notes.(*mockNoteService).booxNotes = []service.BooxNoteSummary{
		{Path: "/boox/a.note", Title: "NoteMax Personal", DeviceModel: "NoteMax", Folder: "Personal"},
		{Path: "/boox/b.note", Title: "NoteMax Moffitt", DeviceModel: "NoteMax", Folder: "Moffitt"},
		{Path: "/boox/c.note", Title: "Go7 Personal", DeviceModel: "Go7", Folder: "Personal"},
		{Path: "/boox/d.note", Title: "Broken Legacy", DeviceModel: "..", Folder: "Go103"},
	}

	t.Run("no_filter_shows_all_and_hides_broken_device_pill", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/files/boox", nil)
		req.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d", w.Code)
		}
		body := w.Body.String()
		for _, want := range []string{"NoteMax Personal", "NoteMax Moffitt", "Go7 Personal"} {
			if !strings.Contains(body, want) {
				t.Errorf("unfiltered view missing %q", want)
			}
		}
		// Device pill row has NoteMax and Go7 but not ".."
		if !strings.Contains(body, "NoteMax") || !strings.Contains(body, "Go7") {
			t.Errorf("device pills missing; body:\n%s", body)
		}
		// The ".." device must not appear as its own pill (the artifact rows
		// still appear as rows because we don't filter ListBooxNotes itself).
		// Look for a pill-button href with `?device=..` which would only come
		// from the device-pill rendering; that shouldn't exist.
		if strings.Contains(body, "?device=..") || strings.Contains(body, "?device=%2E%2E") {
			t.Errorf("device pill for '..' leaked into UI; body:\n%s", body)
		}
	})

	t.Run("filter_by_device", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/files/boox?device=NoteMax", nil)
		req.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d", w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, "NoteMax Personal") || !strings.Contains(body, "NoteMax Moffitt") {
			t.Errorf("NoteMax rows missing; body:\n%s", body)
		}
		if strings.Contains(body, "Go7 Personal") {
			t.Errorf("Go7 row leaked into NoteMax filter; body:\n%s", body)
		}
	})

	t.Run("filter_by_folder", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/files/boox?folder=Personal", nil)
		req.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d", w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, "NoteMax Personal") || !strings.Contains(body, "Go7 Personal") {
			t.Errorf("Personal rows missing; body:\n%s", body)
		}
		if strings.Contains(body, "NoteMax Moffitt") {
			t.Errorf("Moffitt row leaked into Personal filter; body:\n%s", body)
		}
	})

	t.Run("filter_by_device_and_folder", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/files/boox?device=NoteMax&folder=Personal", nil)
		req.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d", w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, "NoteMax Personal") {
			t.Errorf("intersection row missing; body:\n%s", body)
		}
		if strings.Contains(body, "NoteMax Moffitt") || strings.Contains(body, "Go7 Personal") {
			t.Errorf("filter leak; body:\n%s", body)
		}
	})
}

// TestHandleFilesSupernoteNoSource verifies the error branch renders an
// informative empty-state card (not a 500) when no Supernote source is wired.
func TestHandleFilesSupernoteNoSource(t *testing.T) {
	h := newTestHandler()
	h.notes.(*mockNoteService).pipelineConfigured = false

	req := httptest.NewRequest(http.MethodGet, "/files/supernote", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with error banner, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "No Supernote source configured") {
		t.Errorf("expected empty-state banner; body:\n%s", w.Body.String())
	}
}

// TestHandleFilesBooxNoSource verifies the equivalent empty-state for the
// Boox tab.
func TestHandleFilesBooxNoSource(t *testing.T) {
	h := newTestHandler()
	h.notes.(*mockNoteService).booxEnabled = false

	req := httptest.NewRequest(http.MethodGet, "/files/boox", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with error banner, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "No Boox source configured") {
		t.Errorf("expected empty-state banner; body:\n%s", w.Body.String())
	}
}

// TestRowMutationDispatchesByBooxPrefix verifies that a mutation on a Boox
// path emits a _boox_file_row fragment (with Boox-specific Title/Folder
// markup) rather than the legacy _sn_file_row shape.
func TestRowMutationDispatchesByBooxPrefix(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	notes := &mockNoteService{
		docs:               make(map[string][]service.SearchResult),
		contents:           make(map[string]interface{}),
		pipelineConfigured: true,
		booxEnabled:        true,
		booxNotes: []service.BooxNoteSummary{
			{
				Path:      "/boox/bar.note",
				Title:     "Bar Notes",
				Folder:    "Personal",
				JobStatus: "done",
			},
		},
	}
	h := NewHandler(
		&mockTaskService{},
		notes,
		&mockSearchService{},
		&mockConfigService{},
		nil,
		"",
		"/boox",
		logger,
		broadcaster,
	)

	form := url.Values{}
	form.Set("path", "/boox/bar.note")
	req := httptest.NewRequest(http.MethodPost, "/files/queue", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("queue returned %d; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// _boox_file_row-specific markup: Title + Folder ("Personal") column.
	if !strings.Contains(body, "Bar Notes") {
		t.Errorf("expected boox-fragment with Title; body:\n%s", body)
	}
	if !strings.Contains(body, "Personal") {
		t.Errorf("expected boox-fragment with Folder column; body:\n%s", body)
	}
}

// TestRowMutationBooxNonHXRedirectsToBooxTab verifies that the non-HX
// redirect for a Boox-path mutation lands on /files/boox, not /files/
// supernote.
func TestRowMutationBooxNonHXRedirectsToBooxTab(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	notes := &mockNoteService{
		docs:               make(map[string][]service.SearchResult),
		contents:           make(map[string]interface{}),
		pipelineConfigured: true,
		booxEnabled:        true,
	}
	h := NewHandler(
		&mockTaskService{},
		notes,
		&mockSearchService{},
		&mockConfigService{},
		nil,
		"",
		"/boox",
		logger,
		broadcaster,
	)

	form := url.Values{}
	form.Set("path", "/boox/bar.note")
	req := httptest.NewRequest(http.MethodPost, "/files/queue", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// no HX-Request header
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303; got %d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if loc != "/files/boox" {
		t.Errorf("Location=%q, want /files/boox", loc)
	}
}

// postSPCSettings POSTs section=ub-spc with the given form values and asserts a
// 303 redirect, returning the config reloaded from the DB.
func postSPCSettings(t *testing.T, h http.Handler, db *sql.DB, form url.Values) *appconfig.Config {
	t.Helper()
	form.Set("section", "ub-spc")
	req := httptest.NewRequest("POST", "/settings/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303; got %d body=%s", w.Code, w.Body.String())
	}
	cfg, err := appconfig.Load(context.Background(), db)
	if err != nil {
		t.Fatalf("appconfig.Load: %v", err)
	}
	return cfg
}

// TestHandleSettingsSave_SPC verifies the ub-spc section persists non-secret
// fields, keeps secrets when their field is left blank, overwrites them when
// non-blank, and ignores an unparseable quota.
func TestHandleSettingsSave_SPC(t *testing.T) {
	ctx := context.Background()
	db, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("notedb open: %v", err)
	}
	defer db.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	config := service.NewConfigService(db, &appconfig.Config{})
	h := NewHandler(nil, &mockNoteService{}, nil, config, db, "", "", logger, broadcaster)

	// First save: set all fields including secrets.
	cfg := postSPCSettings(t, h, db, url.Values{
		"spc_mode":            {"server"},
		"spc_listen_addr":     {":9999"},
		"spc_file_root":       {"/tmp/root1"},
		"spc_device_account":  {"dev@example.com"},
		"spc_device_password": {"secret1"},
		"spc_jwt_secret":      {"jwt1"},
		"spc_quota_bytes":     {"123456"},
	})
	if cfg.SPCMode != "server" || cfg.SPCListenAddr != ":9999" || cfg.SPCFileRoot != "/tmp/root1" ||
		cfg.SPCDeviceAccount != "dev@example.com" || cfg.SPCDevicePassword != "secret1" ||
		cfg.SPCJWTSecret != "jwt1" || cfg.SPCQuotaBytes != 123456 {
		t.Fatalf("first save not persisted: %+v", cfg)
	}

	// Second save: blank secrets keep current; non-secret field updates;
	// unparseable quota keeps the prior value.
	cfg = postSPCSettings(t, h, db, url.Values{
		"spc_mode":            {"server"},
		"spc_listen_addr":     {":9999"},
		"spc_file_root":       {"/tmp/root2"},
		"spc_device_account":  {"dev@example.com"},
		"spc_device_password": {""},
		"spc_jwt_secret":      {""},
		"spc_quota_bytes":     {"not-a-number"},
	})
	if cfg.SPCFileRoot != "/tmp/root2" {
		t.Errorf("file root not updated: %q", cfg.SPCFileRoot)
	}
	if cfg.SPCDevicePassword != "secret1" {
		t.Errorf("blank password should keep current; got %q", cfg.SPCDevicePassword)
	}
	if cfg.SPCJWTSecret != "jwt1" {
		t.Errorf("blank JWT secret should keep current; got %q", cfg.SPCJWTSecret)
	}
	if cfg.SPCQuotaBytes != 123456 {
		t.Errorf("unparseable quota should keep prior value; got %d", cfg.SPCQuotaBytes)
	}

	// Third save: a non-blank secret overwrites.
	cfg = postSPCSettings(t, h, db, url.Values{
		"spc_mode":            {"server"},
		"spc_device_password": {"secret2"},
	})
	if cfg.SPCDevicePassword != "secret2" {
		t.Errorf("password not overwritten; got %q", cfg.SPCDevicePassword)
	}
}
