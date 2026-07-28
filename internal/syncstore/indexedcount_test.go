package syncstore

import (
	"context"
	"testing"
)

// CountIndexedPages backs the ForestNote "N indexed" figure in the pipeline
// bar. It exists because the bridge's Processed counter is in-memory and
// monotonic since process start — durable output has to be counted, not
// remembered. What it must NOT count is work whose subject is gone: the other
// sources' done counts drop when a note is deleted (Boox removes the job rows
// with the note), so this one joins through page and notebook to match.
func TestCountIndexedPages(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	const (
		nbLive = "00000000000000000000000NBA"
		nbGone = "00000000000000000000000NBB"
	)
	pages := []struct{ id, nb string }{
		{"00000000000000000000000PGA", nbLive}, // text → counts
		{"00000000000000000000000PGB", nbLive}, // no text → doesn't count
		{"00000000000000000000000PGC", nbLive}, // empty text → doesn't count
		{"00000000000000000000000PGD", nbLive}, // text, then tombstoned → doesn't count
		{"00000000000000000000000PGE", nbLive}, // text, page deleted → doesn't count
		{"00000000000000000000000PGF", nbGone}, // text, notebook deleted → doesn't count
	}

	var ops []Op
	var seq int64
	next := func() int64 { seq++; return seq }
	ops = append(ops,
		Op{Table: "notebook", PK: nbLive, SiteID: siteA, OpSeq: next(), WallTS: 1000,
			Cols: map[string]any{"name": "Live", "sort_order": float64(0), "created_at": float64(1000), "deleted_at": nil, "folder_id": nil, "aspect_long_axis": nil}},
		Op{Table: "notebook", PK: nbGone, SiteID: siteA, OpSeq: next(), WallTS: 1000,
			Cols: map[string]any{"name": "Deleted", "sort_order": float64(0), "created_at": float64(1000), "deleted_at": float64(2000), "folder_id": nil, "aspect_long_axis": nil}},
	)
	for i, p := range pages {
		var deletedAt any
		if p.id == "00000000000000000000000PGE" {
			deletedAt = float64(2000)
		}
		ops = append(ops, Op{Table: "page", PK: p.id, SiteID: siteA, OpSeq: next(), WallTS: 1000,
			Cols: map[string]any{"notebook_id": p.nb, "sort_order": float64(i), "created_at": float64(1000),
				"deleted_at": deletedAt, "template": nil, "template_pitch_mm": nil}})
	}
	if _, err := s.ApplyBatch(ctx, siteA, ops); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for _, p := range []struct{ id, text string }{
		{"00000000000000000000000PGA", "handwriting"},
		{"00000000000000000000000PGC", ""},
		{"00000000000000000000000PGD", "handwriting"},
		{"00000000000000000000000PGE", "handwriting"},
		{"00000000000000000000000PGF", "handwriting"},
	} {
		if err := s.AuthorPageText(ctx, p.id, p.text, 1000, "modelX"); err != nil {
			t.Fatalf("author %s: %v", p.id, err)
		}
	}
	if err := s.AuthorPageTextTombstone(ctx, "00000000000000000000000PGD"); err != nil {
		t.Fatalf("tombstone: %v", err)
	}

	got, err := s.CountIndexedPages(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 1 {
		t.Errorf("CountIndexedPages = %d, want 1 (only PGA: live page, live notebook, non-empty untombstoned text)", got)
	}
}

func TestCountIndexedPages_EmptyMirror(t *testing.T) {
	got, err := newTestStore(t).CountIndexedPages(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 0 {
		t.Errorf("CountIndexedPages on an empty mirror = %d, want 0", got)
	}
}
