package booxpipeline

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/taskstore"
)

// mockTaskCreator implements TaskCreator for testing.
type mockTaskCreator struct {
	tasks     []taskstore.Task
	created   []taskstore.Task
	listErr   error
	createErr error
}

func (m *mockTaskCreator) List(ctx context.Context) ([]taskstore.Task, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.tasks, nil
}

func (m *mockTaskCreator) Create(ctx context.Context, t *taskstore.Task) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.created = append(m.created, *t)
	return nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCreateTasksFromTodos_CreatesNew(t *testing.T) {
	tc := &mockTaskCreator{}
	todos := []TodoItem{
		{Type: "todo", Text: "Buy milk"},
		{Type: "todo", Text: "Call dentist"},
	}

	created := CreateTasksFromTodos(context.Background(), tc, "/notes/test.note", todos, "", testLogger())

	if created != 2 {
		t.Errorf("created = %d, want 2", created)
	}
	if len(tc.created) != 2 {
		t.Fatalf("tc.created = %d, want 2", len(tc.created))
	}
	if tc.created[0].Title.String != "Buy milk" {
		t.Errorf("first task title = %q, want 'Buy milk'", tc.created[0].Title.String)
	}
	if tc.created[1].Title.String != "Call dentist" {
		t.Errorf("second task title = %q, want 'Call dentist'", tc.created[1].Title.String)
	}
	// Detail is two lines: a human-readable "From Boox red ink in <basename>"
	// plus a relative URL that the web UI parses into a clickable link.
	got := tc.created[0].Detail.String
	wantHeader := "From Boox red ink in test.note"
	wantURL := "Open: /files/boox?detail=%2Fnotes%2Ftest.note"
	if !strings.Contains(got, wantHeader) {
		t.Errorf("detail missing header %q; got %q", wantHeader, got)
	}
	if !strings.Contains(got, wantURL) {
		t.Errorf("detail missing URL line %q; got %q", wantURL, got)
	}
}

func TestCreateTasksFromTodos_ExternalBaseURL(t *testing.T) {
	tc := &mockTaskCreator{}
	todos := []TodoItem{{Type: "todo", Text: "External link"}}

	// With base URL set, Detail's second line should contain the absolute URL.
	created := CreateTasksFromTodos(context.Background(), tc, "/notes/test.note", todos, "https://ub.example.com", testLogger())
	if created != 1 {
		t.Fatalf("created = %d, want 1", created)
	}
	got := tc.created[0].Detail.String
	want := "Open: https://ub.example.com/files/boox?detail=%2Fnotes%2Ftest.note"
	if !strings.Contains(got, want) {
		t.Errorf("detail missing absolute URL %q; got %q", want, got)
	}

	// Trailing slash on base URL should be stripped (no double slash).
	tc2 := &mockTaskCreator{}
	_ = CreateTasksFromTodos(context.Background(), tc2, "/notes/test.note", todos, "https://ub.example.com/", testLogger())
	got2 := tc2.created[0].Detail.String
	if strings.Contains(got2, "com//files") {
		t.Errorf("trailing slash not stripped; got %q", got2)
	}
}

func TestCreateTasksFromTodos_SkipsDuplicateIncomplete(t *testing.T) {
	tc := &mockTaskCreator{
		tasks: []taskstore.Task{
			{Title: sql.NullString{String: "Buy milk", Valid: true}, Status: sql.NullString{String: "needsAction", Valid: true}},
		},
	}
	todos := []TodoItem{
		{Type: "todo", Text: "Buy milk"},
		{Type: "todo", Text: "New task"},
	}

	created := CreateTasksFromTodos(context.Background(), tc, "/notes/test.note", todos, "", testLogger())

	if created != 1 {
		t.Errorf("created = %d, want 1 (duplicate skipped)", created)
	}
	if len(tc.created) != 1 {
		t.Fatalf("tc.created = %d, want 1", len(tc.created))
	}
	if tc.created[0].Title.String != "New task" {
		t.Errorf("created task = %q, want 'New task'", tc.created[0].Title.String)
	}
}

func TestCreateTasksFromTodos_SkipsDuplicateCompleted(t *testing.T) {
	// Completed tasks should also prevent re-creation
	tc := &mockTaskCreator{
		tasks: []taskstore.Task{
			{Title: sql.NullString{String: "Already done", Valid: true}, Status: sql.NullString{String: "completed", Valid: true}},
		},
	}
	todos := []TodoItem{
		{Type: "todo", Text: "Already done"},
	}

	created := CreateTasksFromTodos(context.Background(), tc, "/notes/test.note", todos, "", testLogger())

	if created != 0 {
		t.Errorf("created = %d, want 0 (completed task should not be re-created)", created)
	}
}

func TestCreateTasksFromTodos_DedupsWithinBatch(t *testing.T) {
	tc := &mockTaskCreator{}
	todos := []TodoItem{
		{Type: "todo", Text: "Same thing"},
		{Type: "todo", Text: "Same thing"},
		{Type: "todo", Text: "Same thing"},
	}

	created := CreateTasksFromTodos(context.Background(), tc, "/notes/test.note", todos, "", testLogger())

	if created != 1 {
		t.Errorf("created = %d, want 1 (batch dedup)", created)
	}
}

func TestCreateTasksFromTodos_EmptyList(t *testing.T) {
	tc := &mockTaskCreator{}
	created := CreateTasksFromTodos(context.Background(), tc, "/notes/test.note", nil, "", testLogger())

	if created != 0 {
		t.Errorf("created = %d, want 0", created)
	}
}

func TestCreateTasksFromTodos_ListError(t *testing.T) {
	tc := &mockTaskCreator{
		listErr: context.DeadlineExceeded,
	}
	todos := []TodoItem{
		{Type: "todo", Text: "Should not be created"},
	}

	created := CreateTasksFromTodos(context.Background(), tc, "/notes/test.note", todos, "", testLogger())

	if created != 0 {
		t.Errorf("created = %d, want 0 (list error should abort)", created)
	}
}

func TestCreateTasksFromTodos_CreateError(t *testing.T) {
	tc := &mockTaskCreator{
		createErr: context.DeadlineExceeded,
	}
	todos := []TodoItem{
		{Type: "todo", Text: "Will fail"},
		{Type: "todo", Text: "Also fail"},
	}

	created := CreateTasksFromTodos(context.Background(), tc, "/notes/test.note", todos, "", testLogger())

	if created != 0 {
		t.Errorf("created = %d, want 0 (create errors)", created)
	}
	// Should still have attempted both
}
