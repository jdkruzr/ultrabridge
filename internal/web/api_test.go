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
	"time"

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
	if searchSvc.lastOpts.Mode != "" {
		t.Errorf("API search without mode should leave mode empty for service hybrid default, got %q", searchSvc.lastOpts.Mode)
	}
}

func TestAPISearch_ModeParamThreadsThrough(t *testing.T) {
	svc := &mockSearchService{embeddingPipelineConfigured: true}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := NewHandler(nil, nil, svc, nil, nil, "", "", logger, broadcaster)

	req := httptest.NewRequest("GET", "/api/search?q=test&mode=keyword", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	if svc.lastOpts.Mode != service.SearchModeKeyword {
		t.Fatalf("API mode = %q, want keyword", svc.lastOpts.Mode)
	}
}

func TestAPISearch_FilterParamsThreadThrough(t *testing.T) {
	svc := &mockSearchService{embeddingPipelineConfigured: true}
	handler := NewHandler(nil, nil, svc, nil, nil, "", "", slog.New(slog.NewTextHandler(io.Discard, nil)), logging.NewLogBroadcaster())

	req := httptest.NewRequest("GET", "/api/search?q=test&source=boox&source=forestnote&folder=Work&device_model=Palma2&created_from=2026-06-01&modified_to=2026-06-30&sort=date_desc", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := svc.lastOpts.Sources; len(got) != 2 || got[0] != "boox" || got[1] != "forestnote" {
		t.Fatalf("sources = %v, want [boox forestnote]", got)
	}
	if svc.lastOpts.Folder != "Work" {
		t.Fatalf("folder = %q, want Work", svc.lastOpts.Folder)
	}
	if svc.lastOpts.DeviceModel != "Palma2" {
		t.Fatalf("device model = %q, want Palma2", svc.lastOpts.DeviceModel)
	}
	if svc.lastOpts.CreatedFrom.IsZero() || svc.lastOpts.CreatedFrom.Format(time.DateOnly) != "2026-06-01" {
		t.Fatalf("created_from = %s, want 2026-06-01", svc.lastOpts.CreatedFrom)
	}
	if svc.lastOpts.ModifiedTo.IsZero() || svc.lastOpts.ModifiedTo.Format(time.DateOnly) != "2026-06-30" {
		t.Fatalf("modified_to = %s, want 2026-06-30", svc.lastOpts.ModifiedTo)
	}
	if svc.lastOpts.Sort != "date_desc" {
		t.Fatalf("sort = %q, want date_desc", svc.lastOpts.Sort)
	}
}

func TestAPISearch_DeprecatedAliasesThreadThrough(t *testing.T) {
	svc := &mockSearchService{embeddingPipelineConfigured: true}
	handler := NewHandler(nil, nil, svc, nil, nil, "", "", slog.New(slog.NewTextHandler(io.Discard, nil)), logging.NewLogBroadcaster())

	req := httptest.NewRequest("GET", "/api/search?q=test&device=Palma2&date_from=2026-01-01&date_to=2026-01-31", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	if svc.lastOpts.DeviceModel != "Palma2" {
		t.Fatalf("device alias = %q, want Palma2", svc.lastOpts.DeviceModel)
	}
	if svc.lastOpts.ModifiedFrom.IsZero() || svc.lastOpts.ModifiedFrom.Format(time.DateOnly) != "2026-01-01" {
		t.Fatalf("date_from alias = %s, want 2026-01-01", svc.lastOpts.ModifiedFrom)
	}
	if svc.lastOpts.ModifiedTo.IsZero() || svc.lastOpts.ModifiedTo.Format(time.DateOnly) != "2026-01-31" {
		t.Fatalf("date_to alias = %s, want 2026-01-31", svc.lastOpts.ModifiedTo)
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

// TestAPISearch_LimitParamThreadsThrough is the regression for the
// QA-found "search_notes ignores limit" bug (UB-1). The handler previously
// dropped the ?limit= query param entirely; SearchService.Search has a
// new limit arg now and the mock captures lastLimit so we can assert the
// param round-tripped.
func TestAPISearch_LimitParamThreadsThrough(t *testing.T) {
	t.Run("integer ?limit= reaches the service", func(t *testing.T) {
		svc := &mockSearchService{embeddingPipelineConfigured: true}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		broadcaster := logging.NewLogBroadcaster()
		handler := NewHandler(nil, nil, svc, nil, nil, "", "", logger, broadcaster)

		req := httptest.NewRequest("GET", "/api/search?q=test&limit=3", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
		}
		if svc.lastLimit != 3 {
			t.Errorf("lastLimit: got %d, want 3", svc.lastLimit)
		}
	})

	t.Run("absent ?limit= passes 0 (service-default)", func(t *testing.T) {
		// Pre-seed lastLimit with a sentinel so a "Search was never called"
		// regression is distinguishable from "Search was called with 0" —
		// without this both look like lastLimit==0 to the assertion.
		svc := &mockSearchService{embeddingPipelineConfigured: true, lastLimit: -1}
		handler := NewHandler(nil, nil, svc, nil, nil, "", "", slog.New(slog.NewTextHandler(io.Discard, nil)), logging.NewLogBroadcaster())

		req := httptest.NewRequest("GET", "/api/search?q=test", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200", w.Code)
		}
		if svc.lastLimit != 0 {
			t.Errorf("lastLimit: got %d, want 0 (service-default sentinel); -1 means Search was never invoked",
				svc.lastLimit)
		}
	})

	t.Run("non-integer ?limit= is tolerated as 0", func(t *testing.T) {
		svc := &mockSearchService{embeddingPipelineConfigured: true}
		handler := NewHandler(nil, nil, svc, nil, nil, "", "", slog.New(slog.NewTextHandler(io.Discard, nil)), logging.NewLogBroadcaster())

		// Per the handler doc — non-integer ?limit is treated as 0, not a 400,
		// since MCP callers sometimes stringify ints loosely.
		req := httptest.NewRequest("GET", "/api/search?q=test&limit=banana", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("non-integer limit should not 400; got %d", w.Code)
		}
		if svc.lastLimit != 0 {
			t.Errorf("lastLimit: got %d, want 0", svc.lastLimit)
		}
	})

	t.Run("negative ?limit= is tolerated as 0", func(t *testing.T) {
		svc := &mockSearchService{embeddingPipelineConfigured: true}
		handler := NewHandler(nil, nil, svc, nil, nil, "", "", slog.New(slog.NewTextHandler(io.Discard, nil)), logging.NewLogBroadcaster())

		req := httptest.NewRequest("GET", "/api/search?q=test&limit=-5", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200", w.Code)
		}
		if svc.lastLimit != 0 {
			t.Errorf("lastLimit: got %d, want 0", svc.lastLimit)
		}
	})
}
