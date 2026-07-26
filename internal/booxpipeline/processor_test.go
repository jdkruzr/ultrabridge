package booxpipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/booxnote"
	pb "github.com/sysop/ultrabridge/internal/booxnote/proto"
	"github.com/sysop/ultrabridge/internal/booxnote/testutil"
	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/processor"
)

// mockIndexer records IndexPage calls for test assertion.
type mockIndexer struct {
	calls []indexCall
}

type indexCall struct {
	path      string
	page      int
	source    string
	bodyText  string
	titleText string
}

func (m *mockIndexer) IndexPage(_ context.Context, path string, page int, source, bodyText, titleText, _ string) error {
	m.calls = append(m.calls, indexCall{path, page, source, bodyText, titleText})
	return nil
}

// mockContentDeleter records Delete calls.
type mockContentDeleter struct {
	deletedPaths []string
}

func (m *mockContentDeleter) Delete(_ context.Context, path string) error {
	m.deletedPaths = append(m.deletedPaths, path)
	return nil
}

// mockOCRServer returns a fixed JSON response matching the Anthropic Messages API format.
func mockOCRServer(t *testing.T, responseText string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		type mockResp struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		resp := mockResp{
			Content: []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{
				{Type: "text", Text: responseText},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
}

// openTestProcessor creates a Processor with an in-memory DB and temp directory.
// It does NOT start the processor - the caller should do that if needed.
// NOTE: The caller is responsible for calling proc.Stop() and db.Close()
// to ensure proper cleanup.
func openTestProcessor(t *testing.T, indexer *mockIndexer, contentDeleter *mockContentDeleter, ocr *processor.OCRClient) (*Processor, *sql.DB) {
	t.Helper()

	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}

	notesPath := t.TempDir()
	cachePath := filepath.Join(notesPath, ".cache")

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg := WorkerConfig{
		Indexer:        indexer,
		ContentDeleter: contentDeleter,
		OCR:            ocr,
		CachePath:      cachePath,
	}

	proc := New(db, notesPath, cfg, logger)
	return proc, db
}

// waitForJobStatus polls the database until a job reaches the desired status, with a timeout.
// Returns true if the status was reached, false if timeout occurs.
func waitForJobStatus(t *testing.T, db *sql.DB, notePath string, desiredStatus string, timeout time.Duration) bool {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		var jobStatus string
		err := db.QueryRowContext(context.Background(),
			"SELECT status FROM boox_jobs WHERE note_path = ? ORDER BY id DESC LIMIT 1", notePath).Scan(&jobStatus)

		if err == sql.ErrNoRows {
			// Job not yet in database, keep polling
		} else if err != nil {
			t.Logf("query job status: %v", err)
		} else if jobStatus == desiredStatus {
			return true
		}

		if time.Now().After(deadline) {
			return false
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// boox-notes-pipeline.AC4.2: TestProcessor_EndToEnd verifies parse → render → OCR → index pipeline
func TestProcessor_EndToEnd(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a synthetic .note file with one page
	noteID := "test-note-e2e"
	opts := testutil.TestNoteOpts{
		NoteID: noteID,
		Title:  "End to End Test",
		Pages: []*testutil.TestPage{
			{
				PageID: "page-1",
				Width:  1404,
				Height: 1872,
				Shapes: []*pb.ShapeInfoProto{
					{
						UniqueId:  "shape-1",
						ShapeType: 0,
						Color:     -16777216, // 0xFF000000 as signed int32
						Thickness: 1.0,
						Zorder:    0,
					},
				},
				Points: map[string][]booxnote.TinyPoint{
					"shape-1": {
						{X: 100.0, Y: 100.0, Size: 1, Pressure: 100, Time: 0},
						{X: 101.0, Y: 101.0, Size: 1, Pressure: 100, Time: 1},
					},
				},
			},
		},
	}
	notePath := testutil.BuildTestNoteFile(t, tmpDir, opts)

	// Create mock indexer and deleter
	indexer := &mockIndexer{}
	deleter := &mockContentDeleter{}

	// Create mock OCR server
	ocrServer := mockOCRServer(t, "Page content from OCR")
	defer ocrServer.Close()

	// Create OCR client pointing to mock server
	ocrClient := processor.NewOCRClient(ocrServer.URL, "test-key", "test-model", "anthropic")

	// Open processor and start it
	proc, db := openTestProcessor(t, indexer, deleter, ocrClient)
	defer db.Close()
	defer proc.Stop()
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Enqueue the job
	if err := proc.Enqueue(context.Background(), notePath); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Wait for job to complete (OCR may take a few seconds)
	if !waitForJobStatus(t, db, notePath, "done", 10*time.Second) {
		t.Fatalf("job did not complete in time")
	}

	// Stop the processor to clean up gracefully
	proc.Stop()

	// Verify cached JPEGs exist
	cacheDir := filepath.Join(proc.cfg.CachePath, noteID)
	pageJPEG := filepath.Join(cacheDir, "page_0.jpg")
	if _, err := os.Stat(pageJPEG); err != nil {
		t.Errorf("cached JPEG not found: %v", err)
	}

	// Verify mockIndexer received IndexPage calls
	if len(indexer.calls) == 0 {
		t.Errorf("indexer received no IndexPage calls, want at least 1")
	}
}

// boox-notes-pipeline.AC4.3: TestProcessor_IndexesContent verifies content is indexed correctly
func TestProcessor_IndexesContent(t *testing.T) {
	tmpDir := t.TempDir()

	noteID := "test-note-index"
	opts := testutil.TestNoteOpts{
		NoteID: noteID,
		Title:  "Index Test",
		Pages: []*testutil.TestPage{
			{
				PageID: "page-1",
				Width:  1404,
				Height: 1872,
				Shapes: []*pb.ShapeInfoProto{
					{
						UniqueId:  "shape-1",
						ShapeType: 0,
						Color:     -16777216,
						Thickness: 1.0,
					},
				},
				Points: map[string][]booxnote.TinyPoint{
					"shape-1": {
						{X: 50.0, Y: 50.0, Size: 1, Pressure: 100, Time: 0},
					},
				},
			},
		},
	}
	notePath := testutil.BuildTestNoteFile(t, tmpDir, opts)

	indexer := &mockIndexer{}
	deleter := &mockContentDeleter{}

	// Create mock OCR
	ocrServer := mockOCRServer(t, "OCR text from vision API")
	defer ocrServer.Close()
	ocrClient := processor.NewOCRClient(ocrServer.URL, "test-key", "test-model", "anthropic")

	proc, db := openTestProcessor(t, indexer, deleter, ocrClient)
	defer db.Close()
	defer proc.Stop()
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := proc.Enqueue(context.Background(), notePath); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Wait for job completion (OCR may take a few seconds)
	if !waitForJobStatus(t, db, notePath, "done", 10*time.Second) {
		t.Fatalf("job did not complete in time")
	}

	// Verify indexer received calls with correct parameters
	if len(indexer.calls) == 0 {
		t.Errorf("indexer calls = 0, want > 0")
	}
	for _, call := range indexer.calls {
		if call.path != notePath {
			t.Errorf("indexer call path = %q, want %q", call.path, notePath)
		}
		if call.source != "api" {
			t.Errorf("indexer call source = %q, want api", call.source)
		}
		if call.bodyText == "" {
			t.Errorf("indexer call bodyText is empty, want non-empty")
		}
	}
}

// boox-notes-pipeline.AC4.4: TestProcessor_ReprocessOnReupload verifies re-uploading triggers re-processing
func TestProcessor_ReprocessOnReupload(t *testing.T) {
	tmpDir := t.TempDir()

	noteID := "test-note-reprocess"
	opts := testutil.TestNoteOpts{
		NoteID: noteID,
		Title:  "Reprocess Test",
		Pages: []*testutil.TestPage{
			{
				PageID: "page-1",
				Width:  1404,
				Height: 1872,
				Shapes: []*pb.ShapeInfoProto{
					{
						UniqueId:  "shape-1",
						ShapeType: 0,
						Thickness: 1.0,
					},
				},
				Points: map[string][]booxnote.TinyPoint{
					"shape-1": {
						{X: 100.0, Y: 100.0, Size: 1, Pressure: 100, Time: 0},
					},
				},
			},
		},
	}
	notePath := testutil.BuildTestNoteFile(t, tmpDir, opts)

	indexer := &mockIndexer{}
	deleter := &mockContentDeleter{}

	// Create mock OCR
	ocrServer := mockOCRServer(t, "OCR text")
	defer ocrServer.Close()
	ocrClient := processor.NewOCRClient(ocrServer.URL, "test-key", "test-model", "anthropic")

	proc, db := openTestProcessor(t, indexer, deleter, ocrClient)
	defer db.Close()
	defer proc.Stop()
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// First enqueue
	if err := proc.Enqueue(context.Background(), notePath); err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}

	// Wait for first processing (OCR may take a few seconds)
	if !waitForJobStatus(t, db, notePath, "done", 10*time.Second) {
		t.Fatalf("first job did not complete in time")
	}

	// Second enqueue (re-upload) - clears old cache and re-indexes
	if err := proc.Enqueue(context.Background(), notePath); err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}

	// Wait for second processing (OCR may take a few seconds)
	if !waitForJobStatus(t, db, notePath, "done", 10*time.Second) {
		t.Fatalf("second job did not complete in time")
	}

	// Verify cache was cleared and re-created
	cacheDir := filepath.Join(proc.cfg.CachePath, noteID)
	pageJPEG := filepath.Join(cacheDir, "page_0.jpg")
	if _, err := os.Stat(pageJPEG); err != nil {
		t.Errorf("cached JPEG not found after re-process: %v", err)
	}

	// Verify ContentDeleter was called to clear old content
	if len(deleter.deletedPaths) == 0 {
		t.Errorf("content deleter not called, want >= 1 call")
	}

	// Verify new indexer calls were made
	if len(indexer.calls) == 0 {
		t.Errorf("no new indexer calls after re-upload, want > 0")
	}
}

// boox-notes-pipeline.AC4.5: TestProcessor_OCRFailure verifies failed OCR marks job as failed
func TestProcessor_OCRFailure(t *testing.T) {
	tmpDir := t.TempDir()

	noteID := "test-note-ocr-fail"
	opts := testutil.TestNoteOpts{
		NoteID: noteID,
		Title:  "OCR Fail Test",
		Pages: []*testutil.TestPage{
			{
				PageID: "page-1",
				Width:  1404,
				Height: 1872,
				Shapes: []*pb.ShapeInfoProto{
					{
						UniqueId:  "shape-1",
						ShapeType: 0,
						Thickness: 1.0,
					},
				},
				Points: map[string][]booxnote.TinyPoint{
					"shape-1": {
						{X: 100.0, Y: 100.0, Size: 1, Pressure: 100, Time: 0},
					},
				},
			},
		},
	}
	notePath := testutil.BuildTestNoteFile(t, tmpDir, opts)

	indexer := &mockIndexer{}
	deleter := &mockContentDeleter{}

	// An OCR failure is recorded with a message either way, but the terminal
	// state now depends on whether the failure was a verdict. A 5xx means the
	// backend never rendered one (this is job 2412's "model failed to load"),
	// so the job is requeued with backoff rather than stranded in `failed`; a
	// 4xx is a verdict and stays terminal.
	run := func(t *testing.T, status int, body string) (string, sql.NullInt64, string) {
		t.Helper()
		ocrServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(body))
		}))
		defer ocrServer.Close()

		ocrClient := processor.NewOCRClient(ocrServer.URL, "test-key", "test-model", "anthropic")
		proc, db := openTestProcessor(t, indexer, deleter, ocrClient)
		defer db.Close()
		defer proc.Stop()
		if err := proc.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := proc.Enqueue(context.Background(), notePath); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}

		// Poll for the worker to have processed and settled the job.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var st, lastErr string
			var requeueAfter sql.NullInt64
			err := db.QueryRowContext(context.Background(),
				`SELECT status, requeue_after, last_error FROM boox_jobs WHERE note_path = ?`,
				notePath).Scan(&st, &requeueAfter, &lastErr)
			if err == nil && lastErr != "" {
				return st, requeueAfter, lastErr
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("job never recorded a failure")
		return "", sql.NullInt64{}, ""
	}

	t.Run("5xx requeues with backoff", func(t *testing.T) {
		st, requeueAfter, lastErr := run(t, http.StatusInternalServerError, `{"error":"internal server error"}`)
		if st != "pending" {
			t.Errorf("status = %q, want pending — a 5xx must not strand the note in failed", st)
		}
		if !requeueAfter.Valid || requeueAfter.Int64 <= time.Now().Unix() {
			t.Errorf("requeue_after = %v, want a future timestamp", requeueAfter)
		}
		if lastErr == "" {
			t.Error("last_error is empty, want the reason visible while the job waits")
		}
	})

	t.Run("4xx fails terminally", func(t *testing.T) {
		st, _, lastErr := run(t, http.StatusBadRequest, `{"error":"unsupported image"}`)
		if st != "failed" {
			t.Errorf("status = %q, want failed — a 4xx is a verdict and will not change on retry", st)
		}
		if lastErr == "" {
			t.Error("last_error is empty, want error message")
		}
	})
}

