package booxpipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/taskstore"
)

func newLedgerStore(t *testing.T) *Store {
	t.Helper()
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewStore(db)
}

func TestTodoTextKey(t *testing.T) {
	cases := map[string]string{
		"Call the vendor":          "call the vendor",
		"  Call   the vendor.  ":   "call the vendor",
		"Call the Vendor!":         "call the vendor",
		`"Weka doesn't support"`:   "weka doesn t support",
		"Bright License - Midia":   "bright license midia",
		"Look for new people\nnow": "look for new people now",
		"...":                      "",
	}
	for in, want := range cases {
		if got := todoTextKey(in); got != want {
			t.Errorf("todoTextKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTodoSourceNote(t *testing.T) {
	cases := []struct {
		detail string
		want   string
		ok     bool
	}{
		{"From Boox red ink in 20261007 Hammerspace.note\nOpen: /files/boox?detail=x", "20261007 Hammerspace.note", true},
		{"From Boox red ink in Notebook-1.pdf", "Notebook-1.pdf", true},
		{"From Boox red ink: /mnt/boox/onyx/Go/Notebooks/20260301 Sync.note", "20260301 Sync.note", true},
		{"From Boox red ink in \nOpen: x", "", false},
		{"Buy milk", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := TodoSourceNote(c.detail)
		if got != c.want || ok != c.ok {
			t.Errorf("TodoSourceNote(%q) = (%q, %v), want (%q, %v)", c.detail, got, ok, c.want, c.ok)
		}
	}
}

func TestStoreTodoLedger(t *testing.T) {
	s := newLedgerStore(t)
	ctx := context.Background()

	if seen, err := s.TodoEmitted(ctx, "a.note", "Call the vendor"); err != nil || seen {
		t.Fatalf("fresh ledger: seen=%v err=%v", seen, err)
	}
	if err := s.RecordTodo(ctx, "a.note", "Call the vendor"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTodo(ctx, "a.note", "Call the vendor"); err != nil {
		t.Fatalf("RecordTodo must be idempotent: %v", err)
	}
	if seen, _ := s.TodoEmitted(ctx, "a.note", "call the vendor."); !seen {
		t.Error("normalized match not found")
	}
	if seen, _ := s.TodoEmitted(ctx, "b.note", "Call the vendor"); seen {
		t.Error("ledger is per note; same text in another note must not match")
	}
}

func TestSeedTodoLedger(t *testing.T) {
	s := newLedgerStore(t)
	ctx := context.Background()
	tasks := []taskstore.Task{
		{Title: taskstore.SqlStr("Live todo"), Detail: taskstore.SqlStr("From Boox red ink in x.note\nOpen: /files/boox?detail=x"), IsDeleted: "N"},
		{Title: taskstore.SqlStr("Deleted todo"), Detail: taskstore.SqlStr("From Boox red ink in x.note\nOpen: /files/boox?detail=x"), IsDeleted: "Y"},
		{Title: taskstore.SqlStr("Legacy todo"), Detail: taskstore.SqlStr("From Boox red ink: /notes/y.note"), IsDeleted: "N"},
		{Title: taskstore.SqlStr("Manual task"), Detail: taskstore.SqlStr("typed in a CalDAV client")},
		{Title: taskstore.SqlStr("No detail")},
	}
	added, err := s.SeedTodoLedger(ctx, tasks)
	if err != nil {
		t.Fatal(err)
	}
	if added != 3 {
		t.Errorf("added = %d, want 3", added)
	}
	if again, _ := s.SeedTodoLedger(ctx, tasks); again != 0 {
		t.Errorf("reseed added %d, want 0 (idempotent)", again)
	}
	for _, c := range []struct{ note, text string }{{"x.note", "Deleted todo"}, {"x.note", "Live todo"}, {"y.note", "Legacy todo"}} {
		if seen, _ := s.TodoEmitted(ctx, c.note, c.text); !seen {
			t.Errorf("%s / %q not seeded", c.note, c.text)
		}
	}
}

// failingLedger fails every lookup.
type failingLedger struct{}

func (failingLedger) TodoEmitted(context.Context, string, string) (bool, error) {
	return false, errors.New("db gone")
}
func (failingLedger) RecordTodo(context.Context, string, string) error { return nil }

func TestCreateTasksFromTodos_LedgerKeepsDeletedTodosGone(t *testing.T) {
	ledger := newLedgerStore(t)
	ctx := context.Background()
	todos := []TodoItem{{Type: "todo", Text: "Call the vendor"}, {Type: "todo", Text: "Order NVMe"}}

	tc := &mockTaskCreator{}
	if n := CreateTasksFromTodos(ctx, tc, ledger, "/notes/Mtg.note", todos, "", testLogger()); n != 2 {
		t.Fatalf("first pass created %d, want 2", n)
	}

	// The user deletes both tasks (they vanish from List), then the device
	// re-uploads the note and it is re-processed with slightly different OCR.
	tc2 := &mockTaskCreator{}
	again := []TodoItem{{Type: "todo", Text: "Call the vendor."}, {Type: "todo", Text: "order nvme"}, {Type: "todo", Text: "Brand new item"}}
	if n := CreateTasksFromTodos(ctx, tc2, ledger, "/other/dir/Mtg.note", again, "", testLogger()); n != 1 {
		t.Fatalf("re-process created %d, want 1 (only the new item)", n)
	}
	if tc2.created[0].Title.String != "Brand new item" {
		t.Errorf("created %q, want 'Brand new item'", tc2.created[0].Title.String)
	}
}

func TestCreateTasksFromTodos_LedgerErrorSkips(t *testing.T) {
	tc := &mockTaskCreator{}
	n := CreateTasksFromTodos(context.Background(), tc, failingLedger{}, "/notes/a.note",
		[]TodoItem{{Type: "todo", Text: "Anything"}}, "", testLogger())
	if n != 0 || len(tc.created) != 0 {
		t.Errorf("created %d with a broken ledger, want 0 (fail closed)", n)
	}
}

func TestCreateTasksFromTodos_TitleDuplicateIsRecorded(t *testing.T) {
	ledger := newLedgerStore(t)
	ctx := context.Background()
	tc := &mockTaskCreator{tasks: []taskstore.Task{{Title: taskstore.SqlStr("Call the vendor")}}}
	if n := CreateTasksFromTodos(ctx, tc, ledger, "/notes/a.note", []TodoItem{{Type: "todo", Text: "Call the vendor"}}, "", testLogger()); n != 0 {
		t.Fatalf("created %d, want 0 (title duplicate)", n)
	}
	if seen, _ := ledger.TodoEmitted(ctx, "a.note", "Call the vendor"); !seen {
		t.Error("a to-do skipped as a title duplicate should still be recorded in the ledger")
	}
}
