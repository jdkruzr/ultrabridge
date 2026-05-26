package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/appconfig"
	"github.com/sysop/ultrabridge/internal/booxpipeline"
	"github.com/sysop/ultrabridge/internal/logging"
	"github.com/sysop/ultrabridge/internal/notestore"
	"github.com/sysop/ultrabridge/internal/service"
)

// mockBooxStore implements BooxStore for testing
type mockBooxStore struct {
	notes    []booxpipeline.BooxNoteEntry
	versions []booxpipeline.BooxVersion
	noteIDs  map[string]string // path -> noteID
	err      error
}

func (m *mockBooxStore) ListNotes(ctx context.Context) ([]booxpipeline.BooxNoteEntry, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.notes, nil
}

func (m *mockBooxStore) GetVersions(ctx context.Context, path string) ([]booxpipeline.BooxVersion, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.versions, nil
}

func (m *mockBooxStore) GetNoteID(ctx context.Context, path string) (string, error) {
	if noteID, ok := m.noteIDs[path]; ok {
		return noteID, nil
	}
	return "", fmt.Errorf("note not found")
}

func (m *mockBooxStore) EnqueueJob(ctx context.Context, notePath string) error {
	return nil
}

func (m *mockBooxStore) GetLatestJob(ctx context.Context, notePath string) (*booxpipeline.BooxJob, error) {
	return nil, nil
}

func (m *mockBooxStore) RetryAllFailed(ctx context.Context) (int64, error) {
	return 0, nil
}

func (m *mockBooxStore) SkipNote(ctx context.Context, path, reason string) error {
	return nil
}

func (m *mockBooxStore) UnskipNote(ctx context.Context, path string) error {
	return nil
}

func (m *mockBooxStore) DeleteNote(ctx context.Context, path string) error {
	return nil
}

func (m *mockBooxStore) GetQueueStatus(ctx context.Context) (booxpipeline.QueueStatus, error) {
	return booxpipeline.QueueStatus{}, nil
}

func (m *mockBooxStore) ListFolders(ctx context.Context) ([]booxpipeline.FolderCount, error) {
	counts := map[string]int{}
	for _, bn := range m.notes {
		counts[bn.Folder]++
	}
	var out []booxpipeline.FolderCount
	for f, c := range counts {
		out = append(out, booxpipeline.FolderCount{Folder: f, Count: c})
	}
	return out, nil
}

func (m *mockBooxStore) ListDevices(ctx context.Context) ([]booxpipeline.DeviceCount, error) {
	counts := map[string]int{}
	for _, bn := range m.notes {
		if bn.DeviceModel == ".." {
			continue
		}
		counts[bn.DeviceModel]++
	}
	var out []booxpipeline.DeviceCount
	for d, c := range counts {
		out = append(out, booxpipeline.DeviceCount{DeviceModel: d, Count: c})
	}
	return out, nil
}

func (m *mockBooxStore) CountNotesWithPrefix(ctx context.Context, prefix string) (int, error) {
	return 0, nil
}

func (m *mockBooxStore) ReconcileCreatedAtFromFilename(ctx context.Context) (int64, error) {
	return 0, nil
}

func (m *mockBooxStore) ListAutoNamedNotebooks(ctx context.Context) ([]booxpipeline.BooxNote, error) {
	return nil, nil
}

func (m *mockBooxStore) MoveNote(ctx context.Context, oldPath, newPath, newFolder string) error {
	return nil
}

