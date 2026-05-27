package web

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/logging"
	"github.com/sysop/ultrabridge/internal/service"
)

func fnTestHandler(notes *mockNoteService) *Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewHandler(&mockTaskService{}, notes, &mockSearchService{}, &mockConfigService{}, nil, "", "", logger, logging.NewLogBroadcaster())
}

func TestForestNoteTab_TreeMode(t *testing.T) {
	notes := &mockNoteService{
		forestNoteEnabled: true,
		fnTree: []service.ForestNoteTreeNode{{
			FolderID: "f1", Name: "Projects",
			Notebooks: []service.ForestNoteNotebook{{NotebookID: "n1", Name: "Filed Notebook", Path: "forestnote://n1", PageCount: 2}},
		}},
		fnUnfiled: []service.ForestNoteNotebook{{NotebookID: "n2", Name: "Loose Notebook", Path: "forestnote://n2"}},
	}
	w := httptest.NewRecorder()
	fnTestHandler(notes).ServeHTTP(w, httptest.NewRequest("GET", "/files/forestnote", nil))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"Projects", "Filed Notebook", "Loose Notebook", "?notebook=n1"} {
		if !strings.Contains(body, want) {
			t.Errorf("tree body missing %q", want)
		}
	}
}

func TestForestNoteTab_NotebookViewer(t *testing.T) {
	notes := &mockNoteService{
		forestNoteEnabled: true,
		fnNotebookName:    "Journal",
		fnPages: []service.ForestNotePage{
			{PageID: "pgA", Path: "forestnote://n1/pgA", Ordinal: 0},
			{PageID: "pgB", Path: "forestnote://n1/pgB", Ordinal: 1},
		},
	}
	w := httptest.NewRecorder()
	fnTestHandler(notes).ServeHTTP(w, httptest.NewRequest("GET", "/files/forestnote?notebook=n1", nil))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	// The render src is context-escaped in the query, so match on the encoded form.
	for _, want := range []string{"Journal", "/files/forestnote/render?path=forestnote"} {
		if !strings.Contains(body, want) {
			t.Errorf("viewer body missing %q\n---\n%s", want, body)
		}
	}
}

func TestForestNoteTab_EmptyStateWhenNoSource(t *testing.T) {
	w := httptest.NewRecorder()
	fnTestHandler(&mockNoteService{forestNoteEnabled: false}).ServeHTTP(w, httptest.NewRequest("GET", "/files/forestnote", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "No ForestNote source configured") {
		t.Error("expected empty-state message")
	}
}
