package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/appconfig"
	"github.com/sysop/ultrabridge/internal/logging"
	"github.com/sysop/ultrabridge/internal/rag"
	"github.com/sysop/ultrabridge/internal/search"
)

// fakeRetriever returns canned hybrid-search results for tests.
type fakeRetriever struct{ results []rag.SearchResult }

func (f *fakeRetriever) Search(_ context.Context, _ rag.SearchRequest) ([]rag.SearchResult, error) {
	return f.results, nil
}

// configSearchIndex implements SearchIndex with configurable results for testing
type configSearchIndex struct {
	results []search.SearchResult
}

func (c *configSearchIndex) Index(_ context.Context, _ search.NoteDocument) error { return nil }
func (c *configSearchIndex) Search(_ context.Context, q search.SearchQuery) ([]search.SearchResult, error) {
	return c.results, nil
}
func (c *configSearchIndex) Delete(_ context.Context, _ string) error { return nil }
func (c *configSearchIndex) IndexPage(_ context.Context, _ string, _ int, _, _, _, _ string) error {
	return nil
}
func (c *configSearchIndex) GetContent(_ context.Context, _ string) ([]search.NoteDocument, error) {
	return nil, nil
}
func (c *configSearchIndex) ListFolders(_ context.Context) ([]string, error) {
	return nil, nil
}

// boox-notes-pipeline.AC6.2: Search results include source badges. Badges now
// derive from each result's SourceType (set by the hybrid retriever), not from
// a path-prefix guess — so this drives the page through a fake retriever.
func TestSearchPage_SourceBadges(t *testing.T) {
	retriever := &fakeRetriever{
		results: []rag.SearchResult{
			{NotePath: "/boox/notes/test.note", Page: 0, BodyText: "test boox content here", SourceType: rag.SourceBoox, Score: -1.5},
			{NotePath: "/notes/supernote.note", Page: 0, BodyText: "test supernote content here", SourceType: rag.SourceSupernote, Score: -1.6},
		},
	}

	booxNotesPath := "/boox"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := LegacyNewHandler(
		newMockTaskStore(),
		&mockNotifier{},
		newMockNoteStore(),
		nil, // searchIndex (unused; retriever drives the search tab now)
		newMockProcessor(),
		&mockScanner{},
		nil, // syncProvider removed
		nil, // booxStore not needed for this test
		nil, // booxImporter not needed for this test
		booxNotesPath,
		"",  // notesPathPrefix
		nil, // noteDB
		logger,
		broadcaster,
		nil,       // embedder
		nil,       // embedStore
		"",        // embedModel
		retriever, // hybrid retriever
		nil,       // chatHandler
		nil,       // chatStore
		RAGDisplayConfig{},
		&appconfig.Config{},
	)

	// Execute GET /search?q=test
	req := httptest.NewRequest("GET", "/search?q=test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Verify response status
	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	respBody := w.Body.String()

	// Verify badge-boox class is present (for Boox source)
	if !strings.Contains(respBody, "badge-boox") {
		t.Error("expected 'badge-boox' CSS class in response for Boox note")
	}

	// Verify badge-sn class is present (for Supernote source)
	if !strings.Contains(respBody, "badge-sn") {
		t.Error("expected 'badge-sn' CSS class in response for Supernote note")
	}

	// Verify both file paths are in the response
	if !strings.Contains(respBody, "/boox/notes/test.note") {
		t.Error("expected boox file path in response")
	}
	if !strings.Contains(respBody, "/notes/supernote.note") {
		t.Error("expected supernote file path in response")
	}
}

func TestSearchPage_DefaultsEnabledSourcesChecked(t *testing.T) {
	handler := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/search", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /search = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`name="source" value="supernote" checked`, `name="source" value="boox" checked`} {
		if !strings.Contains(body, want) {
			t.Fatalf("search page missing checked source %q in:\n%s", want, body)
		}
	}
}

func TestSearchPage_AllUncheckedSubmissionShowsValidation(t *testing.T) {
	handler := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/search?q=alpha&sources_submitted=1", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /search all unchecked = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Select at least one source to search.") {
		t.Fatalf("expected source validation, got:\n%s", body)
	}
	if strings.Contains(body, `value="supernote" checked`) || strings.Contains(body, `value="boox" checked`) {
		t.Fatalf("unchecked submission should not re-check sources:\n%s", body)
	}
}
