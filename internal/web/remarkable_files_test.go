package web

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
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
	for _, want := range []string{`value="remarkable" checked`, "badge-rm", "Project Plan", `/files/remarkable?document=doc-1`} {
		if !strings.Contains(body, want) {
			t.Fatalf("search body missing %q:\n%s", want, body)
		}
	}
}

func TestHandleFiles_LegacyRemarkableDetailRedirect(t *testing.T) {
	h := newTestHandler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files?detail=remarkable://doc-1", nil))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("GET /files legacy detail = %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/files/remarkable?document=doc-1" {
		t.Fatalf("Location = %q", loc)
	}
}

// --- file management (upload/download/delete/rename/move/folders) ---

func rmMultipartBody(t *testing.T, field, filename, content, parentField, parent string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if parentField != "" {
		if err := mw.WriteField(parentField, parent); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func TestRemarkableFileManagement_GatedWhenAbsent(t *testing.T) {
	h := newTestHandler()
	notes := h.notes.(*mockNoteService)
	notes.remarkableEnabled = true
	notes.rmFileManager = false

	body, ct := rmMultipartBody(t, "file", "book.pdf", "pdf", "folder", "")
	upload := httptest.NewRequest(http.MethodPost, "/files/remarkable/upload", body)
	upload.Header.Set("Content-Type", ct)

	apiBody, apiCT := rmMultipartBody(t, "file", "book.pdf", "pdf", "parent", "")
	apiUpload := httptest.NewRequest(http.MethodPost, "/api/v1/remarkable/documents", apiBody)
	apiUpload.Header.Set("Content-Type", apiCT)

	reqs := []*http.Request{
		upload,
		httptest.NewRequest(http.MethodGet, "/files/remarkable/download?document=doc-1", nil),
		httptest.NewRequest(http.MethodPost, "/files/remarkable/delete", strings.NewReader("document=doc-1")),
		httptest.NewRequest(http.MethodPost, "/files/remarkable/rename", strings.NewReader("document=doc-1&name=X")),
		httptest.NewRequest(http.MethodPost, "/files/remarkable/move", strings.NewReader("document=doc-1&folder=f")),
		httptest.NewRequest(http.MethodPost, "/files/remarkable/new-folder", strings.NewReader("name=F")),
		apiUpload,
		httptest.NewRequest(http.MethodGet, "/api/v1/remarkable/documents/doc-1/file", nil),
		httptest.NewRequest(http.MethodPatch, "/api/v1/remarkable/documents/doc-1", strings.NewReader(`{"name":"X"}`)),
		httptest.NewRequest(http.MethodDelete, "/api/v1/remarkable/documents/doc-1", nil),
		httptest.NewRequest(http.MethodPost, "/api/v1/remarkable/folders", strings.NewReader(`{"name":"F"}`)),
	}
	for _, req := range reqs {
		if req.Method == http.MethodPost && req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s without file manager = %d, want 404", req.Method, req.URL.Path, w.Code)
		}
	}
}

func TestHandleRemarkableUpload(t *testing.T) {
	h := newTestHandler()
	notes := h.notes.(*mockNoteService)
	notes.remarkableEnabled = true
	notes.rmFileManager = true

	body, ct := rmMultipartBody(t, "file", "Moby Dick.pdf", "pdf-bytes", "folder", "folder-1")
	req := httptest.NewRequest(http.MethodPost, "/files/remarkable/upload", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload = %d body=%s", w.Code, w.Body.String())
	}
	if len(notes.rmUploaded) != 1 || notes.rmUploaded[0] != "Moby Dick.pdf->folder-1" {
		t.Fatalf("rmUploaded = %v", notes.rmUploaded)
	}
	if string(notes.rmUploadPayload) != "pdf-bytes" {
		t.Fatalf("payload = %q", notes.rmUploadPayload)
	}
}

func TestHandleRemarkableDownload(t *testing.T) {
	h := newTestHandler()
	notes := h.notes.(*mockNoteService)
	notes.remarkableEnabled = true
	notes.rmFileManager = true
	notes.rmDownloadPayload = []byte("the original pdf")
	notes.rmDownloadName = "Moby Dick.pdf"
	notes.rmDownloadType = "application/pdf"

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/remarkable/download?document=doc-1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("download = %d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/pdf" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := w.Header().Get("Content-Disposition"); got != `attachment; filename="Moby Dick.pdf"` {
		t.Errorf("Content-Disposition = %q", got)
	}
	if w.Body.String() != "the original pdf" {
		t.Errorf("body = %q", w.Body.String())
	}
}

func TestHandleRemarkableFormMutations(t *testing.T) {
	h := newTestHandler()
	notes := h.notes.(*mockNoteService)
	notes.remarkableEnabled = true
	notes.rmFileManager = true

	post := func(path, form string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	if w := post("/files/remarkable/delete", "document=doc-1&back=folder-1"); w.Code != http.StatusOK {
		t.Fatalf("delete = %d", w.Code)
	}
	if w := post("/files/remarkable/rename", "document=doc-1&name=Renamed"); w.Code != http.StatusOK {
		t.Fatalf("rename = %d", w.Code)
	}
	if w := post("/files/remarkable/move", "document=doc-1&folder=folder-2"); w.Code != http.StatusOK {
		t.Fatalf("move = %d", w.Code)
	}
	if w := post("/files/remarkable/new-folder", "name=Books&folder="); w.Code != http.StatusOK {
		t.Fatalf("new-folder = %d", w.Code)
	}
	if w := post("/files/remarkable/rename", "document=doc-1&name=+"); w.Code != http.StatusBadRequest {
		t.Fatalf("blank rename = %d, want 400", w.Code)
	}

	if got := notes.rmDeleted; len(got) != 1 || got[0] != "doc-1" {
		t.Errorf("rmDeleted = %v", got)
	}
	if got := notes.rmRenamed; len(got) != 1 || got[0] != "doc-1->Renamed" {
		t.Errorf("rmRenamed = %v", got)
	}
	if got := notes.rmMoved; len(got) != 1 || got[0] != "doc-1->folder-2" {
		t.Errorf("rmMoved = %v", got)
	}
	if got := notes.rmFoldersCreated; len(got) != 1 || got[0] != "Books->" {
		t.Errorf("rmFoldersCreated = %v", got)
	}
}

func TestAPIv1RemarkableUpload(t *testing.T) {
	h := newTestHandler()
	notes := h.notes.(*mockNoteService)
	notes.remarkableEnabled = true
	notes.rmFileManager = true

	body, ct := rmMultipartBody(t, "file", "book.epub", "epub-bytes", "parent", "folder-9")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/remarkable/documents", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("API upload = %d body=%s", w.Code, w.Body.String())
	}
	var doc service.RemarkableDocument
	if err := json.NewDecoder(w.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.ID != "new-doc" || doc.Parent != "folder-9" {
		t.Fatalf("doc = %+v", doc)
	}
	if len(notes.rmUploaded) != 1 || notes.rmUploaded[0] != "book.epub->folder-9" {
		t.Fatalf("rmUploaded = %v", notes.rmUploaded)
	}
}

func TestAPIv1RemarkablePatch(t *testing.T) {
	h := newTestHandler()
	notes := h.notes.(*mockNoteService)
	notes.remarkableEnabled = true
	notes.rmFileManager = true

	patch := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/remarkable/documents/doc-1", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	if w := patch(`{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("empty patch = %d, want 400", w.Code)
	}
	if w := patch(`{"name":""}`); w.Code != http.StatusBadRequest {
		t.Fatalf("blank name = %d, want 400", w.Code)
	}
	if w := patch(`{"name":"Renamed"}`); w.Code != http.StatusNoContent {
		t.Fatalf("rename = %d", w.Code)
	}
	// parent "" is a real move (to My files), NOT an omitted field.
	if w := patch(`{"parent":""}`); w.Code != http.StatusNoContent {
		t.Fatalf("move to root = %d", w.Code)
	}
	if got := notes.rmRenamed; len(got) != 1 || got[0] != "doc-1->Renamed" {
		t.Errorf("rmRenamed = %v", got)
	}
	if got := notes.rmMoved; len(got) != 1 || got[0] != "doc-1->" {
		t.Errorf("rmMoved = %v (want move to root recorded)", got)
	}
}

func TestAPIv1RemarkableDeleteAndFolders(t *testing.T) {
	h := newTestHandler()
	notes := h.notes.(*mockNoteService)
	notes.remarkableEnabled = true
	notes.rmFileManager = true

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/v1/remarkable/documents/doc-1", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", w.Code)
	}
	if got := notes.rmDeleted; len(got) != 1 || got[0] != "doc-1" {
		t.Fatalf("rmDeleted = %v", got)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/remarkable/folders", strings.NewReader(`{"name":"Books","parent":"folder-1"}`)))
	if w.Code != http.StatusCreated {
		t.Fatalf("POST folders = %d body=%s", w.Code, w.Body.String())
	}
	var doc service.RemarkableDocument
	if err := json.NewDecoder(w.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.Type != "folder" || doc.Name != "Books" {
		t.Fatalf("folder = %+v", doc)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/remarkable/folders", strings.NewReader(`{"name":"  "}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("blank folder name = %d, want 400", w.Code)
	}
}

func TestAPIv1RemarkableErrorMapping(t *testing.T) {
	h := newTestHandler()
	notes := h.notes.(*mockNoteService)
	notes.remarkableEnabled = true
	notes.rmFileManager = true

	cases := []struct {
		err  error
		want int
	}{
		{service.ErrRemarkableNotFound, http.StatusNotFound},
		{service.ErrRemarkableNoPayload, http.StatusNotFound},
		{service.ErrRemarkableParentNotFound, http.StatusNotFound},
		{service.ErrRemarkableUnsupportedFile, http.StatusBadRequest},
		{service.ErrRemarkableNotAFolder, http.StatusBadRequest},
		{service.ErrRemarkableFolderNotEmpty, http.StatusConflict},
		{service.ErrRemarkableNoHashTree, http.StatusConflict},
		{service.ErrRemarkableTreeConflict, http.StatusConflict},
	}
	for _, tc := range cases {
		notes.rmFileErr = tc.err
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/v1/remarkable/documents/doc-1", nil))
		if w.Code != tc.want {
			t.Errorf("delete with %v = %d, want %d", tc.err, w.Code, tc.want)
		}
	}
}
