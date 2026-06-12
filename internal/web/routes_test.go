package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sysop/ultrabridge/internal/logging"
)

func TestRoutes(t *testing.T) {
	tests := []struct {
		method string
		path   string
		status int
	}{
		{"GET", "/", http.StatusOK},
		{"GET", "/files", http.StatusSeeOther}, // legacy route redirects to /files/supernote
		{"GET", "/files/supernote", http.StatusOK},
		{"GET", "/files/boox", http.StatusOK},
		{"GET", "/search", http.StatusOK},
		{"GET", "/settings", http.StatusSeeOther}, // legacy entry point redirects to /settings/devices
		{"GET", "/settings/devices", http.StatusOK},
		{"GET", "/settings/ai", http.StatusOK},
		{"GET", "/settings/integrations", http.StatusOK},
		{"GET", "/settings/system", http.StatusOK},
		{"GET", "/chat", http.StatusOK},
		{"GET", "/nonexistent", http.StatusNotFound},
	}

	handler := newTestHandler()

	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != tt.status {
			t.Errorf("%s %s returned status %d, want %d", tt.method, tt.path, w.Code, tt.status)
		}
	}

	// The legacy /settings redirect lands on the default group.
	req := httptest.NewRequest("GET", "/settings", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if loc := w.Header().Get("Location"); loc != "/settings/devices" {
		t.Errorf("GET /settings Location = %q, want /settings/devices", loc)
	}
}

func TestSectionVisibility(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()

	tasks := &mockTaskService{}
	notes := &mockNoteService{}
	search := &mockSearchService{}
	config := &mockConfigService{}

	handler := NewHandler(tasks, notes, search, config, nil, "", "", logger, broadcaster)

	t.Run("RAG Search not configured", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/settings/devices", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		// Assuming the template renders "not configured" message when some flag is false
		// For now we just verify it doesn't crash
		if w.Code != http.StatusOK {
			t.Errorf("GET /settings/devices returned %d", w.Code)
		}
	})
}
