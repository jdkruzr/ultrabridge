package synchttp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sysop/ultrabridge/internal/synchttp"
	"github.com/sysop/ultrabridge/internal/syncstore"
	"github.com/sysop/ultrabridge/internal/syncsvc"
)

// End-to-end Phase 1 exit test: real syncstore + syncsvc + synchttp, driven over
// HTTP with JSON, two devices converging through the relay + LWW merge.

const (
	siteA = "0000000000000000000000000A"
	siteB = "0000000000000000000000000B"
	siteC = "0000000000000000000000000C"
	nb1   = "00000000000000000000000NB1"
)

func newStack(t *testing.T) http.Handler {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)",
		filepath.Join(t.TempDir(), "sync.db"))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := syncstore.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := syncsvc.New(syncstore.New(db), 0, nil, nil)
	return synchttp.New(svc, synchttp.DefaultMaxBytes, nil)
}

func nbOp(site string, opSeq, wall int64, name string) syncstore.Op {
	return syncstore.Op{
		Table: "notebook", PK: nb1, SiteID: site, OpSeq: opSeq, WallTS: wall,
		Cols: map[string]any{"name": name, "sort_order": float64(0), "created_at": float64(1000), "deleted_at": nil, "folder_id": nil},
	}
}

func sync(t *testing.T, h http.Handler, r syncsvc.Request) (int, syncsvc.Response) {
	t.Helper()
	body, _ := json.Marshal(r)
	req := httptest.NewRequest(http.MethodPost, "/sync/v1", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var resp syncsvc.Response
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode resp: %v (body=%s)", err, w.Body.String())
		}
	}
	return w.Code, resp
}

func valid(site string, cursor int64, ops ...syncstore.Op) syncsvc.Request {
	return syncsvc.Request{
		ProtocolVersion: syncsvc.ProtocolVersion,
		SchemaHash:      syncstore.SchemaHash(),
		SiteID:          site,
		Cursor:          cursor,
		Ops:             ops,
	}
}

func TestE2E_TwoDevicesConverge(t *testing.T) {
	h := newStack(t)

	// A creates the notebook (wall 1000).
	if code, resp := sync(t, h, valid(siteA, 0, nbOp(siteA, 1, 1000, "from-A"))); code != 200 || resp.AcceptedThrough != 1 {
		t.Fatalf("A push: code=%d accepted=%d", code, resp.AcceptedThrough)
	}

	// B pulls — receives A's op (self-exclusion means A wouldn't, B does).
	code, bPull := sync(t, h, valid(siteB, 0))
	if code != 200 || len(bPull.Ops) != 1 || bPull.Ops[0].SiteID != siteA {
		t.Fatalf("B pull: code=%d ops=%+v", code, bPull.Ops)
	}

	// B renames the same notebook with a HIGHER wall_ts (concurrent edit, B wins).
	if code, _ := sync(t, h, valid(siteB, bPull.Cursor, nbOp(siteB, 1, 2000, "from-B"))); code != 200 {
		t.Fatalf("B push: code=%d", code)
	}

	// A pulls — receives B's winning op.
	code, aPull := sync(t, h, valid(siteA, 1)) // A already has its own op (seq 1)
	if code != 200 || len(aPull.Ops) != 1 || aPull.Ops[0].SiteID != siteB {
		t.Fatalf("A pull: code=%d ops=%+v", code, aPull.Ops)
	}

	// A fresh observer C pulls the whole changelog and merges it locally; the
	// converged state must be B's rename (higher wall_ts wins, spec §5).
	code, cPull := sync(t, h, valid(siteC, 0))
	if code != 200 {
		t.Fatalf("C pull: code=%d", code)
	}
	winners := syncstore.Merge(cPull.Ops)
	got := winners[syncstore.TablePK{Table: "notebook", PK: nb1}]
	if got.Cols["name"] != "from-B" {
		t.Errorf("converged name = %v, want from-B (LWW by wall_ts)", got.Cols["name"])
	}
}

func TestE2E_SchemaMismatchIs409(t *testing.T) {
	h := newStack(t)
	r := valid(siteA, 0, nbOp(siteA, 1, 1000, "x"))
	r.SchemaHash = "wronghash"
	if code, _ := sync(t, h, r); code != http.StatusConflict {
		t.Errorf("schema mismatch: code=%d, want 409", code)
	}
}
