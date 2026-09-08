package synchttp_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/synchttp"
	"github.com/sysop/ultrabridge/internal/syncstore"
	"github.com/sysop/ultrabridge/internal/syncsvc"
	_ "modernc.org/sqlite"
)

const boundedA = "0000000000000000000000000A"
const boundedB = "0000000000000000000000000B"

type boundedBridge struct{ calls int }

func (b *boundedBridge) PagesChanged(context.Context, []syncstore.TablePK) { b.calls++ }

func boundedServer(t *testing.T) (*sql.DB, *syncstore.Store, http.Handler, *boundedBridge) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := syncstore.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	s := syncstore.New(db)
	bridge := &boundedBridge{}
	return db, s, synchttp.New(syncsvc.New(s, 500, bridge, nil), synchttp.DefaultMaxBytes, nil), bridge
}
func boundedNotebook(site string, seq int64, name string) syncstore.Op {
	return syncstore.Op{Table: "notebook", PK: "00000000000000000000000NB1", SiteID: site, OpSeq: seq, WallTS: seq,
		Cols: map[string]any{"name": name, "sort_order": float64(0), "created_at": float64(1), "deleted_at": nil, "folder_id": nil, "aspect_long_axis": nil}}
}
func boundedPost(t *testing.T, h http.Handler, ops []syncstore.Op, cursor int64, bodyLimit, rowLimit string, bounded bool) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(syncsvc.Request{ProtocolVersion: 1, SchemaHash: syncstore.SchemaHash(), SiteID: boundedA, Cursor: cursor, Ops: ops})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/sync/v1", bytes.NewReader(body))
	if bounded {
		r.Header.Set("X-Rhizome-Bounded-Rows", "1")
	}
	if bodyLimit != "" {
		r.Header.Set("X-Rhizome-Max-Response-Bytes", bodyLimit)
	}
	if rowLimit != "" {
		r.Header.Set("X-Rhizome-Max-Row-Bytes", rowLimit)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func boundedSnapshot(t *testing.T, db *sql.DB) []any {
	t.Helper()
	var out []any
	for _, query := range []string{
		`SELECT last_seq FROM sync_seq`, `SELECT last_hlc FROM sync_site`,
		`SELECT seq,site_id,op_seq,payload FROM sync_ops ORDER BY seq`,
		`SELECT site_id,acked_op_seq,last_pull_seq,updated_at,device_name FROM sync_cursors ORDER BY site_id`,
		`SELECT id,name,lww_wall_ts,lww_op_seq,lww_site_id FROM fn_notebook ORDER BY id`,
	} {
		rows, err := db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out = append(out, values)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return out
}

func TestBoundedOversizedPullRollsBackPushMirrorClockAndCursors(t *testing.T) {
	db, s, h, bridge := boundedServer(t)
	if _, err := s.ApplyBatch(context.Background(), boundedB, []syncstore.Op{boundedNotebook(boundedB, 1, strings.Repeat("漢字🙂", 300))}); err != nil {
		t.Fatal(err)
	}
	before := boundedSnapshot(t, db)
	w := boundedPost(t, h, []syncstore.Op{boundedNotebook(boundedA, 1, "queued")}, 0, "1024", "512", true)
	if w.Code != 413 || !strings.Contains(w.Body.String(), "oversized_op") {
		t.Fatal(w.Code, w.Body.String())
	}
	if !reflect.DeepEqual(before, boundedSnapshot(t, db)) {
		t.Fatal("413 changed durable state")
	}
	if bridge.calls != 0 {
		t.Fatal("pipeline notified before commit")
	}
	// Same request without the opt-in header retains legacy semantics.
	w = boundedPost(t, h, []syncstore.Op{boundedNotebook(boundedA, 1, "queued")}, 0, "1024", "512", false)
	if w.Code != 200 || w.Body.Len() <= 1024 {
		t.Fatal(w.Code, w.Body.Len())
	}
}

func TestBoundedCommitFailureAndOversizedRejectionsAreAtomic(t *testing.T) {
	db, _, h, _ := boundedServer(t)
	before := boundedSnapshot(t, db)
	if _, err := db.Exec(`CREATE TRIGGER reject_cursor BEFORE UPDATE OF last_pull_seq ON sync_cursors BEGIN SELECT RAISE(ABORT,'injected cursor failure'); END`); err != nil {
		t.Fatal(err)
	}
	w := boundedPost(t, h, []syncstore.Op{boundedNotebook(boundedA, 1, "queued")}, 0, "", "", true)
	if w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
	if !reflect.DeepEqual(before, boundedSnapshot(t, db)) {
		t.Fatal("failed commit changed state")
	}
	if _, err := db.Exec(`DROP TRIGGER reject_cursor`); err != nil {
		t.Fatal(err)
	}
	var bad []syncstore.Op
	for i := int64(1); i <= 10; i++ {
		bad = append(bad, syncstore.Op{Table: "unknown", PK: "bad", SiteID: boundedA, OpSeq: i})
	}
	w = boundedPost(t, h, bad, 0, "512", "512", true)
	if w.Code != 413 {
		t.Fatal(w.Code, w.Body.String())
	}
	if !reflect.DeepEqual(before, boundedSnapshot(t, db)) {
		t.Fatal("oversized rejection envelope acknowledged input")
	}
}

func TestBoundedPaginationRespectsByteAndCountCaps(t *testing.T) {
	_, s, h, _ := boundedServer(t)
	var ops []syncstore.Op
	for i := int64(1); i <= 501; i++ {
		ops = append(ops, boundedNotebook(boundedB, i, "text <&> 漢字"))
	}
	if _, err := s.ApplyBatch(context.Background(), boundedB, ops); err != nil {
		t.Fatal(err)
	}
	w := boundedPost(t, h, nil, 0, "", "", true)
	var response syncsvc.Response
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(response.Ops) != 500 || !response.HasMore || response.Cursor != 500 {
		t.Fatal(len(response.Ops), response.Cursor, response.HasMore)
	}
	w = boundedPost(t, h, nil, 0, "1024", "1024", true)
	if w.Code != 200 || w.Body.Len() > 1024 {
		t.Fatal(w.Code, w.Body.Len())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Ops) == 0 || len(response.Ops) >= 500 || !response.HasMore {
		t.Fatal(len(response.Ops), response.HasMore)
	}
	w = boundedPost(t, h, nil, 500, "1024", "1024", true)
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Ops) != 1 || response.HasMore || response.Cursor != 501 {
		t.Fatal(response)
	}
}

func TestBoundedInvalidBudgetsNeverTouchStorage(t *testing.T) {
	db, _, h, _ := boundedServer(t)
	before := boundedSnapshot(t, db)
	for _, limit := range []string{"-1", "0", "1", "x", "999999999999999999999"} {
		w := boundedPost(t, h, []syncstore.Op{boundedNotebook(boundedA, 1, "queued")}, 0, limit, "", true)
		if w.Code != 400 {
			t.Fatal(limit, w.Code, w.Body.String())
		}
	}
	if !reflect.DeepEqual(before, boundedSnapshot(t, db)) {
		t.Fatal("bad budget changed state")
	}
}
