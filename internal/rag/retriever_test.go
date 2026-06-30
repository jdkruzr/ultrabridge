package rag

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/search"
)

// TestRetrieverHybridFusion verifies AC2.1: Results combine FTS5 and vector similarity via RRF
func TestRetrieverHybridFusion(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()

	// Populate note_content with distinct pages
	insertTestNote(t, db, "note1.note", 0, "machine learning algorithms", "Deep dive into ML algorithms and neural networks")
	insertTestNote(t, db, "note1.note", 1, "neural networks", "Understanding neural network architectures")
	insertTestNote(t, db, "note2.note", 0, "python programming", "Python programming best practices")
	insertTestNote(t, db, "boox1.note", 0, "deep learning", "Deep learning and neural networks in practice")

	// Create embeddings for some pages
	mockEmbedder := &mockRetrieverEmbedder{
		vectors: map[string][]float32{
			"machine learning algorithms":                      {0.8, 0.2, 0.1},
			"neural networks":                                  {0.75, 0.25, 0.05},
			"Deep dive into ML algorithms and neural networks": {0.7, 0.3, 0.0},
			"Understanding neural network architectures":       {0.72, 0.28, 0.0},
			"deep learning":                                    {0.85, 0.15, 0.0},
		},
	}
	embedStore := NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// Save embeddings for pages that should match via vector search
	embedStore.Save(ctx, "note1.note", 0, 0, mockEmbedder.vectors["machine learning algorithms"], "test-model")
	embedStore.Save(ctx, "note1.note", 1, 0, mockEmbedder.vectors["neural networks"], "test-model")
	embedStore.Save(ctx, "boox1.note", 0, 0, mockEmbedder.vectors["deep learning"], "test-model")

	searchIndex := search.New(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	retriever := NewRetriever(db, searchIndex, embedStore, mockEmbedder, logger)

	// Search with query that matches via FTS5 (keyword) and vector (semantic)
	results, err := retriever.Search(ctx, SearchRequest{
		Query: "machine learning",
		Limit: 20,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	// Should return results from both FTS5 and vector search
	if len(results) == 0 {
		t.Fatalf("Expected results, got none")
	}

	// Verify results include both FTS5 matches and vector matches
	hasFTSMatch := false
	hasVectorMatch := false
	for _, r := range results {
		// FTS5 matches should include pages with "machine" or "learning" keywords
		if r.NotePath == "note1.note" && r.Page == 0 {
			hasFTSMatch = true
		}
		// Vector match should include "deep learning" page (high cosine sim)
		if r.NotePath == "boox1.note" && r.Page == 0 {
			hasVectorMatch = true
		}
	}

	if !hasFTSMatch {
		t.Errorf("Expected FTS match for 'machine learning' query")
	}
	if !hasVectorMatch {
		t.Errorf("expected vector match for 'deep learning' page in hybrid results")
	}

	// Verify results are sorted by RRF score descending
	if len(results) > 1 {
		if results[0].Score < results[1].Score {
			t.Errorf("Results should be sorted by score descending: got %f, %f", results[0].Score, results[1].Score)
		}
	}
}

// TestRetrieverFolderFilter verifies AC2.2: Folder filter works for both FTS5 and vector results
func TestRetrieverFolderFilter(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()

	// Insert pages with metadata rows so enrichResult can populate Folder field
	insertBooxNote(t, db, "/notes/Work/proj1.note", "Boox1", "Work", "Project management details")
	insertBooxNote(t, db, "/notes/Personal/diary.note", "Boox2", "Personal", "Personal diary entry")
	insertBooxNote(t, db, "/notes/Work/proj2.note", "Boox3", "Work", "Another project details")

	searchIndex := search.New(db)
	embedStore := NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Create a mock embedder for vector search
	mockEmbedder := &mockRetrieverEmbedder{
		vectors: map[string][]float32{
			"project details":            {0.9, 0.1, 0.0},
			"Project management details": {0.85, 0.15, 0.0},
			"Another project details":    {0.85, 0.15, 0.0},
			"personal thoughts":          {0.2, 0.8, 0.0},
			"Personal diary entry":       {0.1, 0.9, 0.0},
		},
	}

	// Save embeddings for all pages to enable vector search
	embedStore.Save(ctx, "/notes/Work/proj1.note", 0, 0, mockEmbedder.vectors["project details"], "test-model")
	embedStore.Save(ctx, "/notes/Personal/diary.note", 0, 0, mockEmbedder.vectors["personal thoughts"], "test-model")
	embedStore.Save(ctx, "/notes/Work/proj2.note", 0, 0, mockEmbedder.vectors["project details"], "test-model")

	retriever := NewRetriever(db, searchIndex, embedStore, mockEmbedder, logger)

	// Search with folder filter
	results, err := retriever.Search(ctx, SearchRequest{
		Query:  "project details",
		Folder: "Work",
		Limit:  20,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	// Should only return Work folder pages
	if len(results) == 0 {
		t.Fatalf("Expected results from Work folder")
	}

	for _, r := range results {
		if r.Folder != "Work" {
			t.Errorf("Result folder %s should be Work", r.Folder)
		}
	}
}

// TestRetrieverDeviceFilter verifies AC2.2: Device filter works
func TestRetrieverDeviceFilter(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()

	// Insert boox note with device model
	insertBooxNote(t, db, "/notes/boox1.note", "Palma2", "Work", "boox content here")
	insertSupernoteNote(t, db, "/notes/sn1.note", "MyNotes/Work/sn.note", "supernote content here")

	searchIndex := search.New(db)
	embedStore := NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	retriever := NewRetriever(db, searchIndex, embedStore, nil, logger)

	// Search with device filter for Boox
	results, err := retriever.Search(ctx, SearchRequest{
		Query:  "content",
		Device: "Palma2",
		Limit:  20,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	// Should only return Palma2 (Boox) results
	for _, r := range results {
		if r.Device != "Palma2" {
			t.Errorf("Expected device Palma2, got %s", r.Device)
		}
	}

	// Search with device filter for Supernote
	results, err = retriever.Search(ctx, SearchRequest{
		Query:  "content",
		Device: "Supernote",
		Limit:  20,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	// Should only return Supernote results
	for _, r := range results {
		if r.Device != "Supernote" {
			t.Errorf("Expected device Supernote, got %s", r.Device)
		}
	}
}

// TestRetrieverDateRangeFilter verifies AC2.2: Date range filter works
func TestRetrieverDateRangeFilter(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()

	now := time.Now()
	oldTime := now.AddDate(-1, 0, 0)
	futureTime := now.AddDate(1, 0, 0)

	// Insert boox notes with different dates
	insertBooxNoteWithTime(t, db, "/notes/old.note", "Palma2", "Archive", "old content", oldTime)
	insertBooxNoteWithTime(t, db, "/notes/recent.note", "Palma2", "Recent", "recent content", now)
	insertBooxNoteWithTime(t, db, "/notes/future.note", "Palma2", "Future", "future content", futureTime)

	searchIndex := search.New(db)
	embedStore := NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	retriever := NewRetriever(db, searchIndex, embedStore, nil, logger)

	// Search with date range (last 6 months)
	sixMonthsAgo := now.AddDate(0, -6, 0)
	results, err := retriever.Search(ctx, SearchRequest{
		Query:    "content",
		DateFrom: sixMonthsAgo,
		Limit:    20,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	// Should only return notes within date range
	for _, r := range results {
		if r.NoteDate.Before(sixMonthsAgo) {
			t.Errorf("Result date %v should not be before %v", r.NoteDate, sixMonthsAgo)
		}
	}

	// Search with upper date bound
	results, err = retriever.Search(ctx, SearchRequest{
		Query:  "content",
		DateTo: oldTime.AddDate(0, 1, 0),
		Limit:  20,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	for _, r := range results {
		if r.NoteDate.After(oldTime.AddDate(0, 1, 0)) {
			t.Errorf("Result date %v should not be after upper bound", r.NoteDate)
		}
	}
}

// TestRetrieverMetadataJOINs verifies AC2.3: Metadata JOINs populate Device and Folder
func TestRetrieverMetadataJOINs(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()

	// Insert boox note with full metadata
	insertBooxNote(t, db, "/notes/boox1.note", "Palma2", "MyWork", "boox content")

	// Insert supernote note with full metadata
	insertSupernoteNote(t, db, "/notes/sn1.note", "MyNotes/Personal/test.note", "supernote content")

	searchIndex := search.New(db)
	embedStore := NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	retriever := NewRetriever(db, searchIndex, embedStore, nil, logger)

	// Search
	results, err := retriever.Search(ctx, SearchRequest{
		Query: "content",
		Limit: 20,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	// Find boox result and verify metadata JOINs
	for _, r := range results {
		if r.NotePath == "/notes/boox1.note" {
			if r.Device != "Palma2" {
				t.Errorf("Expected device Palma2 from boox_notes JOIN, got %s", r.Device)
			}
			if r.Folder != "MyWork" {
				t.Errorf("Expected folder MyWork from boox_notes JOIN, got %s", r.Folder)
			}
		}
		if r.NotePath == "/notes/sn1.note" {
			if r.Device != "Supernote" {
				t.Errorf("Expected device Supernote from notes JOIN, got %s", r.Device)
			}
			if r.Folder != "MyNotes/Personal" {
				t.Errorf("Expected full folder path MyNotes/Personal extracted from rel_path, got %s", r.Folder)
			}
		}
	}
}

// TestRetrieverSearchResultFields verifies AC2.4: SearchResult has all required fields
func TestRetrieverSearchResultFields(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()

	insertBooxNote(t, db, "/notes/test.note", "TestDevice", "TestFolder", "Test body text here")

	searchIndex := search.New(db)
	embedStore := NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	retriever := NewRetriever(db, searchIndex, embedStore, nil, logger)

	results, err := retriever.Search(ctx, SearchRequest{
		Query: "Test",
		Limit: 20,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	if len(results) == 0 {
		t.Fatalf("Expected results")
	}

	r := results[0]

	// Verify all required fields are present (non-zero values)
	if r.NotePath == "" {
		t.Error("NotePath should not be empty")
	}
	if r.Page < 0 {
		t.Error("Page should be >= 0")
	}
	if r.BodyText == "" {
		t.Error("BodyText should be populated")
	}
	if r.Score == 0 {
		t.Error("Score should be set")
	}
	if r.Folder == "" {
		t.Error("Folder should be populated from metadata JOIN")
	}
	if r.Device == "" {
		t.Error("Device should be populated from metadata JOIN")
	}
	// NoteDate is allowed to be zero if not set, so just check it can be marshaled
	_ = r.NoteDate
}

// TestRetrieverFTS5Fallback verifies AC2.5: FTS5-only fallback when no embeddings
func TestRetrieverFTS5Fallback(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()

	insertTestNote(t, db, "note1.note", 0, "test content", "This is test content for FTS5 search")

	searchIndex := search.New(db)
	embedStore := NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Create retriever with empty embedding store (no embeddings loaded)
	retriever := NewRetriever(db, searchIndex, embedStore, &mockRetrieverEmbedder{vectors: map[string][]float32{}}, logger)

	// Search should still return FTS5 results
	results, err := retriever.Search(ctx, SearchRequest{
		Query: "test content",
		Limit: 20,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	if len(results) == 0 {
		t.Fatalf("Expected FTS5 fallback results")
	}

	// Results should be ordered by BM25 score (FTS5 ranking)
	if len(results) > 1 {
		// BM25 scores should be descending (lower is better in the context)
		for i := 1; i < len(results); i++ {
			if results[i-1].Score < results[i].Score {
				t.Logf("Warning: Expected descending BM25 scores, got %f then %f", results[i-1].Score, results[i].Score)
			}
		}
	}
}

// TestRetrieverNoEmbedder verifies FTS5-only fallback when embedder is nil
func TestRetrieverNoEmbedder(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()

	insertTestNote(t, db, "note1.note", 0, "test content", "This is test content for FTS5 search")

	searchIndex := search.New(db)
	embedStore := NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Create retriever with nil embedder
	retriever := NewRetriever(db, searchIndex, embedStore, nil, logger)

	results, err := retriever.Search(ctx, SearchRequest{
		Query: "test content",
		Limit: 20,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	if len(results) == 0 {
		t.Fatalf("Expected FTS5-only results with nil embedder")
	}
}

// TestRetrieverSourceFilter verifies SourceType classification (by path
// namespace + table joins) and the Sources facet filter (post-merge).
func TestRetrieverSourceFilter(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()

	const kw = "alpha"
	// One indexed doc per source, all matching the same keyword.
	insertTestNote(t, db, "digest://uid-1", 0, "Digest One", "alpha digest excerpt")
	insertTestNote(t, db, "forestnote://nb/pg-1", 0, "", "alpha forestnote page")
	insertTestNote(t, db, "remarkable://doc-1", 0, "Project Plan", "alpha remarkable page")
	insertBooxNote(t, db, "/notes/boox.note", "Palma2", "Work", "alpha boox content")
	insertSupernoteNote(t, db, "/notes/sn.note", "Note/Work/sn.note", "alpha supernote content")

	searchIndex := search.New(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	retriever := NewRetriever(db, searchIndex, NewStore(db, logger), nil, logger)

	// No filter → all four, each correctly classified.
	all, err := retriever.Search(ctx, SearchRequest{Query: kw, Limit: 20})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	got := map[string]string{} // path -> sourceType
	for _, r := range all {
		got[r.NotePath] = r.SourceType
	}
	want := map[string]string{
		"digest://uid-1":       SourceDigest,
		"forestnote://nb/pg-1": SourceForestNote,
		"remarkable://doc-1":   SourceRemarkable,
		"/notes/boox.note":     SourceBoox,
		"/notes/sn.note":       SourceSupernote,
	}
	for path, st := range want {
		if got[path] != st {
			t.Errorf("path %s classified as %q, want %q", path, got[path], st)
		}
	}

	// Filter to digests only.
	digestsOnly, err := retriever.Search(ctx, SearchRequest{Query: kw, Sources: []string{SourceDigest}, Limit: 20})
	if err != nil {
		t.Fatalf("search digests: %v", err)
	}
	if len(digestsOnly) != 1 || digestsOnly[0].NotePath != "digest://uid-1" {
		t.Errorf("digest filter returned %d results: %+v", len(digestsOnly), digestsOnly)
	}

	// Filter to two sources.
	two, err := retriever.Search(ctx, SearchRequest{Query: kw, Sources: []string{SourceBoox, SourceForestNote}, Limit: 20})
	if err != nil {
		t.Fatalf("search two: %v", err)
	}
	for _, r := range two {
		if r.SourceType != SourceBoox && r.SourceType != SourceForestNote {
			t.Errorf("unexpected source %q in two-source filter", r.SourceType)
		}
	}
	if len(two) != 2 {
		t.Errorf("two-source filter returned %d results, want 2", len(two))
	}
}

func TestRetrieverLocationFilterUsesExactFullPath(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()

	insertBooxNote(t, db, "/boox/work.note", "Palma2", "Work", "alpha boox work")
	insertBooxNote(t, db, "/boox/personal-work.note", "Palma2", "Personal/Work", "alpha boox nested")
	insertSupernoteNote(t, db, "/sn/work.note", "Work/work.note", "alpha supernote work")
	insertSupernoteNote(t, db, "/sn/personal-work.note", "Personal/Work/work.note", "alpha supernote nested")

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	retriever := NewRetriever(db, search.New(db), NewStore(db, logger), nil, logger)
	results, err := retriever.Search(ctx, SearchRequest{
		Query:     "alpha",
		Locations: []LocationFilter{{FullPath: "Work"}},
		Limit:     20,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := map[string]bool{}
	for _, r := range results {
		got[r.NotePath] = true
		if r.Folder != "Work" {
			t.Fatalf("result %s folder = %q, want exact full path Work", r.NotePath, r.Folder)
		}
	}
	for _, want := range []string{"/boox/work.note", "/sn/work.note"} {
		if !got[want] {
			t.Fatalf("missing %s from exact Work results: %+v", want, results)
		}
	}
	for _, notWant := range []string{"/boox/personal-work.note", "/sn/personal-work.note"} {
		if got[notWant] {
			t.Fatalf("nested folder %s should not match exact Work", notWant)
		}
	}
}

func TestRetrieverDateSortNewestAndEarliest(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()

	oldTime := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	newTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	insertBooxNoteWithTime(t, db, "/boox/old.note", "Palma2", "Work", "alpha old", oldTime)
	insertBooxNoteWithTime(t, db, "/boox/new.note", "Palma2", "Work", "alpha new", newTime)

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	retriever := NewRetriever(db, search.New(db), NewStore(db, logger), nil, logger)
	newest, err := retriever.Search(ctx, SearchRequest{Query: "alpha", Sort: "date_desc", Limit: 20})
	if err != nil {
		t.Fatalf("newest Search: %v", err)
	}
	if len(newest) < 2 || newest[0].NotePath != "/boox/new.note" {
		t.Fatalf("date_desc first = %+v, want /boox/new.note", newest)
	}
	earliest, err := retriever.Search(ctx, SearchRequest{Query: "alpha", Sort: "date_asc", Limit: 20})
	if err != nil {
		t.Fatalf("earliest Search: %v", err)
	}
	if len(earliest) < 2 || earliest[0].NotePath != "/boox/old.note" {
		t.Fatalf("date_asc first = %+v, want /boox/old.note", earliest)
	}
}

func TestRetrieverKeywordModeExcludesVectorOnlyCandidates(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()

	insertTestNote(t, db, "/notes/cambridge.note", 0, "Cambridge", "cambridge project notes")
	insertTestNote(t, db, "/notes/unrelated.note", 0, "Unrelated", "web development guide with enough text to survive vector-only hybrid tail filtering")

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	embedStore := NewStore(db, logger)
	mockEmbedder := &mockRetrieverEmbedder{
		vectors: map[string][]float32{
			"cambridge": {1, 0, 0},
		},
	}
	embedStore.Save(ctx, "/notes/unrelated.note", 0, 0, []float32{1, 0, 0}, "test-model")
	retriever := NewRetriever(db, search.New(db), embedStore, mockEmbedder, logger)

	keyword, err := retriever.Search(ctx, SearchRequest{Query: "cambridge", Mode: SearchModeKeyword, Limit: 20})
	if err != nil {
		t.Fatalf("keyword Search: %v", err)
	}
	gotKeyword := map[string]bool{}
	for _, r := range keyword {
		gotKeyword[r.NotePath] = true
	}
	if !gotKeyword["/notes/cambridge.note"] {
		t.Fatalf("keyword results missing lexical match: %+v", keyword)
	}
	if gotKeyword["/notes/unrelated.note"] {
		t.Fatalf("keyword mode included vector-only result: %+v", keyword)
	}

	hybrid, err := retriever.Search(ctx, SearchRequest{Query: "cambridge", Mode: SearchModeHybrid, Limit: 20})
	if err != nil {
		t.Fatalf("hybrid Search: %v", err)
	}
	gotHybrid := map[string]bool{}
	for _, r := range hybrid {
		gotHybrid[r.NotePath] = true
	}
	if !gotHybrid["/notes/unrelated.note"] {
		t.Fatalf("hybrid mode should include vector-only result: %+v", hybrid)
	}
}

func TestRetrieverSourceFilterSearchesPastInitialKeywordPage(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()

	for i := 0; i < 120; i++ {
		insertTestNote(t, db, fmt.Sprintf("/generic/storage-%03d.note", i), 0, "Generic", "storage storage storage storage")
	}
	insertTestNote(t, db, "forestnote://nb/storage-page", 0, "", "Historically there are challenges with storage in HPC but Hammerspace can help")

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	retriever := NewRetriever(db, search.New(db), NewStore(db, logger), nil, logger)

	results, err := retriever.Search(ctx, SearchRequest{
		Query:   "storage",
		Sources: []string{SourceForestNote},
		Mode:    SearchModeKeyword,
		Limit:   20,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].NotePath != "forestnote://nb/storage-page" {
		t.Fatalf("forestnote source-filtered storage results = %+v, want forestnote page beyond initial keyword page", results)
	}
}

func TestRetrieverHybridRanksLexicalPhraseHitsBeforeVectorOnlyHits(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()

	insertTestNote(t, db, "/notes/froster-a.note", 0, "Trip A", "Froster Glacier route planning")
	insertTestNote(t, db, "/notes/froster-b.note", 0, "Trip B", "Ask about S3/Glacier as archive. (Froster)")
	for i := 0; i < 5; i++ {
		insertTestNote(t, db, fmt.Sprintf("/notes/vector-only-%d.note", i), 0, "Unrelated", "Tiny")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	embedStore := NewStore(db, logger)
	mockEmbedder := &mockRetrieverEmbedder{
		vectors: map[string][]float32{
			"Froster Glacier": {1, 0, 0},
		},
	}
	for i := 0; i < 5; i++ {
		embedStore.Save(ctx, fmt.Sprintf("/notes/vector-only-%d.note", i), 0, 0, []float32{1, 0, 0}, "test-model")
	}
	retriever := NewRetriever(db, search.New(db), embedStore, mockEmbedder, logger)

	results, err := retriever.Search(ctx, SearchRequest{
		Query: "Froster Glacier",
		Mode:  SearchModeHybrid,
		Limit: 3,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("results = %+v, want at least two lexical phrase hits", results)
	}
	for i := 0; i < 2; i++ {
		if results[i].NotePath != "/notes/froster-a.note" && results[i].NotePath != "/notes/froster-b.note" {
			t.Fatalf("result %d = %+v, want lexical Froster Glacier hit before vector-only hits; all results: %+v", i, results[i], results)
		}
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want only substantial lexical hits; short vector-only pages should not pad tail", results)
	}
}

func TestRetrieverHybridKeepsSubstantialVectorOnlyCandidates(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()

	insertTestNote(t, db, "/notes/froster.note", 0, "Trip", "Froster Glacier route planning")
	insertTestNote(t, db, "/notes/deep-learning.note", 0, "Deep Learning", "Deep learning and neural networks in practice")

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	embedStore := NewStore(db, logger)
	mockEmbedder := &mockRetrieverEmbedder{
		vectors: map[string][]float32{
			"Froster Glacier": {1, 0, 0},
		},
	}
	embedStore.Save(ctx, "/notes/deep-learning.note", 0, 0, []float32{1, 0, 0}, "test-model")
	retriever := NewRetriever(db, search.New(db), embedStore, mockEmbedder, logger)

	results, err := retriever.Search(ctx, SearchRequest{
		Query: "Froster Glacier",
		Mode:  SearchModeHybrid,
		Limit: 3,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := map[string]bool{}
	for _, r := range results {
		got[r.NotePath] = true
	}
	if !got["/notes/froster.note"] || !got["/notes/deep-learning.note"] {
		t.Fatalf("results = %+v, want lexical hit plus substantial vector-only hit", results)
	}
}

func TestRetrieverKeywordDateSortDoesNotPromoteVectorOnlyCandidates(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()

	oldTime := time.Date(2023, 10, 17, 0, 0, 0, 0, time.UTC)
	newTime := time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC)
	insertBooxNoteWithTime(t, db, "/boox/cambridge.note", "Palma2", "Work", "cambridge meeting", oldTime)
	insertBooxNoteWithTime(t, db, "/boox/unrelated.note", "Palma2", "Work", "web development guide", newTime)

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	embedStore := NewStore(db, logger)
	mockEmbedder := &mockRetrieverEmbedder{
		vectors: map[string][]float32{
			"cambridge": {1, 0, 0},
		},
	}
	embedStore.Save(ctx, "/boox/unrelated.note", 0, 0, []float32{1, 0, 0}, "test-model")
	retriever := NewRetriever(db, search.New(db), embedStore, mockEmbedder, logger)

	results, err := retriever.Search(ctx, SearchRequest{
		Query: "cambridge",
		Mode:  SearchModeKeyword,
		Sort:  "date_desc",
		Limit: 20,
	})
	if err != nil {
		t.Fatalf("keyword date Search: %v", err)
	}
	if len(results) != 1 || results[0].NotePath != "/boox/cambridge.note" {
		t.Fatalf("keyword date sort results = %+v, want only lexical Cambridge hit", results)
	}
}

func TestRetrieverDateSortUnknownDatesLast(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()

	knownTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	insertBooxNoteWithTime(t, db, "/boox/known.note", "Palma2", "Work", "alpha known", knownTime)
	insertTestNote(t, db, "/orphan/unknown.note", 0, "Unknown", "alpha unknown")

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	retriever := NewRetriever(db, search.New(db), NewStore(db, logger), nil, logger)

	for _, sortMode := range []string{"date_desc", "date_asc"} {
		results, err := retriever.Search(ctx, SearchRequest{Query: "alpha", Mode: SearchModeKeyword, Sort: sortMode, Limit: 20})
		if err != nil {
			t.Fatalf("%s Search: %v", sortMode, err)
		}
		if len(results) < 2 || results[len(results)-1].NotePath != "/orphan/unknown.note" {
			t.Fatalf("%s results = %+v, want unknown date last", sortMode, results)
		}
	}
}

func TestRetrieverRemarkableDatesUseMetadataAndTitleDateOnly(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()
	createRemarkableDocumentsTable(t, db)

	insertTestNote(t, db, "remarkable://doc-1", -1, "20231017 Cambridge", "20231017 Cambridge\nFolder: Moffitt / Vendors\nPages: 1")
	_, err := db.ExecContext(ctx,
		`INSERT INTO remarkable_documents(id, modified_client, parent_id, updated_at, deleted)
		 VALUES (?, ?, ?, ?, 0)`,
		"doc-1", "", "", time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC).UnixMilli())
	if err != nil {
		t.Fatalf("insert remarkable doc: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	retriever := NewRetriever(db, search.New(db), NewStore(db, logger), nil, logger)
	results, err := retriever.Search(ctx, SearchRequest{Query: "Cambridge", Mode: SearchModeKeyword, Limit: 20})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v, want one rM hit", results)
	}
	wantCreated := time.Date(2023, 10, 17, 0, 0, 0, 0, time.UTC)
	if !results[0].CreatedAt.Equal(wantCreated) {
		t.Fatalf("CreatedAt = %v, want %v", results[0].CreatedAt, wantCreated)
	}
	if !results[0].ModifiedAt.IsZero() {
		t.Fatalf("ModifiedAt = %v, want zero because updated_at is ingestion time", results[0].ModifiedAt)
	}
}

// mockRetrieverEmbedder for testing — returns deterministic vectors
type mockRetrieverEmbedder struct {
	vectors map[string][]float32
}

func (m *mockRetrieverEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if vec, ok := m.vectors[text]; ok {
		return vec, nil
	}
	// Default vector for unknown text
	return []float32{0.5, 0.5, 0.5}, nil
}

// Helper functions

func createRemarkableDocumentsTable(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `CREATE TABLE remarkable_documents (
		id TEXT PRIMARY KEY,
		version INTEGER NOT NULL DEFAULT 1,
		modified_client TEXT NOT NULL DEFAULT '',
		doc_type TEXT NOT NULL DEFAULT '',
		visible_name TEXT NOT NULL DEFAULT '',
		current_page INTEGER NOT NULL DEFAULT 0,
		bookmarked INTEGER NOT NULL DEFAULT 0,
		parent_id TEXT NOT NULL DEFAULT '',
		payload_path TEXT NOT NULL DEFAULT '',
		deleted INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create remarkable_documents: %v", err)
	}
}

func insertTestNote(t *testing.T, db *sql.DB, path string, page int, titleText, bodyText string) {
	ctx := context.Background()
	_, err := db.ExecContext(ctx,
		`INSERT INTO note_content (note_path, page, title_text, body_text, keywords, source, model, indexed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		path, page, titleText, bodyText, "", "api", "test", time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("Failed to insert note: %v", err)
	}
}

func insertBooxNote(t *testing.T, db *sql.DB, path, device, folder, bodyText string) {
	ctx := context.Background()

	// Insert into note_content
	_, err := db.ExecContext(ctx,
		`INSERT INTO note_content (note_path, page, title_text, body_text, keywords, source, model, indexed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		path, 0, folder, bodyText, "", "api", "test", time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("Failed to insert note_content: %v", err)
	}

	// Insert into boox_notes
	now := time.Now()
	_, err = db.ExecContext(ctx,
		`INSERT INTO boox_notes (path, note_id, title, device_model, note_type, folder, page_count, file_hash, version, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		path, "note-id-1", folder, device, "text", folder, 1, "hash123", 1, now.UnixMilli(), now.UnixMilli(),
	)
	if err != nil {
		t.Fatalf("Failed to insert boox_notes: %v", err)
	}
}

func insertBooxNoteWithTime(t *testing.T, db *sql.DB, path, device, folder, bodyText string, createdTime time.Time) {
	ctx := context.Background()

	// Insert into note_content
	_, err := db.ExecContext(ctx,
		`INSERT INTO note_content (note_path, page, title_text, body_text, keywords, source, model, indexed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		path, 0, folder, bodyText, "", "api", "test", time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("Failed to insert note_content: %v", err)
	}

	// Insert into boox_notes with specific time
	_, err = db.ExecContext(ctx,
		`INSERT INTO boox_notes (path, note_id, title, device_model, note_type, folder, page_count, file_hash, version, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		path, "note-id-1", folder, device, "text", folder, 1, "hash123", 1, createdTime.UnixMilli(), createdTime.UnixMilli(),
	)
	if err != nil {
		t.Fatalf("Failed to insert boox_notes: %v", err)
	}
}

func insertSupernoteNote(t *testing.T, db *sql.DB, path, relPath, bodyText string) {
	ctx := context.Background()

	// Insert into note_content
	_, err := db.ExecContext(ctx,
		`INSERT INTO note_content (note_path, page, title_text, body_text, keywords, source, model, indexed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		path, 0, "Supernote", bodyText, "", "api", "test", time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("Failed to insert note_content: %v", err)
	}

	// Insert into notes
	now := time.Now()
	_, err = db.ExecContext(ctx,
		`INSERT INTO notes (path, rel_path, file_type, size_bytes, mtime, sha256, backup_path, backed_up_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		path, relPath, ".note", 1024, now.Unix(), "sha256hash", "", 0, now.Unix(), now.Unix(),
	)
	if err != nil {
		t.Fatalf("Failed to insert notes: %v", err)
	}
}
