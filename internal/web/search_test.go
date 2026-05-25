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
	"github.com/sysop/ultrabridge/internal/search"
)

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

// boox-notes-pipeline.AC6.2: Search results include source badges (Boox and Supernote)
func TestSearchPage_SourceBadges(t *testing.T) {
	// Set up handler with search index returning results from both sources
	searchIdx := &configSearchIndex{
		results: []search.SearchResult{
			{
				Path:    "/boox/notes/test.note",
				Page:    0,
				Snippet: "test boox content here",
				Score:   -1.5,
			},
			{
				Path:    "/notes/supernote.note",
				Page:    0,
				Snippet: "test supernote content here",
				Score:   -1.6,
			},
		},
	}

	booxNotesPath := "/boox"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := LegacyNewHandler(
		newMockTaskStore(),
		&mockNotifier{},
		newMockNoteStore(),
		searchIdx,
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
		nil, // embedder
		nil, // embedStore
		"",  // embedModel
		nil, // retriever
		nil, // chatHandler
		nil, // chatStore
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
