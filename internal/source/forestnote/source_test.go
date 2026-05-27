package forestnote

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sysop/ultrabridge/internal/source"
	"github.com/sysop/ultrabridge/internal/syncbridge"
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

// compile-time: *Source satisfies source.Source.
var _ source.Source = (*Source)(nil)

// compile-time: nopIndexer satisfies syncbridge.Indexer.
var _ syncbridge.Indexer = nopIndexer{}
