package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/sysop/ultrabridge/internal/logging"
	"github.com/sysop/ultrabridge/internal/service"
)

// TestAPISearchSuccess verifies AC3.1: GET /api/search?q=... returns JSON array
func TestAPISearchSuccess(t *testing.T) {
	searchSvc := &mockSearchService{
		embeddingPipelineConfigured: true,
		results: []service.SearchResult{
			{
				Path:    "/home/user/test.note",
				Page:    0,
				Snippet: "This is test content",
				Score:   0.95,
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := NewHandler(nil, nil, searchSvc, nil, nil, "", "", logger, broadcaster)

	req := httptest.NewRequest("GET", "/api/search?q=test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	var results []service.SearchResult
	if err := json.NewDecoder(w.Body).Decode(&results); err != nil {
		t.Fatalf("Failed to decode JSON: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("Expected 1 result, got %d", len(results))
	}

	result := results[0]
	if result.Path != "/home/user/test.note" {
		t.Errorf("Expected path '/home/user/test.note', got %v", result.Path)
	}
	if result.Page != 0 {
		t.Errorf("Expected page 0, got %v", result.Page)
	}
}

// TestAPISearchMissingQ verifies AC3.5: missing q parameter returns 400
func TestAPISearchMissingQ(t *testing.T) {
	handler := newTestHandler()

	req := httptest.NewRequest("GET", "/api/search", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", w.Code)
	}
}

// TestAPIGetPagesSuccess verifies AC3.2: GET /api/notes/pages?path=... returns JSON array
func TestAPIGetPagesSuccess(t *testing.T) {
	notesDir := t.TempDir()
	notePath := filepath.Join(notesDir, "test.note")
	noteSvc := &mockNoteService{
		contents: map[string]interface{}{
			notePath: []map[string]interface{}{
				{
					"page":       0,
					"title_text": "Page 1 Title",
					"body_text":  "Page 1 content",
				},
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := NewHandler(nil, noteSvc, nil, nil, nil, notesDir, "", logger, broadcaster)

	req := httptest.NewRequest("GET", "/api/notes/pages?path="+url.QueryEscape(notePath), nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	var pages []map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&pages); err != nil {
		t.Fatalf("Failed to decode JSON: %v", err)
	}

	if len(pages) != 1 {
		t.Fatalf("Expected 1 page, got %d", len(pages))
	}
}

// TestAPIGetPagesMissingPath verifies missing path parameter returns 400
func TestAPIGetPagesMissingPath(t *testing.T) {
	handler := newTestHandler()

	req := httptest.NewRequest("GET", "/api/notes/pages", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", w.Code)
	}
}

// TestAPIGetImageMissingPath verifies missing path parameter returns 400
func TestAPIGetImageMissingPath(t *testing.T) {
	handler := newTestHandler()

	req := httptest.NewRequest("GET", "/api/notes/pages/image?page=0", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", w.Code)
	}
}

// TestAPIGetImageMissingPage verifies missing page parameter returns 400
func TestAPIGetImageMissingPage(t *testing.T) {
	notesDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := NewHandler(nil, &mockNoteService{
		docs:     make(map[string][]service.SearchResult),
		contents: make(map[string]interface{}),
	}, nil, nil, nil, notesDir, "", logger, broadcaster)

	notePath := filepath.Join(notesDir, "test.note")
	req := httptest.NewRequest("GET", "/api/notes/pages/image?path="+url.QueryEscape(notePath), nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", w.Code)
	}
}

// TestAPIResponseContentType verifies JSON content-type header
func TestAPIResponseContentType(t *testing.T) {
	handler := newTestHandler()

	req := httptest.NewRequest("GET", "/api/search?q=test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	contentType := w.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("Expected Content-Type 'application/json', got '%s'", contentType)
	}
}
