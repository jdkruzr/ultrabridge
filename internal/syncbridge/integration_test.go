package syncbridge

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/search"
	"github.com/sysop/ultrabridge/internal/syncstore"
)

// TestBridge_RealSearchRoundTrip is the Phase 2 payoff: a synced page's strokes
// are rendered + OCR'd, indexed via the REAL search.Store on a forestnote:// path,
// and a keyword search returns it. Uses notedb (which creates the FTS5 tables)
// plus the syncstore migration on the same DB.
func TestBridge_RealSearchRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := notedb.Open(ctx, filepath.Join(t.TempDir(), "notes.db"))
	if err != nil {
		t.Fatalf("notedb open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := syncstore.Migrate(ctx, db); err != nil {
		t.Fatalf("syncstore migrate: %v", err)
	}

	store := syncstore.New(db)
	seedPageWithStroke(t, store)

	searchStore := search.New(db) // the real FTS5 indexer
	bridge := New(store, Deps{Indexer: searchStore, OCR: fakeOCR{text: "the quick brown fox"}}, nil)

	bridge.processPage(ctx, pg1)

	results, err := searchStore.Search(ctx, search.SearchQuery{Text: "brown", Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	wantPath := "forestnote://" + nb1 + "/" + pg1
	found := false
	for _, r := range results {
		if r.Path == wantPath {
			found = true
		}
	}
	if !found {
		t.Errorf("search for 'brown' did not return the synced page %q; results=%+v", wantPath, results)
	}
}
