package caldav

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"

	ical "github.com/emersion/go-ical"
	"github.com/sysop/ultrabridge/internal/taskstore"
)

func bareCal() *ical.Calendar {
	cal := ical.NewCalendar()
	cal.Props.SetText("VERSION", "2.0")
	cal.Props.SetText("PRODID", "test")
	todo := ical.NewComponent("VTODO")
	todo.Props.SetText("UID", "t1")
	cal.Children = append(cal.Children, todo)
	return cal
}

func countFNRenderAttach(t *testing.T, cal *ical.Calendar) (int, *ical.Prop) {
	t.Helper()
	todo, err := FindVTODO(cal)
	if err != nil || todo == nil {
		t.Fatal("no vtodo")
	}
	n := 0
	var last *ical.Prop
	props := todo.Props[ical.PropAttach]
	for i := range props {
		if props[i].Params.Get("X-UB-FN-RENDER") == "1" {
			n++
			last = &props[i]
		}
	}
	return n, last
}

func fnTask() *taskstore.Task {
	return &taskstore.Task{
		TaskID:               "t1",
		Title:                sql.NullString{String: "lasso note", Valid: true},
		Status:               sql.NullString{String: "needsAction", Valid: true},
		ForestNoteNotebookID: sql.NullString{String: "NB1", Valid: true},
		ForestNotePageID:     sql.NullString{String: "PG1", Valid: true},
	}
}

func TestAddFNRenderAttach_Synthesized(t *testing.T) {
	cal := bareCal()
	deps := newAttachDeps(t) // BaseURL https://ub.example.com
	AddFNRenderAttach(cal, fnTask(), deps)

	n, p := countFNRenderAttach(t, cal)
	if n != 1 {
		t.Fatalf("want exactly 1 fn-render ATTACH, got %d", n)
	}
	if p.Params.Get("FMTTYPE") != "image/jpeg" {
		t.Errorf("FMTTYPE = %q", p.Params.Get("FMTTYPE"))
	}
	wantPrefix := "https://ub.example.com/api/v1/attachments/fn-render?path="
	if !strings.HasPrefix(p.Value, wantPrefix) {
		t.Errorf("value %q lacks prefix %q", p.Value, wantPrefix)
	}
	// The canonical UB fnpath (forestnote://NB1/PG1) must be the signed target,
	// NOT FN's native-URL form (forestnote://notebook/NB1/page/PG1).
	if !strings.Contains(p.Value, "forestnote%3A%2F%2FNB1%2FPG1") {
		t.Errorf("value %q missing escaped forestnote://NB1/PG1", p.Value)
	}
	if !strings.Contains(p.Value, "&sig=") {
		t.Errorf("value %q missing signature", p.Value)
	}
}

func TestAddFNRenderAttach_Idempotent(t *testing.T) {
	cal := bareCal()
	deps := newAttachDeps(t)
	AddFNRenderAttach(cal, fnTask(), deps)
	AddFNRenderAttach(cal, fnTask(), deps) // second call must not add a duplicate
	if n, _ := countFNRenderAttach(t, cal); n != 1 {
		t.Errorf("want 1 fn-render ATTACH after two calls, got %d", n)
	}
}

func TestAddFNRenderAttach_SkipNoBaseURL(t *testing.T) {
	cal := bareCal()
	deps := newAttachDeps(t)
	deps.BaseURL = "" // no external origin → can't make an absolute URL
	AddFNRenderAttach(cal, fnTask(), deps)
	if n, _ := countFNRenderAttach(t, cal); n != 0 {
		t.Errorf("should skip without a base URL, got %d", n)
	}
}

func TestAddFNRenderAttach_SkipNonFN(t *testing.T) {
	cal := bareCal()
	deps := newAttachDeps(t)
	nonFN := &taskstore.Task{
		TaskID: "t1",
		Title:  sql.NullString{String: "plain", Valid: true},
		Status: sql.NullString{String: "needsAction", Valid: true},
	}
	AddFNRenderAttach(cal, nonFN, deps)
	if n, _ := countFNRenderAttach(t, cal); n != 0 {
		t.Errorf("should skip non-FN task, got %d", n)
	}
}

// TestBackend_FNTaskGetsRenderAttach exercises the wiring: GET an FN task and
// it carries exactly one fn-render ATTACH; a second GET still yields exactly
// one (synthesized fresh each time — no accumulation / re-sync).
func TestBackend_FNTaskGetsRenderAttach(t *testing.T) {
	store := newMockTaskStore()
	deps := newAttachDeps(t)
	backend := NewBackend(store, "/caldav", "Test", "preserve", nil)
	backend.SetTaskAttach(deps.Store, deps.Signer, deps.BaseURL)

	task := fnTask()
	store.tasks[task.TaskID] = task

	for i := 0; i < 2; i++ {
		obj := backend.taskToCalendarObject(task)
		if n, _ := countFNRenderAttach(t, obj.Data); n != 1 {
			t.Fatalf("GET #%d: want 1 fn-render ATTACH, got %d", i+1, n)
		}
	}
}

// TestBackend_FNTaskWithInlineComposes confirms an FN task that ALSO has an
// inline-binary attachment round-trips both: the reconstructed inline ATTACH
// plus the synthesized fn-render ATTACH coexist.
func TestBackend_FNTaskWithInlineComposes(t *testing.T) {
	store := newMockTaskStore()
	deps := newAttachDeps(t)
	backend := NewBackend(store, "/caldav", "Test", "preserve", nil)
	backend.SetTaskAttach(deps.Store, deps.Signer, deps.BaseURL)

	// PUT an FN task carrying an inline binary attachment.
	cal := buildInlineAttachCal(bytes.Repeat([]byte("Z"), 1024), "image/png", "p.png")
	todo, _ := FindVTODO(cal)
	todo.Props.SetText("X-FORESTNOTE-NOTEBOOK-ID", "NB1")
	todo.Props.SetText("X-FORESTNOTE-PAGE-ID", "PG1")
	if _, err := backend.PutCalendarObject(context.Background(), "/caldav/user/calendars/tasks/t1.ics", cal, nil); err != nil {
		t.Fatalf("PUT: %v", err)
	}
	var stored *taskstore.Task
	for _, tk := range store.tasks {
		stored = tk
	}
	if stored == nil {
		t.Fatal("not stored")
	}

	obj := backend.taskToCalendarObject(stored)
	out, _ := FindVTODO(obj.Data)
	attaches := out.Props[ical.PropAttach]
	if len(attaches) != 2 {
		t.Fatalf("want 2 ATTACH (inline + fn-render), got %d", len(attaches))
	}
	var sawInline, sawRender bool
	for i := range attaches {
		p := &attaches[i]
		if p.Params.Get("X-UB-FN-RENDER") == "1" {
			sawRender = true
		} else if _, err := p.Binary(); err == nil {
			sawInline = true // reconstructed inline binary
		}
	}
	if !sawInline || !sawRender {
		t.Errorf("composition broken: inline=%v render=%v", sawInline, sawRender)
	}
}
