package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/rag"
	"github.com/sysop/ultrabridge/internal/service"
)

func TestHandleFilesRemarkable_BrowseAndDetail(t *testing.T) {
	h := newTestHandler()
	notes := h.notes.(*mockNoteService)
	notes.remarkableEnabled = true
	notes.rmCrumbs = []service.RemarkableCrumb{
		{FolderID: "", Name: "Home"},
		{FolderID: "folder-1", Name: "Projects"},
	}
	notes.rmEntries = []service.RemarkableEntry{
		{IsFolder: true, ID: "folder-2", Name: "Archive"},
		{ID: "doc-1", Name: "Project Plan", Path: "remarkable://doc-1", PageCount: 5},
	}
	notes.rmDetail = service.RemarkableDocumentDetail{
		ID: "doc-1", Name: "Project Plan", Type: "document", Path: "remarkable://doc-1", PageCount: 5,
		FolderPath: []string{"Projects"},
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/remarkable?folder=folder-1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /files/remarkable = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"reMarkable Files", "Projects", "Archive", "Project Plan", "5", "remarkable://doc-1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("browse body missing %q:\n%s", want, body)
		}
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/remarkable?document=doc-1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /files/remarkable detail = %d", w.Code)
	}
	body = w.Body.String()
	for _, want := range []string{"Project Plan", "doc-1", "Rendering is not available yet", "OCR is not available yet"} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail body missing %q:\n%s", want, body)
		}
	}
}

func TestAPIv1RemarkableDocumentDetail(t *testing.T) {
	h := newTestHandler()
	notes := h.notes.(*mockNoteService)
	notes.remarkableEnabled = true
	notes.rmDetail = service.RemarkableDocumentDetail{
		ID: "doc-1", Name: "Project Plan", Type: "document", Path: "remarkable://doc-1", PageCount: 5,
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/remarkable/documents/doc-1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/remarkable/documents/doc-1 = %d", w.Code)
	}
	var body service.RemarkableDocumentDetail
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.ID != "doc-1" || body.Path != "remarkable://doc-1" || body.RenderAvailable || body.OCRAvailable {
		t.Fatalf("detail = %+v", body)
	}
}

func TestHandleRemarkableReprocess(t *testing.T) {
	h := newTestHandler()
	notes := h.notes.(*mockNoteService)

	req := httptest.NewRequest(http.MethodPost, "/files/remarkable/reprocess", strings.NewReader("document=doc-1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST /files/remarkable/reprocess = %d body=%s", w.Code, w.Body.String())
	}
	if len(notes.rmReprocessed) != 1 || notes.rmReprocessed[0] != "doc-1" {
		t.Fatalf("rmReprocessed = %v", notes.rmReprocessed)
	}
}

func TestSearchPage_RemarkableFacetAndBadge(t *testing.T) {
	h := newTestHandler()
	notes := h.notes.(*mockNoteService)
	notes.remarkableEnabled = true
	search := h.search.(*mockSearchService)
	search.results = []service.SearchResult{{
		Path: "remarkable://doc-1", Page: 0, Title: "Project Plan", Snippet: "alpha", SourceType: rag.SourceRemarkable,
	}}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/search?q=alpha&source=remarkable", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /search = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`value="remarkable" checked`, "badge-rm", "Project Plan"} {
		if !strings.Contains(body, want) {
			t.Fatalf("search body missing %q:\n%s", want, body)
		}
	}
}
