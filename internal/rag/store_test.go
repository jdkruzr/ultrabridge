package rag

import (
	"context"
	"database/sql"
	"log/slog"
	"testing"

	"github.com/sysop/ultrabridge/internal/notedb"
)

// openTestDB opens an in-memory SQLite database with the notes schema.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	return db
}

// TestStore_Save verifies AC1.1:
// Save writes to note_embeddings table and can be read back with correct values.
// Also verifies UPSERT behavior on duplicate (note_path, page).
func TestStore_Save(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	logger := slog.Default()
	store := NewStore(db, logger)

	ctx := context.Background()

	// First save
	embedding1 := []float32{0.1, 0.2, 0.3}
	if err := store.Save(ctx, "note1.note", 0, 0, embedding1, "test-model"); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Verify it's in the database
	var blob []byte
	var model string
	err := db.QueryRowContext(ctx,
		`SELECT embedding, model FROM note_embeddings WHERE note_path = ? AND page = ?`,
		"note1.note", 0).Scan(&blob, &model)
	if err != nil {
		t.Fatalf("query after save: %v", err)
	}

	// Verify round-trip float32 conversion
	recovered := bytesToFloat32s(blob)
	if len(recovered) != 3 {
		t.Errorf("expected 3-dim, got %d", len(recovered))
	}
	for i, v := range recovered {
		if v != embedding1[i] {
			t.Errorf("embedding[%d]: expected %f, got %f", i, embedding1[i], v)
		}
	}
	if model != "test-model" {
		t.Errorf("model: expected 'test-model', got %q", model)
	}

	// Update with UPSERT
	embedding2 := []float32{0.4, 0.5, 0.6}
	if err := store.Save(ctx, "note1.note", 0, 0, embedding2, "new-model"); err != nil {
		t.Fatalf("Save (update) failed: %v", err)
	}

	// Verify updated value
	err = db.QueryRowContext(ctx,
		`SELECT embedding, model FROM note_embeddings WHERE note_path = ? AND page = ?`,
		"note1.note", 0).Scan(&blob, &model)
	if err != nil {
		t.Fatalf("query after update: %v", err)
	}

	recovered = bytesToFloat32s(blob)
	for i, v := range recovered {
		if v != embedding2[i] {
			t.Errorf("after update: embedding[%d]: expected %f, got %f", i, embedding2[i], v)
		}
	}
	if model != "new-model" {
		t.Errorf("after update: model: expected 'new-model', got %q", model)
	}

	// Verify only one row exists (UPSERT, not duplicate insert)
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM note_embeddings WHERE note_path = ? AND page = ?`,
		"note1.note", 0).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 row, got %d", count)
	}
}

// TestStore_SaveUpdatesCache verifies that Save also updates the in-memory cache.
func TestStore_SaveUpdatesCache(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	logger := slog.Default()
	store := NewStore(db, logger)

	ctx := context.Background()

	embedding := []float32{0.1, 0.2, 0.3}
	if err := store.Save(ctx, "note1.note", 0, 0, embedding, "model1"); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Check cache
	cached := store.AllEmbeddings()
	if len(cached) != 1 {
		t.Fatalf("cache: expected 1 record, got %d", len(cached))
	}
	if cached[0].NotePath != "note1.note" {
		t.Errorf("cache[0].NotePath: expected 'note1.note', got %q", cached[0].NotePath)
	}
	if cached[0].Page != 0 {
		t.Errorf("cache[0].Page: expected 0, got %d", cached[0].Page)
	}
	for i, v := range cached[0].Embedding {
		if v != embedding[i] {
			t.Errorf("cache[0].Embedding[%d]: expected %f, got %f", i, embedding[i], v)
		}
	}
}

func TestStore_Delete(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	store := NewStore(db, slog.Default())
	ctx := context.Background()

	if err := store.Save(ctx, "keep.note", 0, 0, []float32{0.1, 0.2}, "m"); err != nil {
		t.Fatalf("Save keep: %v", err)
	}
	if err := store.Save(ctx, "gone.note", 0, 0, []float32{0.3, 0.4}, "m"); err != nil {
		t.Fatalf("Save gone: %v", err)
	}

	if err := store.Delete(ctx, "gone.note"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Cache reflects the delete.
	cached := store.AllEmbeddings()
	if len(cached) != 1 || cached[0].NotePath != "keep.note" {
		t.Errorf("after delete, cache = %+v, want only keep.note", cached)
	}

	// DB reflects the delete (reload from disk into a fresh cache).
	fresh := NewStore(db, slog.Default())
	n, err := fresh.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if n != 1 {
		t.Errorf("after delete, DB has %d rows, want 1", n)
	}

	// Deleting a non-existent path is a no-op, not an error.
	if err := store.Delete(ctx, "never.note"); err != nil {
		t.Errorf("delete of missing path should be a no-op, got %v", err)
	}
}

// TestStore_LoadAll verifies AC1.6:
// LoadAll returns correct count and populates cache. AllEmbeddings() returns the loaded records.
func TestStore_LoadAll(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	logger := slog.Default()
	store := NewStore(db, logger)

	ctx := context.Background()

	// Insert some embeddings directly into DB (simulating existing data)
	embeddings := map[string]map[int][]float32{
		"note1.note": {0: {0.1, 0.2}, 1: {0.3, 0.4}},
		"note2.note": {0: {0.5, 0.6}},
	}

	for notePath, pages := range embeddings {
		for page, vec := range pages {
			blob := float32sToBytes(vec)
			if _, err := db.ExecContext(ctx,
				`INSERT INTO note_embeddings (note_path, page, embedding, model, created_at) VALUES (?, ?, ?, ?, ?)`,
				notePath, page, blob, "test-model", 0); err != nil {
				t.Fatalf("insert embedding: %v", err)
			}
		}
	}

	// LoadAll should read all 3 embeddings
	count, err := store.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}
	if count != 3 {
		t.Errorf("LoadAll: expected count 3, got %d", count)
	}

	// Check cache
	cached := store.AllEmbeddings()
	if len(cached) != 3 {
		t.Errorf("AllEmbeddings: expected 3 records, got %d", len(cached))
	}

	// Verify all embeddings are present with correct values
	foundRecords := make(map[string]map[int]bool)
	for _, rec := range cached {
		if foundRecords[rec.NotePath] == nil {
			foundRecords[rec.NotePath] = make(map[int]bool)
		}
		foundRecords[rec.NotePath][rec.Page] = true

		expectedVec := embeddings[rec.NotePath][rec.Page]
		for i, v := range rec.Embedding {
			if v != expectedVec[i] {
				t.Errorf("%s page %d: embedding[%d]: expected %f, got %f",
					rec.NotePath, rec.Page, i, expectedVec[i], v)
			}
		}
	}

	// Verify we found all expected combinations
	for notePath, pages := range embeddings {
		for page := range pages {
			if !foundRecords[notePath][page] {
				t.Errorf("missing embedding for %s page %d", notePath, page)
			}
		}
	}
}

// TestStore_UnembeddedPages verifies that UnembeddedPages returns only pages
// without embeddings.
func TestStore_UnembeddedPages(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	logger := slog.Default()
	store := NewStore(db, logger)

	ctx := context.Background()

	// Insert note_content rows (some with embeddings, some without)
	noteContentRows := []struct {
		notePath string
		page     int
		bodyText string
		hasEmbed bool
	}{
		{"note1.note", 0, "text for page 0", false},
		{"note1.note", 1, "text for page 1", true},
		{"note1.note", 2, "text for page 2", false},
		{"note2.note", 0, "text in note2 page 0", true},
		{"note2.note", 1, "text in note2 page 1", false},
		{"note3.note", 0, "", false}, // Empty text should be skipped
	}

	for _, row := range noteContentRows {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO note_content (note_path, page, body_text, indexed_at) VALUES (?, ?, ?, ?)`,
			row.notePath, row.page, row.bodyText, 0); err != nil {
			t.Fatalf("insert note_content: %v", err)
		}
	}

	// Insert some embeddings (for pages marked hasEmbed=true)
	for _, row := range noteContentRows {
		if row.hasEmbed {
			blob := float32sToBytes([]float32{0.1})
			if _, err := db.ExecContext(ctx,
				`INSERT INTO note_embeddings (note_path, page, embedding, model, created_at) VALUES (?, ?, ?, ?, ?)`,
				row.notePath, row.page, blob, "model", 0); err != nil {
				t.Fatalf("insert embedding: %v", err)
			}
		}
	}

	// UnembeddedPages should return only non-embedded pages with non-empty text
	unembedded, err := store.UnembeddedPages(ctx)
	if err != nil {
		t.Fatalf("UnembeddedPages failed: %v", err)
	}

	if len(unembedded) != 3 {
		t.Errorf("expected 3 unembedded pages, got %d", len(unembedded))
		for _, p := range unembedded {
			t.Logf("  %s page %d: %q", p.NotePath, p.Page, p.BodyText)
		}
	}

	// Verify the correct pages are returned
	expected := map[string]map[int]bool{
		"note1.note": {0: true, 2: true},
		"note2.note": {1: true},
	}

	for _, p := range unembedded {
		if !expected[p.NotePath][p.Page] {
			t.Errorf("unexpected unembedded page: %s page %d", p.NotePath, p.Page)
		}
	}
}

