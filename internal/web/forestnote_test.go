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

func TestForestNoteTab_BrowseTable(t *testing.T) {
	notes := &mockNoteService{
		forestNoteEnabled: true,
		fnCrumbs:          []service.ForestNoteCrumb{{FolderID: "f1", Name: "Projects"}},
		fnEntries: []service.ForestNoteEntry{
			{IsFolder: true, ID: "f2", Name: "Subfolder"},
			{ID: "n1", Name: "Filed Notebook", Path: "forestnote://n1", PageCount: 2, Status: "indexed"},
		},
	}
	w := httptest.NewRecorder()
	fnTestHandler(notes).ServeHTTP(w, httptest.NewRequest("GET", "/files/forestnote?folder=f1", nil))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	// Renders a table with the Supernote-style columns and the entries.
	for _, want := range []string{"<table", "Created", "Modified", "Status", "Actions",
		"Projects", "Subfolder", "Filed Notebook", "?notebook=n1", "?folder=f2", "indexed"} {
		if !strings.Contains(body, want) {
			t.Errorf("browse body missing %q", want)
		}
	}
}

func TestForestNoteTab_NotebookDetail(t *testing.T) {
	notes := &mockNoteService{
		forestNoteEnabled: true,
		fnDetail: service.ForestNoteNotebookDetail{
			NotebookID: "n1", Name: "Journal", PageCount: 2,
			FolderPath: []string{"Projects"},
			Pages: []service.ForestNotePage{
				{PageID: "pgA", Path: "forestnote://n1/pgA", Ordinal: 0, BodyText: "recognized words"},
				{PageID: "pgB", Path: "forestnote://n1/pgB", Ordinal: 1},
			},
		},
	}
	w := httptest.NewRecorder()
	fnTestHandler(notes).ServeHTTP(w, httptest.NewRequest("GET", "/files/forestnote?notebook=n1", nil))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Journal", "Projects", // header metadata
		"/files/forestnote/render?path=forestnote", // page image
		"recognized words",                         // OCR text
		"/files/forestnote/export?notebook=n1",     // download
		"/files/forestnote/reprocess",              // re-OCR
		"/files/forestnote/delete",                 // delete
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail body missing %q\n---\n%s", want, body)
		}
	}
}

func TestForestNoteTab_NotebookDetailTargetsPageQuery(t *testing.T) {
	notes := &mockNoteService{
		forestNoteEnabled: true,
		fnDetail: service.ForestNoteNotebookDetail{
			NotebookID: "n1", Name: "Journal", PageCount: 2,
			Pages: []service.ForestNotePage{
				{PageID: "pgA", Path: "forestnote://n1/pgA", Ordinal: 0},
				{PageID: "pgB", Path: "forestnote://n1/pgB", Ordinal: 1, BodyText: "source page"},
			},
		},
	}
	w := httptest.NewRecorder()
	fnTestHandler(notes).ServeHTTP(w, httptest.NewRequest("GET", "/files/forestnote?notebook=n1&page=pgB", nil))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`id="fn-page-pgB"`, `detail-page-target`, `source page`} {
		if !strings.Contains(body, want) {
			t.Errorf("detail body missing %q\n---\n%s", want, body)
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

func TestForestNoteDelete_HXEmptyAndTracks(t *testing.T) {
	notes := &mockNoteService{forestNoteEnabled: true}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/files/forestnote/delete", strings.NewReader("notebook=n1&back=f1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	fnTestHandler(notes).ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(notes.fnDeleted) != 1 || notes.fnDeleted[0] != "n1" {
		t.Errorf("deleted = %+v, want [n1]", notes.fnDeleted)
	}
}

func TestForestNoteReprocess_HXEmptyAndTracks(t *testing.T) {
	notes := &mockNoteService{forestNoteEnabled: true}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/files/forestnote/reprocess?notebook=n1", nil)
	req.Header.Set("HX-Request", "true")
	fnTestHandler(notes).ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(notes.fnReprocessed) != 1 || notes.fnReprocessed[0] != "n1" {
		t.Errorf("reprocessed = %+v, want [n1]", notes.fnReprocessed)
	}
}

func TestForestNoteExport_PDFHeaders(t *testing.T) {
	notes := &mockNoteService{forestNoteEnabled: true, fnExportPDF: []byte("%PDF-1.4 fake")}
	w := httptest.NewRecorder()
	fnTestHandler(notes).ServeHTTP(w, httptest.NewRequest("GET", "/files/forestnote/export?notebook=n1", nil))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("content-type = %q, want application/pdf", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".pdf") {
		t.Errorf("content-disposition = %q", cd)
	}
}
