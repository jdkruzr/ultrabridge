package rag

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/notedb"
)

// seqEmbedder returns a distinct fixed-dim vector per call; optionally fails.
type seqEmbedder struct {
	fail bool
	n    int
}

func (e *seqEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	if e.fail {
		return nil, fmt.Errorf("boom")
	}
	e.n++
	return []float32{float32(e.n), 0.5, 0.5}, nil
}

func TestEmbedAndStorePage_ChunksLongText(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()
	store := NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	emb := &seqEmbedder{}

	long := strings.TrimSpace(strings.Repeat("lorem ipsum dolor sit amet ", 400)) // ~10.8k → multiple chunks
	wantChunks := len(ChunkText(long))
	if wantChunks < 2 {
		t.Fatalf("test setup: expected multi-chunk text, got %d", wantChunks)
	}

	n := EmbedAndStorePage(ctx, emb, store, "/n.note", 0, long, "m", nil)
	if n != wantChunks {
		t.Errorf("stored %d chunks, want %d", n, wantChunks)
	}

	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM note_embeddings WHERE note_path=? AND page=0`, "/n.note").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != wantChunks {
		t.Errorf("db has %d chunk rows, want %d", rows, wantChunks)
	}
}

func TestEmbedAndStorePage_ReindexReplacesStaleChunks(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()
	store := NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	emb := &seqEmbedder{}

	long := strings.TrimSpace(strings.Repeat("alpha beta gamma delta ", 300))
	EmbedAndStorePage(ctx, emb, store, "/n.note", 0, long, "m", nil)
	// Re-index the same page with much shorter text → old chunks must be cleared.
	EmbedAndStorePage(ctx, emb, store, "/n.note", 0, "short now", "m", nil)

	var rows int
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM note_embeddings WHERE note_path=? AND page=0`, "/n.note").Scan(&rows)
	if rows != 1 {
		t.Errorf("after re-index with short text, expected 1 chunk, got %d (stale chunks not cleared)", rows)
	}
}

func TestEmbedAndStorePage_AllFailKeepsPrior(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()
	store := NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// First a successful embed.
	EmbedAndStorePage(ctx, &seqEmbedder{}, store, "/n.note", 0, "hello world", "m", nil)
	// Then a re-index where embedding fails entirely → must NOT delete prior.
	n := EmbedAndStorePage(ctx, &seqEmbedder{fail: true}, store, "/n.note", 0, "new text", "m", nil)
	if n != 0 {
		t.Errorf("failed embed should store 0, got %d", n)
	}
	var rows int
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM note_embeddings WHERE note_path=?`, "/n.note").Scan(&rows)
	if rows != 1 {
		t.Errorf("prior embedding should survive a full failure, got %d rows", rows)
	}
}

func TestEmbedAndStorePage_EmptyDropsPage(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()
	store := NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	EmbedAndStorePage(ctx, &seqEmbedder{}, store, "/n.note", 0, "hello", "m", nil)
	EmbedAndStorePage(ctx, &seqEmbedder{}, store, "/n.note", 0, "   ", "m", nil) // emptied
	var rows int
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM note_embeddings WHERE note_path=?`, "/n.note").Scan(&rows)
	if rows != 0 {
		t.Errorf("emptied page should drop embeddings, got %d rows", rows)
	}
}
