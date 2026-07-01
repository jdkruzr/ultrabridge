package forestnote

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sysop/ultrabridge/internal/source"
	"github.com/sysop/ultrabridge/internal/syncbridge"
	"github.com/sysop/ultrabridge/internal/syncstore"
)

// nopIndexer satisfies syncbridge.Indexer; the source needs a non-nil one.
type nopIndexer struct{}

func (nopIndexer) IndexPage(context.Context, string, int, string, string, string, string) error {
	return nil
}
func (nopIndexer) Delete(context.Context, string) error { return nil }

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "n.db") + "?_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestNewSource_DefaultsBatchLimit(t *testing.T) {
	db := testDB(t)
	// Empty config_json → default batch limit.
	s, err := NewSource(db, source.SourceRow{Type: "forestnote", Name: "FN"}, source.SharedDeps{}, ForestNoteDeps{Indexer: nopIndexer{}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if s.cfg.BatchLimit != defaultBatchLimit {
		t.Errorf("batch limit = %d, want %d", s.cfg.BatchLimit, defaultBatchLimit)
	}
	if s.Type() != "forestnote" || s.Name() != "FN" {
		t.Errorf("type/name = %q/%q", s.Type(), s.Name())
	}
}

func TestNewSource_ParsesConfig(t *testing.T) {
	db := testDB(t)
	s, err := NewSource(db, source.SourceRow{ConfigJSON: `{"batch_limit":42}`}, source.SharedDeps{}, ForestNoteDeps{Indexer: nopIndexer{}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if s.cfg.BatchLimit != 42 {
		t.Errorf("batch limit = %d, want 42", s.cfg.BatchLimit)
	}
}

func TestSource_StartBuildsServiceAndStore(t *testing.T) {
	db := testDB(t)
	s, err := NewSource(db, source.SourceRow{Name: "FN"}, source.SharedDeps{}, ForestNoteDeps{Indexer: nopIndexer{}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()
	if s.SyncService() == nil {
		t.Error("SyncService nil after Start")
	}
	if s.Store() == nil {
		t.Error("Store nil after Start")
	}
}

func TestSource_StopSafeBeforeStart(t *testing.T) {
	db := testDB(t)
	s, _ := NewSource(db, source.SourceRow{}, source.SharedDeps{}, ForestNoteDeps{Indexer: nopIndexer{}})
	s.Stop() // must not panic before Start
}

// countingIndexer signals each IndexPage call on a channel so a test can wait
// for the bridge to drain the reprocess queue.
type countingIndexer struct{ indexed chan string }

func (c countingIndexer) IndexPage(_ context.Context, path string, _ int, _, _, _, _ string) error {
	c.indexed <- path
	return nil
}
func (countingIndexer) Delete(context.Context, string) error { return nil }

func TestReprocessNotebook_EnqueuesLivePages(t *testing.T) {
	db := testDB(t)
	idx := countingIndexer{indexed: make(chan string, 8)}
	s, err := NewSource(db, source.SourceRow{Name: "FN"}, source.SharedDeps{}, ForestNoteDeps{Indexer: idx})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()

	// Seed a notebook with two pages, each carrying a stroke (so render produces
	// content and the bridge calls IndexPage rather than dropPage).
	const (
		site = "0000000000000000000000SYTE"
		nb   = "00000000000000000000000NBA"
		pgA  = "00000000000000000000000PGA"
		pgB  = "00000000000000000000000PGB"
		stA  = "00000000000000000000000STA"
		stB  = "00000000000000000000000STB"
	)
	pts := "MgAAADwAAADIAAAAAAAAAAEAAAA="
	mkPage := func(seq, wall int64, pk string) syncstore.Op {
		return syncstore.Op{Table: "page", PK: pk, SiteID: site, OpSeq: seq, WallTS: wall,
			Cols: map[string]any{"notebook_id": nb, "sort_order": float64(0), "created_at": float64(1000), "deleted_at": nil, "template": nil, "template_pitch_mm": nil}}
	}
	mkStroke := func(seq, wall int64, pk, page string) syncstore.Op {
		return syncstore.Op{Table: "stroke", PK: pk, SiteID: site, OpSeq: seq, WallTS: wall,
			Cols: map[string]any{"page_id": page, "color": float64(4278190080), "pen_width_min": float64(2), "pen_width_max": float64(6), "points": pts, "z": float64(0), "created_at": float64(1000), "deleted_at": nil}}
	}
	if _, err := s.Store().ApplyBatch(context.Background(), site, []syncstore.Op{
		{Table: "notebook", PK: nb, SiteID: site, OpSeq: 1, WallTS: 1000, Cols: map[string]any{"name": "NB", "sort_order": float64(0), "created_at": float64(1000), "deleted_at": nil, "folder_id": nil, "aspect_long_axis": nil}},
		mkPage(2, 1010, pgA), mkPage(3, 1020, pgB),
		mkStroke(4, 1030, stA, pgA), mkStroke(5, 1040, stB, pgB),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := s.ReprocessNotebook(context.Background(), nb); err != nil {
		t.Fatalf("reprocess: %v", err)
	}

	got := map[string]bool{}
	timeout := time.After(5 * time.Second)
	for len(got) < 2 {
		select {
		case p := <-idx.indexed:
			got[p] = true
		case <-timeout:
			t.Fatalf("timed out; indexed only %d pages: %v", len(got), got)
		}
	}
}

func TestReprocessNotebook_ErrorsBeforeStart(t *testing.T) {
	db := testDB(t)
	s, _ := NewSource(db, source.SourceRow{}, source.SharedDeps{}, ForestNoteDeps{Indexer: nopIndexer{}})
	if err := s.ReprocessNotebook(context.Background(), "00000000000000000000000NBA"); err == nil {
		t.Error("expected error reprocessing before Start")
	}
}

// compile-time: *Source satisfies source.Source.
var _ source.Source = (*Source)(nil)

// compile-time: nopIndexer satisfies syncbridge.Indexer.
var _ syncbridge.Indexer = nopIndexer{}