// boox-notes-pipeline.AC4.6: TestProcessor_CorruptNote verifies corrupt files are handled gracefully
func TestProcessor_CorruptNote(t *testing.T) {
	tmpDir := t.TempDir()

	indexer := &mockIndexer{}
	deleter := &mockContentDeleter{}

	proc, db := openTestProcessor(t, indexer, deleter, nil)
	defer db.Close()
	defer proc.Stop()
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Create a corrupt .note file (not a valid ZIP)
	notePath := filepath.Join(tmpDir, "corrupt.note")
	if err := os.WriteFile(notePath, []byte("this is not a valid zip"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := proc.Enqueue(context.Background(), notePath); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Wait for job to be marked as failed
	if !waitForJobStatus(t, db, notePath, "failed", 10*time.Second) {
		t.Fatalf("job did not fail as expected for corrupt file")
	}

	// Verify job has an error message
	var lastError string
	err := db.QueryRowContext(context.Background(),
		"SELECT last_error FROM boox_jobs WHERE note_path = ?", notePath).Scan(&lastError)
	if err != nil {
		t.Fatalf("query last_error: %v", err)
	}
	if lastError == "" {
		t.Errorf("last_error is empty, want error message for corrupt file")
	}
}

// boox-notes-pipeline.AC4.7: TestProcessor_ManyPages verifies processing of notes with many pages
func TestProcessor_ManyPages(t *testing.T) {
	tmpDir := t.TempDir()

	noteID := "test-note-many-pages"

	// Create 12 pages
	pages := make([]*testutil.TestPage, 12)
	for i := 0; i < 12; i++ {
		pageID := fmt.Sprintf("page-%d", i)
		pages[i] = &testutil.TestPage{
			PageID: pageID,
			Width:  1404,
			Height: 1872,
			Shapes: []*pb.ShapeInfoProto{
				{
					UniqueId:  fmt.Sprintf("shape-%d", i),
					ShapeType: 0,
					Thickness: 1.0,
				},
			},
			Points: map[string][]booxnote.TinyPoint{
				fmt.Sprintf("shape-%d", i): {
					{X: 100.0, Y: 100.0, Size: 1, Pressure: 100, Time: 0},
				},
			},
		}
	}

	opts := testutil.TestNoteOpts{
		NoteID: noteID,
		Title:  "Many Pages Test",
		Pages:  pages,
	}
	notePath := testutil.BuildTestNoteFile(t, tmpDir, opts)

	indexer := &mockIndexer{}
	deleter := &mockContentDeleter{}

	// Create mock OCR
	ocrServer := mockOCRServer(t, "OCR text")
	defer ocrServer.Close()
	ocrClient := processor.NewOCRClient(ocrServer.URL, "test-key", "test-model", "anthropic")

	proc, db := openTestProcessor(t, indexer, deleter, ocrClient)
	defer db.Close()
	defer proc.Stop()
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := proc.Enqueue(context.Background(), notePath); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Wait for job to complete (12 pages may take longer, 15s per page)
	if !waitForJobStatus(t, db, notePath, "done", 20*time.Second) {
		t.Fatalf("job with 12 pages did not complete in time")
	}

	// Verify all 12 pages were rendered and indexed
	if len(indexer.calls) < 12 {
		t.Errorf("indexer calls = %d, want 12", len(indexer.calls))
	}

	// Verify all cached JPEGs exist
	cacheDir := filepath.Join(proc.cfg.CachePath, noteID)
	for i := 0; i < 12; i++ {
		pageJPEG := filepath.Join(cacheDir, fmt.Sprintf("page_%d.jpg", i))
		if _, err := os.Stat(pageJPEG); err != nil {
			t.Errorf("cached JPEG for page %d not found: %v", i, err)
		}
	}
}

// TestProcessor_Enqueue verifies enqueue creates both note and job rows
func TestProcessor_Enqueue(t *testing.T) {
	indexer := &mockIndexer{}
	deleter := &mockContentDeleter{}

	proc, _ := openTestProcessor(t, indexer, deleter, nil)

	notePath := "/tmp/test-enqueue.note"

	if err := proc.Enqueue(context.Background(), notePath); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Verify boox_notes row
	note, err := proc.store.GetNote(context.Background(), notePath)
	if err != nil {
		t.Fatalf("GetNote: %v", err)
	}
	if note == nil {
		t.Fatal("expected note row to exist")
	}

	// Verify boox_jobs row
	var jobStatus string
	err = proc.store.db.QueryRowContext(context.Background(),
		"SELECT status FROM boox_jobs WHERE note_path = ?", notePath).Scan(&jobStatus)
	if err != nil {
		t.Fatalf("query job: %v", err)
	}
	if jobStatus != "pending" {
		t.Errorf("job status = %q, want pending", jobStatus)
	}
}

// TestProcessor_StartStop verifies processor lifecycle
func TestProcessor_StartStop(t *testing.T) {
	indexer := &mockIndexer{}
	deleter := &mockContentDeleter{}

	proc, _ := openTestProcessor(t, indexer, deleter, nil)

	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Give it time to start
	time.Sleep(100 * time.Millisecond)

	// Should not panic on Stop
	proc.Stop()

	// Stop again should not panic (idempotent)
	proc.Stop()
}

// mockEmbedder tracks embedding calls and can be configured to fail.
type mockEmbedder struct {
	calls []string // track what text was embedded
	err   error    // if set, Embed returns this error
}

func (m *mockEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if m.err != nil {
		return nil, m.err
	}
	m.calls = append(m.calls, text)
	return make([]float32, 768), nil // return zero vector
}

// rag-retrieval-pipeline.AC1.2: TestEmbed_NoteFileWithEmbedder verifies embeddings are created for .note files.
func TestEmbed_NoteFileWithEmbedder(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a synthetic .note file with one page
	noteID := "test-embed-note"
	opts := testutil.TestNoteOpts{
		NoteID: noteID,
		Title:  "Embed Test",
		Pages: []*testutil.TestPage{
			{
				PageID: "page-1",
				Width:  1404,
				Height: 1872,
				Shapes: []*pb.ShapeInfoProto{
					{
						UniqueId:  "shape-1",
						ShapeType: 0,
						Color:     -16777216,
						Thickness: 1.0,
						Zorder:    0,
					},
				},
				Points: map[string][]booxnote.TinyPoint{
					"shape-1": {
						{X: 100.0, Y: 100.0, Size: 1, Pressure: 100, Time: 0},
						{X: 101.0, Y: 101.0, Size: 1, Pressure: 100, Time: 1},
					},
				},
			},
		},
	}
	notePath := testutil.BuildTestNoteFile(t, tmpDir, opts)

	// Create mock indexer and embedder
	indexer := &mockIndexer{}
	embedder := &mockEmbedder{}
	embedStore := &testEmbedStore{
		embeddings: make(map[string]map[int][]float32),
	}

	// Create mock OCR server
	ocrServer := mockOCRServer(t, "Test OCR content for embedding")
	defer ocrServer.Close()

	// Create OCR client
	ocrClient := processor.NewOCRClient(ocrServer.URL, "test-key", "test-model", "anthropic")

	// Open processor with embedder
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	defer db.Close()

	cachePath := filepath.Join(tmpDir, ".cache")
	cfg := WorkerConfig{
		Indexer:    indexer,
		OCR:        ocrClient,
		CachePath:  cachePath,
		Embedder:   embedder,
		EmbedModel: "test-model",
		EmbedStore: embedStore,
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	proc := New(db, tmpDir, cfg, logger)
	defer proc.Stop()

	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Enqueue the job
	if err := proc.Enqueue(context.Background(), notePath); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Wait for job to complete
	if !waitForJobStatus(t, db, notePath, "done", 10*time.Second) {
		t.Fatalf("job did not complete in time")
	}

	proc.Stop()

	// Verify that Embed was called
	if len(embedder.calls) == 0 {
		t.Errorf("embedder was not called, want at least 1 embedding call")
	}

	// Verify that the embedding was saved for page 0
	if savedEmbeddings, ok := embedStore.embeddings[notePath]; !ok {
		t.Errorf("no embeddings saved for note path %s", notePath)
	} else {
		if _, ok := savedEmbeddings[0]; !ok {
			t.Errorf("no embedding saved for page 0")
		}
	}
}

// rag-retrieval-pipeline.AC1.7: TestEmbed_FailureDoesNotFailJob verifies that embedding errors don't fail the job.
func TestEmbed_FailureDoesNotFailJob(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a synthetic .note file
	noteID := "test-embed-fail"
	opts := testutil.TestNoteOpts{
		NoteID: noteID,
		Title:  "Embed Failure Test",
		Pages: []*testutil.TestPage{
			{
				PageID: "page-1",
				Width:  1404,
				Height: 1872,
				Shapes: []*pb.ShapeInfoProto{
					{
						UniqueId:  "shape-1",
						ShapeType: 0,
						Color:     -16777216,
						Thickness: 1.0,
						Zorder:    0,
					},
				},
				Points: map[string][]booxnote.TinyPoint{
					"shape-1": {
						{X: 100.0, Y: 100.0, Size: 1, Pressure: 100, Time: 0},
						{X: 101.0, Y: 101.0, Size: 1, Pressure: 100, Time: 1},
					},
				},
			},
		},
	}
	notePath := testutil.BuildTestNoteFile(t, tmpDir, opts)

	// Create embedder that always fails
	failingEmbedder := &mockEmbedder{err: fmt.Errorf("simulated embedding failure")}
	embedStore := &testEmbedStore{
		embeddings: make(map[string]map[int][]float32),
	}

	// Create mock indexer
	indexer := &mockIndexer{}

	// Create mock OCR server
	ocrServer := mockOCRServer(t, "Page content that fails to embed")
	defer ocrServer.Close()

	// Create OCR client
	ocrClient := processor.NewOCRClient(ocrServer.URL, "test-key", "test-model", "anthropic")

	// Open processor with failing embedder
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	defer db.Close()

	cachePath := filepath.Join(tmpDir, ".cache")
	cfg := WorkerConfig{
		Indexer:    indexer,
		OCR:        ocrClient,
		CachePath:  cachePath,
		Embedder:   failingEmbedder,
		EmbedModel: "test-model",
		EmbedStore: embedStore,
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	proc := New(db, tmpDir, cfg, logger)
	defer proc.Stop()

	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Enqueue the job
	if err := proc.Enqueue(context.Background(), notePath); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Wait for job to complete - should succeed despite embedder failure
	if !waitForJobStatus(t, db, notePath, "done", 10*time.Second) {
		t.Fatalf("job did not complete in time")
	}

	proc.Stop()

	// Verify that job completed successfully despite embedding failure
	var jobStatus string
	err = db.QueryRowContext(context.Background(),
		"SELECT status FROM boox_jobs WHERE note_path = ? ORDER BY id DESC LIMIT 1", notePath).Scan(&jobStatus)
	if err != nil {
		t.Fatalf("failed to query job status: %v", err)
	}
	if jobStatus != "done" {
		t.Errorf("job status is %s, want done", jobStatus)
	}

	// Verify that no embeddings were saved (embedder failed)
	if len(embedStore.embeddings) > 0 {
		t.Errorf("embeddings were saved despite failure, want none")
	}
}

// rag-retrieval-pipeline.AC1.2: TestEmbed_PDFFileWithEmbedder verifies embeddings are created for .pdf files.
func TestEmbed_PDFFileWithEmbedder(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a simple single-page PDF
	pdfPath := filepath.Join(tmpDir, "test.pdf")
	if err := createMinimalPDF(pdfPath); err != nil {
		t.Fatalf("createMinimalPDF: %v", err)
	}

	// Create mock indexer and embedder
	indexer := &mockIndexer{}
	embedder := &mockEmbedder{}
	embedStore := &testEmbedStore{
		embeddings: make(map[string]map[int][]float32),
	}

	// Create mock OCR server
	ocrServer := mockOCRServer(t, "PDF content from OCR")
	defer ocrServer.Close()

	// Create OCR client
	ocrClient := processor.NewOCRClient(ocrServer.URL, "test-key", "test-model", "anthropic")

	// Open processor with embedder
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	defer db.Close()

	cachePath := filepath.Join(tmpDir, ".cache")
	cfg := WorkerConfig{
		Indexer:    indexer,
		OCR:        ocrClient,
		CachePath:  cachePath,
		Embedder:   embedder,
		EmbedModel: "test-model",
		EmbedStore: embedStore,
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	proc := New(db, tmpDir, cfg, logger)
	defer proc.Stop()

	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Enqueue the PDF
	if err := proc.Enqueue(context.Background(), pdfPath); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Wait for job to complete
	if !waitForJobStatus(t, db, pdfPath, "done", 10*time.Second) {
		t.Fatalf("job did not complete in time")
	}

	proc.Stop()

	// Verify that Embed was called for the PDF page
	if len(embedder.calls) == 0 {
		t.Errorf("embedder was not called, want at least 1 embedding call")
	}

	// Verify that the embedding was saved
	if savedEmbeddings, ok := embedStore.embeddings[pdfPath]; !ok {
		t.Errorf("no embeddings saved for PDF path %s", pdfPath)
	} else {
		if _, ok := savedEmbeddings[0]; !ok {
			t.Errorf("no embedding saved for PDF page 0")
		}
	}
}

// testEmbedStore is a simple in-memory implementation of rag.Store for testing.
type testEmbedStore struct {
	embeddings map[string]map[int][]float32 // note_path -> page -> embedding
}

func (s *testEmbedStore) Save(ctx context.Context, notePath string, page, chunk int, embedding []float32, model string) error {
	if s.embeddings[notePath] == nil {
		s.embeddings[notePath] = make(map[int][]float32)
	}
	vec := make([]float32, len(embedding))
	copy(vec, embedding)
	s.embeddings[notePath][page] = vec
	return nil
}

func (s *testEmbedStore) DeletePage(ctx context.Context, notePath string, page int) error {
	if s.embeddings[notePath] != nil {
		delete(s.embeddings[notePath], page)
	}
	return nil
}

func (s *testEmbedStore) LoadAll(ctx context.Context) (int, error) {
	return 0, nil
}

func (s *testEmbedStore) AllEmbeddings() []interface{} {
	return nil
}

func (s *testEmbedStore) UnembeddedPages(ctx context.Context) ([]struct {
	NotePath string
	Page     int
	BodyText string
}, error) {
	return nil, nil
}

// createMinimalPDF creates a minimal PDF file for testing.
// This is a very basic PDF structure that can be parsed by pdfrender.
func createMinimalPDF(path string) error {
	// Minimal PDF with one page
	pdfContent := `%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /Resources << >> /MediaBox [0 0 612 792] /Contents 4 0 R >>
endobj
4 0 obj
<< /Length 44 >>
stream
BT
/F1 12 Tf
100 700 Td
(Test PDF) Tj
ET
endstream
endobj
xref
0 5
0000000000 65535 f
0000000009 00000 n
0000000058 00000 n
0000000115 00000 n
0000000206 00000 n
trailer
<< /Size 5 /Root 1 0 R >>
startxref
299
%%EOF
`
	return os.WriteFile(path, []byte(pdfContent), 0644)
}

// TestProcessorStartStopCycle covers the lifecycle the global pipeline status
// bar exposes: its Boox ▶/⏹ controls are now reachable from every page, so a
// stop -> start -> stop round trip has to be safe. It used to panic — `done`
// was minted once in New and closed by run's defer, so the second Stop waited
// on (and the second run closed) an already-closed channel.
func TestProcessorStartStopCycle(t *testing.T) {
	proc, db := openTestProcessor(t, &mockIndexer{}, &mockContentDeleter{}, nil)
	defer db.Close()

	if proc.Running() {
		t.Fatal("Running() = true before Start")
	}

	for i := 0; i < 3; i++ {
		if err := proc.Start(context.Background()); err != nil {
			t.Fatalf("Start #%d: %v", i, err)
		}
		if !proc.Running() {
			t.Fatalf("Running() = false after Start #%d", i)
		}
		proc.Stop()
		if proc.Running() {
			t.Fatalf("Running() = true after Stop #%d", i)
		}
	}

	// Both directions are idempotent: a double-press must be a no-op, not a
	// second worker or a wait on a closed channel.
	proc.Stop()
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("Start after redundant Stop: %v", err)
	}
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("redundant Start: %v", err)
	}
	proc.Stop()
}

func TestRetryDelay_BackoffAndSaturation(t *testing.T) {
	for _, tc := range []struct {
		attempts int
		want     time.Duration
	}{
		{0, retryBackoffBase}, // defensive: treated as the first attempt
		{1, 1 * time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{4, 8 * time.Minute},
		{6, retryBackoffMax},  // 32m would exceed the cap
		{99, retryBackoffMax}, // must saturate, not shift into overflow
	} {
		if got := retryDelay(tc.attempts); got != tc.want {
			t.Errorf("retryDelay(%d) = %v, want %v", tc.attempts, got, tc.want)
		}
	}
}

// TestProcessJob_TransientFailureRequeues covers the gap that left Boox jobs
// 2391 and 2412 stuck in `failed` since June and July: FailJob was terminal,
// so a momentary OCR-backend outage stranded a note until a human pressed
// Retry Failed. A transient failure must go back to pending with a delay, and
// only become terminal once the attempt cap is reached.
func TestProcessJob_TransientFailureRequeues(t *testing.T) {
	proc, db := openTestProcessor(t, &mockIndexer{}, &mockContentDeleter{}, nil)
	defer db.Close()
	ctx := context.Background()

	notePath := filepath.Join(proc.notesPath, "flaky.note")
	if err := os.WriteFile(notePath, []byte("not a real zip"), 0o644); err != nil {
		t.Fatalf("write note: %v", err)
	}
	if err := proc.store.EnqueueJob(ctx, notePath); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	statusOf := func(t *testing.T, id int64) (string, int, sql.NullInt64) {
		t.Helper()
		var status string
		var attempts int
		var requeueAfter sql.NullInt64
		if err := db.QueryRowContext(ctx,
			`SELECT status, attempts, requeue_after FROM boox_jobs WHERE id = ?`, id).
			Scan(&status, &attempts, &requeueAfter); err != nil {
			t.Fatalf("read job: %v", err)
		}
		return status, attempts, requeueAfter
	}

	// Drive processJob directly with a transient failure, once per attempt.
	// Each iteration claims (attempts++) then fails transiently.
	var jobID int64
	for attempt := 1; attempt < maxJobAttempts; attempt++ {
		job, err := proc.store.ClaimNextJob(ctx)
		if err != nil {
			t.Fatalf("claim %d: %v", attempt, err)
		}
		if job == nil {
			t.Fatalf("attempt %d: no claimable job — a requeue delay leaked into the test", attempt)
		}
		jobID = job.ID

		before := time.Now()
		proc.handleJobError(ctx, job, processor.Transient(errors.New("connection refused")))

		status, attempts, requeueAfter := statusOf(t, jobID)
		if status != "pending" {
			t.Fatalf("attempt %d: status = %q, want pending (transient failures must requeue)", attempt, status)
		}
		if attempts != attempt {
			t.Fatalf("attempt %d: attempts = %d", attempt, attempts)
		}
		if !requeueAfter.Valid || requeueAfter.Int64 < before.Unix() {
			t.Fatalf("attempt %d: requeue_after = %v, want a future timestamp", attempt, requeueAfter)
		}
		// The delay is real: the job must not be immediately re-claimable.
		if j, err := proc.store.ClaimNextJob(ctx); err != nil || j != nil {
			t.Fatalf("attempt %d: job claimable despite requeue_after (j=%v err=%v)", attempt, j, err)
		}
		// Clear the delay so the next loop iteration can claim it.
		if _, err := db.ExecContext(ctx,
			`UPDATE boox_jobs SET requeue_after = NULL WHERE id = ?`, jobID); err != nil {
			t.Fatalf("clear delay: %v", err)
		}
	}

	// The claim that reaches the cap fails terminally instead of looping.
	job, err := proc.store.ClaimNextJob(ctx)
	if err != nil || job == nil {
		t.Fatalf("final claim: job=%v err=%v", job, err)
	}
	proc.handleJobError(ctx, job, processor.Transient(errors.New("connection refused")))
	if status, attempts, _ := statusOf(t, jobID); status != "failed" || attempts != maxJobAttempts {
		t.Fatalf("at cap: status=%q attempts=%d, want failed/%d", status, attempts, maxJobAttempts)
	}
}

// TestProcessJob_PermanentFailureDoesNotRetry pins the other half: a parse
// error (the empty-notebook case behind jobs 1504 and 1571) is a real verdict
// and must fail on the first attempt rather than churning through the backoff.
func TestProcessJob_PermanentFailureDoesNotRetry(t *testing.T) {
	proc, db := openTestProcessor(t, &mockIndexer{}, &mockContentDeleter{}, nil)
	defer db.Close()
	ctx := context.Background()

	notePath := filepath.Join(proc.notesPath, "empty.note")
	if err := os.WriteFile(notePath, []byte("not a real zip"), 0o644); err != nil {
		t.Fatalf("write note: %v", err)
	}
	if err := proc.store.EnqueueJob(ctx, notePath); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	job, err := proc.store.ClaimNextJob(ctx)
	if err != nil || job == nil {
		t.Fatalf("claim: job=%v err=%v", job, err)
	}

	proc.handleJobError(ctx, job, errors.New("parse note: booxnote: read virtual page: entry not found"))

	var status string
	if err := db.QueryRowContext(ctx,
		`SELECT status FROM boox_jobs WHERE id = ?`, job.ID).Scan(&status); err != nil {
		t.Fatalf("read job: %v", err)
	}
	if status != "failed" {
		t.Errorf("status = %q, want failed on the first attempt", status)
	}
}
