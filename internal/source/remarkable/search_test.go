package remarkable

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/source"
)

func newSearchTestDB(t *testing.T) *store {
	t.Helper()
	db, err := notedb.Open(context.Background(), filepath.Join(t.TempDir(), "ultrabridge.db"))
	if err != nil {
		t.Fatalf("open notedb: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate(context.Background(), db); err != nil {
		t.Fatalf("remarkable migrate: %v", err)
	}
	st := newStore(db, t.TempDir())
	if err := st.ensurePaths(); err != nil {
		t.Fatalf("ensurePaths: %v", err)
	}
	return st
}

func seedSearchNotebook(t *testing.T, st *store, docID string, pages []string) {
	t.Helper()
	lines := []string{
		entryLine(seedBlob(t, st, docID+"-content", searchNotebookContent(t, pages)), "0", docID+".content", 0, 100),
		entryLine(seedBlob(t, st, docID+"-metadata",
			`{"visibleName":"Search Notebook","type":"DocumentType","parent":""}`), "0", docID+".metadata", 0, 100),
	}
	for _, pageID := range pages {
		lines = append(lines, entryLine(
			seedBlob(t, st, docID+"-"+pageID+"-rm", "reMarkable .lines file, version=6          page"),
			"0",
			docID+"/"+pageID+".rm",
			0,
			100,
		))
	}
	sub := seedBlob(t, st, docID+"-sub", indexFile(lines...))
	top := seedBlob(t, st, docID+"-top", indexFile(entryLine(sub, "80000000", docID, len(lines), 0)))
	seedBlob(t, st, rootBlobID, top)
}

func searchNotebookContent(t *testing.T, pages []string) string {
	t.Helper()
	content := struct {
		FileType  string `json:"fileType"`
		PageCount int    `json:"pageCount"`
		CPages    struct {
			Pages []struct {
				ID string `json:"id"`
			} `json:"pages"`
		} `json:"cPages"`
	}{
		FileType:  "notebook",
		PageCount: len(pages),
	}
	for _, pageID := range pages {
		content.CPages.Pages = append(content.CPages.Pages, struct {
			ID string `json:"id"`
		}{ID: pageID})
	}
	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	return string(data)
}

func insertSearchContent(t *testing.T, st *store, id int64, path string, page int, body string, indexedAt int64) {
	t.Helper()
	_, err := st.db.ExecContext(context.Background(), `
		INSERT INTO note_content(id, note_path, page, title_text, body_text, keywords, source, model, indexed_at)
		VALUES (?, ?, ?, '', ?, '', 'remarkable', 'test', ?)`,
		id, path, page, body, indexedAt,
	)
	if err != nil {
		t.Fatalf("insert search content: %v", err)
	}
}

func TestStoreListSearchPagesFiltersAndMapsRemarkableOCR(t *testing.T) {
	st := newSearchTestDB(t)
	ctx := context.Background()
	seedSearchNotebook(t, st, "doc-1", []string{"page-a", "page-b", "page-c"})

	insertSearchContent(t, st, 1, remarkablePath("doc-1"), metadataPage, "metadata only", 10)
	insertSearchContent(t, st, 2, remarkablePath("doc-1"), 0, "first handwriting page", 20)
	insertSearchContent(t, st, 3, "boox://note", 0, "wrong source", 30)
	insertSearchContent(t, st, 4, remarkablePath("doc-1"), 1, "second handwriting page", 40)
	insertSearchContent(t, st, 5, remarkablePath("doc-1"), 2, "(Blank Page)", 50)
	insertSearchContent(t, st, 6, remarkablePath("missing-doc"), 0, "missing document", 60)

	rows, hasMore, err := st.listSearchPages(ctx, 0, 1)
	if err != nil {
		t.Fatalf("listSearchPages: %v", err)
	}
	if !hasMore {
		t.Fatal("hasMore = false, want true with limit 1")
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].DocumentID != "doc-1" || rows[0].PageID != "page-a" || rows[0].BodyText != "first handwriting page" {
		t.Fatalf("first row = %+v, want doc-1/page-a", rows[0])
	}

	rows, hasMore, err = st.listSearchPages(ctx, rows[0].Generation, 10)
	if err != nil {
		t.Fatalf("listSearchPages after generation: %v", err)
	}
	if hasMore {
		t.Fatal("hasMore = true, want false")
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].DocumentID != "doc-1" || rows[0].PageID != "page-b" || rows[0].Generation != searchGeneration(40, 4) {
		t.Fatalf("second row = %+v, want doc-1/page-b", rows[0])
	}
}

func TestSearchIndexVersionMatchesPaperProFirmware(t *testing.T) {
	if searchIndexVersion != 2 {
		t.Fatalf("searchIndexVersion = %d, want 2", searchIndexVersion)
	}
}

func TestDefaultSearchDeltaLimitCoversInitialBackfill(t *testing.T) {
	st := newSearchTestDB(t)
	ctx := context.Background()
	pages := make([]string, 101)
	for i := range pages {
		pages[i] = fmt.Sprintf("page-%03d", i)
	}
	seedSearchNotebook(t, st, "doc-1", pages)
	for i := range pages {
		insertSearchContent(t, st, int64(i+1), remarkablePath("doc-1"), i, fmt.Sprintf("handwriting page %d", i), int64(100+i))
	}

	rows, hasMore, err := st.listSearchPages(ctx, 0, searchPageSize)
	if err != nil {
		t.Fatalf("listSearchPages: %v", err)
	}
	if hasMore {
		t.Fatal("default device delta limit returned hasMore=true for 101-page initial backfill")
	}
	if len(rows) != len(pages) {
		t.Fatalf("got %d rows, want %d", len(rows), len(pages))
	}
	if rows[100].PageID != "page-100" {
		t.Fatalf("last row page ID = %q, want page-100", rows[100].PageID)
	}
}

func TestProtocolSearchEndpointsExposeRemarkableOnlyIndexes(t *testing.T) {
	db, err := notedb.Open(context.Background(), filepath.Join(t.TempDir(), "ultrabridge.db"))
	if err != nil {
		t.Fatalf("open notedb: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	row := source.SourceRow{
		Type:       "remarkable",
		Name:       "RM",
		ConfigJSON: `{"data_path":"` + t.TempDir() + `","pairing_code":"123456"}`,
	}
	src, err := NewSource(db, row, source.SharedDeps{})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if err := src.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(src.Stop)

	seedSearchNotebook(t, src.store, "doc-1", []string{"page-a"})
	insertSearchContent(t, src.store, 10, remarkablePath("doc-1"), 0, "find my handwriting", 100)
	insertSearchContent(t, src.store, 11, "boox://doc", 0, "not for device search", 101)

	mux := http.NewServeMux()
	src.RegisterRoutes(mux)
	userToken := pairUserToken(t, mux, "rm-device-search", "reMarkable Paper Pro")

	req := httptest.NewRequest(http.MethodGet, "/search/v1/settings", nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("settings = %d body=%s", w.Code, w.Body.String())
	}
	var settings searchSettingsResponse
	if err := json.NewDecoder(w.Body).Decode(&settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if !settings.SearchEnabled || settings.Language != searchLanguage {
		t.Fatalf("settings = %+v", settings)
	}

	req = httptest.NewRequest(http.MethodGet, "/search/v1/delta", nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delta = %d body=%s", w.Code, w.Body.String())
	}
	var delta searchDeltaResponse
	if err := json.NewDecoder(w.Body).Decode(&delta); err != nil {
		t.Fatalf("decode delta: %v", err)
	}
	if delta.Version != searchIndexVersion || !delta.Latest || len(delta.Changed) != 1 {
		t.Fatalf("delta = %+v", delta)
	}
	change := delta.Changed[0]
	if change.DocumentID != "doc-1" || change.PageID != "page-a" || change.Generation == 0 || change.DeltaID == "" {
		t.Fatalf("change = %+v", change)
	}

	req = httptest.NewRequest(http.MethodGet, "/search/v1/delta", nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	req.Header.Set("If-None-Match", w.Header().Get("ETag"))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotModified {
		t.Fatalf("delta with etag = %d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/search/v1/doc-1/page-a", nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("page index = %d body=%s", w.Code, w.Body.String())
	}
	var index searchIndexResponse
	if err := json.NewDecoder(w.Body).Decode(&index); err != nil {
		t.Fatalf("decode index: %v", err)
	}
	if index.Version != searchIndexVersion || index.Generation != change.Generation {
		t.Fatalf("index metadata = %+v, want generation %d", index, change.Generation)
	}
	if index.Handwritten.Content != "find my handwriting" {
		t.Fatalf("content = %q", index.Handwritten.Content)
	}
	if got := index.Handwritten.MainStrokes; got.Resolution != searchStrokeResolution || len(got.Strokes) != 3 {
		t.Fatalf("strokes = %+v", got)
	}

	req = httptest.NewRequest(http.MethodPost, "/search/v1/error", bytes.NewBufferString(`{"error":"client-side test"}`))
	req.Header.Set("Authorization", "Bearer "+userToken)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("search error = %d body=%s", w.Code, w.Body.String())
	}
}

func TestProtocolSearchEndpointsCompactUUIDsForPaperPro(t *testing.T) {
	db, err := notedb.Open(context.Background(), filepath.Join(t.TempDir(), "ultrabridge.db"))
	if err != nil {
		t.Fatalf("open notedb: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	row := source.SourceRow{
		Type:       "remarkable",
		Name:       "RM",
		ConfigJSON: `{"data_path":"` + t.TempDir() + `","pairing_code":"123456"}`,
	}
	src, err := NewSource(db, row, source.SharedDeps{})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if err := src.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(src.Stop)

	docID := "511007fc-5491-491a-baa8-d912b9014457"
	pageID := "0e0ae8be-61b8-4cf0-839e-64ed92d12eea"
	seedSearchNotebook(t, src.store, docID, []string{pageID})
	insertSearchContent(t, src.store, 20, remarkablePath(docID), 0, "compact uuid search", 200)

	mux := http.NewServeMux()
	src.RegisterRoutes(mux)
	userToken := pairUserToken(t, mux, "rm-device-search", "reMarkable Paper Pro")

	req := httptest.NewRequest(http.MethodGet, "/search/v1/delta", nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delta = %d body=%s", w.Code, w.Body.String())
	}
	var delta searchDeltaResponse
	if err := json.NewDecoder(w.Body).Decode(&delta); err != nil {
		t.Fatalf("decode delta: %v", err)
	}
	if len(delta.Changed) != 1 {
		t.Fatalf("changed = %d, want 1", len(delta.Changed))
	}
	change := delta.Changed[0]
	if change.DocumentID != compactSearchID(docID) || change.PageID != compactSearchID(pageID) {
		t.Fatalf("change IDs = %+v, want compact IDs", change)
	}

	req = httptest.NewRequest(http.MethodGet, "/search/v1/"+change.DocumentID+"/"+change.PageID, nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("compact page index = %d body=%s", w.Code, w.Body.String())
	}
}