// TestCosineSimilarity verifies cosine similarity computation.
func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name     string
		a        []float32
		b        []float32
		expected float32
		desc     string
	}{
		{
			name:     "identical vectors",
			a:        []float32{1.0, 0.0, 0.0},
			b:        []float32{1.0, 0.0, 0.0},
			expected: 1.0,
			desc:     "cosine similarity of identical vectors should be 1.0",
		},
		{
			name:     "orthogonal vectors",
			a:        []float32{1.0, 0.0, 0.0},
			b:        []float32{0.0, 1.0, 0.0},
			expected: 0.0,
			desc:     "cosine similarity of orthogonal vectors should be 0.0",
		},
		{
			name:     "opposite vectors",
			a:        []float32{1.0, 0.0, 0.0},
			b:        []float32{-1.0, 0.0, 0.0},
			expected: -1.0,
			desc:     "cosine similarity of opposite vectors should be -1.0",
		},
		{
			name:     "45-degree angle",
			a:        []float32{1.0, 1.0},
			b:        []float32{1.0, 0.0},
			expected: 1.0 / float32(1.41421356),
			desc:     "cosine similarity at 45 degrees should be 1/√2 ≈ 0.707",
		},
		{
			name:     "zero vector a",
			a:        []float32{0.0, 0.0, 0.0},
			b:        []float32{1.0, 0.0, 0.0},
			expected: 0.0,
			desc:     "cosine similarity with zero vector should be 0.0",
		},
		{
			name:     "zero vector b",
			a:        []float32{1.0, 0.0, 0.0},
			b:        []float32{0.0, 0.0, 0.0},
			expected: 0.0,
			desc:     "cosine similarity with zero vector should be 0.0",
		},
		{
			name:     "different length",
			a:        []float32{1.0, 0.0},
			b:        []float32{1.0, 0.0, 0.0},
			expected: 0.0,
			desc:     "cosine similarity of different-length vectors should be 0.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CosineSimilarity(tt.a, tt.b)

			// For floating point, allow small tolerance
			tolerance := float32(1e-5)
			if result < tt.expected-tolerance || result > tt.expected+tolerance {
				t.Errorf("%s: expected %.6f, got %.6f", tt.desc, tt.expected, result)
			}
		})
	}
}

// TestStore_AllEmbeddings verifies that AllEmbeddings returns a snapshot.
func TestStore_AllEmbeddings(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	logger := slog.Default()
	store := NewStore(db, logger)

	ctx := context.Background()

	// Save an embedding
	embedding := []float32{0.1, 0.2}
	if err := store.Save(ctx, "note1.note", 0, 0, embedding, "model"); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Get snapshot
	snapshot := store.AllEmbeddings()
	if len(snapshot) != 1 {
		t.Fatalf("expected 1 record in snapshot, got %d", len(snapshot))
	}

	// Verify snapshot is a copy (modifying it shouldn't affect cache)
	snapshot[0].Embedding[0] = 999.0

	// Get new snapshot
	snapshot2 := store.AllEmbeddings()
	if snapshot2[0].Embedding[0] != 0.1 {
		t.Errorf("modifying snapshot affected cache: expected 0.1, got %f", snapshot2[0].Embedding[0])
	}
}