// TestFilesPage_ShowsBothSources verifies AC5.1: Files list shows both Supernote and Boox notes
func TestFilesPage_ShowsBothSources(t *testing.T) {
	// Create mock stores
	noteStore := newMockNoteStore()
	booxStore := &mockBooxStore{
		notes: []booxpipeline.BooxNoteEntry{
			{
				Path:        "/boox/notes/note1.note",
				Title:       "Boox Note 1",
				DeviceModel: "Page",
				NoteType:    "Standard",
				Folder:      "Inbox",
				PageCount:   5,
				Version:     1,
				NoteID:      "note1-id",
				UpdatedAt:   1700000000000,
				JobStatus:   "done",
			},
			{
				Path:        "/boox/notes/note2.note",
				Title:       "Boox Note 2",
				DeviceModel: "Page",
				NoteType:    "Standard",
				Folder:      "Inbox",
				PageCount:   3,
				Version:     1,
				NoteID:      "note2-id",
				UpdatedAt:   1700000001000,
				JobStatus:   "pending",
			},
		},
		noteIDs: map[string]string{
			"/boox/notes/note1.note": "note1-id",
			"/boox/notes/note2.note": "note2-id",
		},
	}

	// Add Supernote notes
	noteStore.files[""] = []notestore.NoteFile{
		{
			Path:      "/sn/notes/note1.note",
			RelPath:   "note1.note",
			Name:      "note1.note",
			IsDir:     false,
			FileType:  notestore.FileTypeNote,
			JobStatus: "done",
		},
		{
			Path:      "/sn/notes/note2.note",
			RelPath:   "note2.note",
			Name:      "note2.note",
			IsDir:     false,
			FileType:  notestore.FileTypeNote,
			JobStatus: "done",
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := LegacyNewHandler(newMockTaskStore(), nil, noteStore, nil, nil, nil, nil, booxStore, nil, "/boox/notes", "", nil, logger, broadcaster, nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

	// Post-split: /files/supernote renders SN entries only; /files/boox
	// renders Boox entries only. This test verifies both tabs surface
	// their respective sources.
	snReq := httptest.NewRequest("GET", "/files/supernote", nil)
	snW := httptest.NewRecorder()
	handler.ServeHTTP(snW, snReq)
	if snW.Code != http.StatusOK {
		t.Fatalf("GET /files/supernote returned status %d", snW.Code)
	}
	snBody := snW.Body.String()
	if !strings.Contains(snBody, "note1.note") {
		t.Errorf("Supernote tab missing note1.note; body:\n%s", snBody)
	}
	if !strings.Contains(snBody, "note2.note") {
		t.Errorf("Supernote tab missing note2.note; body:\n%s", snBody)
	}

	booxReq := httptest.NewRequest("GET", "/files/boox", nil)
	booxW := httptest.NewRecorder()
	handler.ServeHTTP(booxW, booxReq)
	if booxW.Code != http.StatusOK {
		t.Fatalf("GET /files/boox returned status %d", booxW.Code)
	}
	booxBody := booxW.Body.String()
	if !strings.Contains(booxBody, "Boox Note 1") {
		t.Errorf("Boox tab missing 'Boox Note 1'; body:\n%s", booxBody)
	}
	if !strings.Contains(booxBody, "Boox Note 2") {
		t.Errorf("Boox tab missing 'Boox Note 2'; body:\n%s", booxBody)
	}
}

// TestBooxRender_ServesCache verifies AC5.2: GET /files/boox/render serves cached JPEG page images
func TestBooxRender_ServesCache(t *testing.T) {
	tmpDir := t.TempDir()
	notePath := filepath.Join(tmpDir, "test.note")
	noteID := "test-note-id"
	cacheDir := filepath.Join(tmpDir, ".cache", noteID)

	// Create cache directory and page file
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}

	testJPEG := []byte{0xFF, 0xD8, 0xFF, 0xE0} // JPEG magic bytes
	pageFile := filepath.Join(cacheDir, "page_0.jpg")
	if err := os.WriteFile(pageFile, testJPEG, 0644); err != nil {
		t.Fatalf("failed to write test JPEG: %v", err)
	}

	booxStore := &mockBooxStore{
		noteIDs: map[string]string{notePath: noteID},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := LegacyNewHandler(newMockTaskStore(), nil, nil, nil, nil, nil, nil, booxStore, nil, tmpDir, "", nil, logger, broadcaster, nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

	req := httptest.NewRequest("GET", "/files/boox/render?path="+url.QueryEscape(notePath)+"&page=0", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /files/boox/render returned status %d, want 200", w.Code)
	}

	if w.Header().Get("Content-Type") != "image/jpeg" {
		t.Errorf("Content-Type = %s, want image/jpeg", w.Header().Get("Content-Type"))
	}

	if !strings.Contains(w.Header().Get("Cache-Control"), "public") {
		t.Errorf("Cache-Control should be 'public, max-age=300', got %s", w.Header().Get("Cache-Control"))
	}

	body := w.Body.Bytes()
	if !strings.HasPrefix(string(body[:4]), string(testJPEG[:4])) {
		t.Errorf("Response body should be JPEG data starting with %v, got %v", testJPEG[:4], body[:4])
	}
}

// TestBooxRender_MissingNote verifies 404 when note not found
func TestBooxRender_MissingNote(t *testing.T) {
	tmpDir := t.TempDir()
	booxStore := &mockBooxStore{
		noteIDs: map[string]string{}, // empty
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := LegacyNewHandler(newMockTaskStore(), nil, nil, nil, nil, nil, nil, booxStore, nil, tmpDir, "", nil, logger, broadcaster, nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

	req := httptest.NewRequest("GET", "/files/boox/render?path="+url.QueryEscape(filepath.Join(tmpDir, "test.note"))+"&page=0", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("GET /files/boox/render (missing note) returned status %d, want 404", w.Code)
	}
}

// TestBooxRender_MissingPage verifies 404 when page not rendered yet
func TestBooxRender_MissingPage(t *testing.T) {
	tmpDir := t.TempDir()
	notePath := filepath.Join(tmpDir, "test.note")
	noteID := "test-note-id"
	cacheDir := filepath.Join(tmpDir, ".cache", noteID)

	// Create cache directory but don't create the page file
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}

	booxStore := &mockBooxStore{
		noteIDs: map[string]string{notePath: noteID},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := LegacyNewHandler(newMockTaskStore(), nil, nil, nil, nil, nil, nil, booxStore, nil, tmpDir, "", nil, logger, broadcaster, nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

	req := httptest.NewRequest("GET", "/files/boox/render?path="+url.QueryEscape(notePath)+"&page=0", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("GET /files/boox/render (missing page) returned status %d, want 404", w.Code)
	}
}

// TestBooxVersions_ReturnsList verifies AC5.3: GET /files/boox/versions returns version list
func TestBooxVersions_ReturnsList(t *testing.T) {
	booxStore := &mockBooxStore{
		versions: []booxpipeline.BooxVersion{
			{
				Path:      "/boox/notes/.versions/test/20260404T100000.note",
				Timestamp: "20260404T100000",
				SizeBytes: 1024,
			},
			{
				Path:      "/boox/notes/.versions/test/20260404T110000.note",
				Timestamp: "20260404T110000",
				SizeBytes: 2048,
			},
			{
				Path:      "/boox/notes/.versions/test/20260404T120000.note",
				Timestamp: "20260404T120000",
				SizeBytes: 3072,
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := LegacyNewHandler(newMockTaskStore(), nil, nil, nil, nil, nil, nil, booxStore, nil, "/boox/notes", "", nil, logger, broadcaster, nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

	req := httptest.NewRequest("GET", "/files/boox/versions?path=%2Fboox%2Fnotes%2Ftest.note", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /files/boox/versions returned status %d, want 200", w.Code)
	}

	if !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
		t.Errorf("Content-Type should be JSON, got %s", w.Header().Get("Content-Type"))
	}

	body := w.Body.String()
	if !strings.Contains(body, "20260404T100000") {
		t.Errorf("Response should contain first version timestamp, got:\n%s", body)
	}
	if !strings.Contains(body, "20260404T110000") {
		t.Errorf("Response should contain second version timestamp, got:\n%s", body)
	}
	if !strings.Contains(body, "20260404T120000") {
		t.Errorf("Response should contain third version timestamp, got:\n%s", body)
	}

	// Verify it's valid JSON with correct count
	var versions []booxpipeline.BooxVersion
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&versions); err != nil {
		t.Fatalf("Failed to decode JSON response: %v", err)
	}

	if len(versions) != 3 {
		t.Errorf("Expected 3 versions in response, got %d", len(versions))
	}
}

// TestBooxVersions_EmptyList verifies empty list when no versions
func TestBooxVersions_EmptyList(t *testing.T) {
	booxStore := &mockBooxStore{
		versions: []booxpipeline.BooxVersion{},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := LegacyNewHandler(newMockTaskStore(), nil, nil, nil, nil, nil, nil, booxStore, nil, "/boox/notes", "", nil, logger, broadcaster, nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

	req := httptest.NewRequest("GET", "/files/boox/versions?path=%2Fboox%2Fnotes%2Ftest.note", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /files/boox/versions (empty) returned status %d, want 200", w.Code)
	}

	body := w.Body.String()
	var versions []booxpipeline.BooxVersion
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&versions); err != nil {
		t.Fatalf("Failed to decode JSON response: %v", err)
	}

	if len(versions) != 0 {
		t.Errorf("Expected 0 versions, got %d", len(versions))
	}
}

// TestBooxVersions_NoBooxStore verifies empty list when booxStore is nil
func TestBooxVersions_NoBooxStore(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := LegacyNewHandler(newMockTaskStore(), nil, nil, nil, nil, nil, nil, nil, nil, "", "", nil, logger, broadcaster, nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

	req := httptest.NewRequest("GET", "/files/boox/versions?path=%2Fboox%2Fnotes%2Ftest.note", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /files/boox/versions (nil store) returned status %d, want 200", w.Code)
	}

	body := w.Body.String()
	var versions []interface{}
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&versions); err != nil {
		t.Fatalf("Failed to decode JSON response: %v", err)
	}

	if len(versions) != 0 {
		t.Errorf("Expected empty list when booxStore is nil, got %d items", len(versions))
	}
}

// TestFilesPage_NoBooxNotes verifies AC5.5: Files list works when Boox enabled but no notes exist
func TestFilesPage_NoBooxNotes(t *testing.T) {
	// Create mock stores
	noteStore := newMockNoteStore()
	booxStore := &mockBooxStore{
		notes: []booxpipeline.BooxNoteEntry{}, // empty
	}

	// Add only Supernote notes
	noteStore.files[""] = []notestore.NoteFile{
		{
			Path:      "/sn/notes/note1.note",
			RelPath:   "note1.note",
			Name:      "note1.note",
			IsDir:     false,
			FileType:  notestore.FileTypeNote,
			JobStatus: "done",
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler := LegacyNewHandler(newMockTaskStore(), nil, noteStore, nil, nil, nil, nil, booxStore, nil, "/boox/notes", "", nil, logger, broadcaster, nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

	req := httptest.NewRequest("GET", "/files/supernote", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /files returned status %d, want 200", w.Code)
	}

	body := w.Body.String()

	// Verify Supernote entry is present
	if !strings.Contains(body, "note1.note") {
		t.Errorf("Response should contain Supernote 'note1.note', got:\n%s", body)
	}

	// Verify page renders without error
	if !strings.Contains(body, "Files") && !strings.Contains(body, "file") {
		t.Errorf("Response should render Files page correctly, got:\n%s", body)
	}
}

// TestFilesPage_NoBooxStore verifies files page works when booxStore is nil (Boox disabled)
func TestFilesPage_NoBooxStore(t *testing.T) {
	noteStore := newMockNoteStore()
	noteStore.files[""] = []notestore.NoteFile{
		{
			Path:      "/sn/notes/note1.note",
			RelPath:   "note1.note",
			Name:      "note1.note",
			IsDir:     false,
			FileType:  notestore.FileTypeNote,
			JobStatus: "done",
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	handler2 := LegacyNewHandler(newMockTaskStore(), nil, noteStore, nil, nil, nil, nil, nil, nil, "", "", nil, logger, broadcaster, nil, nil, "", nil, nil, nil, RAGDisplayConfig{}, &appconfig.Config{})

	req := httptest.NewRequest("GET", "/files/supernote", nil)
	w := httptest.NewRecorder()
	handler2.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /files returned status %d, want 200", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "note1.note") {
		t.Errorf("Response should contain 'note1.note', got:\n%s", body)
	}
}

// TestNoteSourceFunction verifies search results render a per-source badge.
// Badges now derive from each result's SourceType (set by the hybrid
// retriever / service), not from a path-prefix guess.
func TestNoteSourceFunction(t *testing.T) {
	handler := newTestHandler()
	handler.booxNotesPath = "/boox/notes"
	search := handler.search.(*mockSearchService)
	search.results = []service.SearchResult{
		{Path: "/boox/notes/one.note", Snippet: "boox snippet", SourceType: "boox"},
		{Path: "/sn/notes/two.note", Snippet: "sn snippet", SourceType: "supernote"},
	}

	req := httptest.NewRequest("GET", "/search?q=foo", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /search returned %d; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "badge-boox") {
		t.Errorf("search results should tag Boox path; body:\n%s", body)
	}
	if !strings.Contains(body, "badge-sn") {
		t.Errorf("search results should tag Supernote path; body:\n%s", body)
	}
}
