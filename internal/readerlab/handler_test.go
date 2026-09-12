package readerlab

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jdkruzr/rhizome/server-go/assets"
	"github.com/jdkruzr/rhizome/server-go/bounded"
	"github.com/sysop/ultrabridge/internal/readercontract"
	"github.com/sysop/ultrabridge/internal/readerstore"
	"github.com/sysop/ultrabridge/internal/syncstore"
	_ "modernc.org/sqlite"
)

var ctx = context.Background()

func TestCombinedAssetsRequireExplicitAdmissionAndFixtureAuth(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("assets-%v", enabled), func(t *testing.T) {
			db, _ := open(t, filepath.Join(t.TempDir(), "combined.db"))
			if err := (&assets.SQLStore{DB: db}).Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			h, err := HandlerWithAssets(ctx, db, nil, nil, enabled)
			if err != nil {
				t.Fatal(err)
			}
			s := httptest.NewServer(h)
			defer s.Close()
			call := func(method, path, user, body string) (int, []byte) {
				t.Helper()
				req, _ := http.NewRequest(method, s.URL+path, strings.NewReader(body))
				if user != "" {
					req.SetBasicAuth(user, "readerlab")
				}
				res, err := s.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				b, err := io.ReadAll(res.Body)
				if err != nil {
					t.Fatal(err)
				}
				return res.StatusCode, b
			}
			for _, path := range []string{"/sync/capabilities", "/sync/assets/v1/" + bookID, "/sync/assets/v1/" + bookID + "/chunks/0", "/reader/search"} {
				code, _ := call("GET", path, "", "")
				if code != 401 {
					t.Fatalf("unprotected %s: %d", path, code)
				}
			}
			code, caps := call("GET", "/sync/capabilities", "reader-a", "")
			if code != 200 || bytes.Contains(caps, []byte(`"assets-v1"`)) != enabled {
				t.Fatalf("bad capabilities: %s", caps)
			}
			code, body := call("PUT", "/sync/assets/v1/"+bookID, "reader-a", `{"asset_id":"`+bookID+`","byte_length":"10","chunk_bytes":262144}`)
			want := 404
			if enabled {
				want = 201
			}
			if code != want {
				t.Fatalf("stage: %d %s", code, body)
			}
			if enabled {
				code, _ = call("GET", "/sync/assets/v1/"+bookID, "reader-b", "")
				if code != 200 {
					t.Fatalf("same library peer cannot see asset: %d", code)
				}
			}
			post(t, s, "reader-b", request(SiteA), nil, 403)
		})
	}
}

const bookID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func open(t *testing.T, path string) (*sql.DB, *httptest.Server) {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := syncstore.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	h, err := Handler(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	t.Cleanup(func() { s.Close(); db.Close() })
	return db, s
}
func op(site, table, pk string, seq int64, cols map[string]any) json.RawMessage {
	b, err := json.Marshal(map[string]any{"table": table, "pk": pk, "site_id": site, "op_seq": seq, "op_ts": seq, "cols": cols})
	if err != nil {
		panic(err)
	}
	return b
}
func title(site string, seq int64, value any) json.RawMessage {
	return op(site, "reader_book_title", bookID, seq, map[string]any{"title": value})
}
func notebook(site string, seq int64) json.RawMessage {
	return op(site, "notebook", "00000000000000000000000001", seq, map[string]any{"name": "Writer survives", "sort_order": 0, "created_at": 1, "deleted_at": nil, "folder_id": nil, "aspect_long_axis": nil, "page_width": nil, "page_height": nil})
}
func request(site string, ops ...json.RawMessage) bounded.Request {
	return bounded.Request{ProtocolVersion: 1, SchemaHash: readercontract.CandidateCombined().SchemaHash(), SiteID: site, Ops: ops}
}
func post(t *testing.T, s *httptest.Server, user string, req bounded.Request, headers map[string]string, want int) bounded.Response {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequest("POST", s.URL+"/sync/v1", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.SetBasicAuth(user, "readerlab")
	r.Header.Set(bounded.Header, "1")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	res, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != want {
		t.Fatalf("HTTP %d want %d: %s", res.StatusCode, want, b)
	}
	var out bounded.Response
	if want == 200 {
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
	}
	return out
}
func scalar(t *testing.T, db *sql.DB, query string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func exec(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
}
func state(t *testing.T, db *sql.DB) []int64 {
	t.Helper()
	var out []int64
	for _, q := range []string{"SELECT count(*) FROM reader_store_incoming", "SELECT count(*) FROM sync_ops", "SELECT last_seq FROM sync_seq", "SELECT last_hlc FROM sync_site", "SELECT count(*) FROM sync_cursors", "SELECT COALESCE(sum(acked_op_seq),0) FROM sync_cursors", "SELECT COALESCE(sum(last_pull_seq),0) FROM sync_cursors", "SELECT count(*) FROM fn_notebook"} {
		out = append(out, scalar(t, db, q))
	}
	return out
}

func TestMixedGapRejectedReceiptRestartAndLosslessRelay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notedb.sqlite")
	db, s := open(t, path)
	book := op(SiteA, "reader_book", bookID, 1, map[string]any{"asset_id": bookID, "byte_length": int64(9007199254740993), "media_type": "application/epub+zip", "metadata_json": `{ "version":1,"title":"No Pancakes" }`})
	// First contact cannot reseed its ACK from this batch's inserts. An invalid
	// reader row above a missing writer op remains durably rejected across restart.
	r := post(t, s, "reader-a", request(SiteA, book, title(SiteA, 3, 42), title(SiteA, 4, "New title")), nil, 200)
	if r.AcceptedThrough != 1 || string(r.Rejected) == "[]" {
		t.Fatalf("wrong gap ACK: %+v", r)
	}
	s.Close()
	db.Close()
	db, s = open(t, path)
	r = post(t, s, "reader-a", request(SiteA, notebook(SiteA, 2)), nil, 200)
	if r.AcceptedThrough != 4 {
		t.Fatalf("durable rejection wedged ACK: %+v", r)
	}
	post(t, s, "reader-a", request(SiteA, book, title(SiteA, 3, 42), notebook(SiteA, 2)), nil, 200)
	if scalar(t, db, "SELECT count(*) FROM sync_ops") != 3 || scalar(t, db, "SELECT count(*) FROM reader_store_incoming") != 3 {
		t.Fatal("retry duplicated receipts/relay")
	}
	r = post(t, s, "reader-b", request(SiteB), nil, 200)
	if len(r.Ops) != 3 {
		t.Fatalf("wrong relay: %+v", r)
	}
	var exact bool
	for _, raw := range r.Ops {
		if bytes.Contains(raw, []byte(`"byte_length":9007199254740993`)) && bytes.Contains(raw, []byte(`{ \"version\":1`)) {
			exact = true
		}
	}
	if !exact {
		t.Fatal("reader payload rounded or selector string rewritten")
	}
	for i := 0; i < 3; i++ {
		if _, err := readerstore.New(db).Drain(ctx, 0, 128); err != nil {
			t.Fatal(err)
		}
	}
	if scalar(t, db, "SELECT byte_length FROM fn_reader_book") != 9007199254740993 || scalar(t, db, "SELECT lww_op_seq FROM fn_reader_book") != 1 {
		t.Fatal("projection lost precision/provenance")
	}
	if syncstore.AcceptsSchemaHash(readercontract.CandidateCombined().SchemaHash()) {
		t.Fatal("production hash activated")
	}
}

func TestBoundedFailureRollsBackAllParticipants(t *testing.T) {
	db, s := open(t, filepath.Join(t.TempDir(), "notedb.sqlite"))
	post(t, s, "reader-b", request(SiteB, title(SiteB, 1, strings.Repeat("large", 200))), nil, 200)
	before := state(t, db)
	req := request(SiteA, title(SiteA, 1, "reader"), notebook(SiteA, 2))
	post(t, s, "reader-a", req, map[string]string{"X-Rhizome-Max-Response-Bytes": "512"}, 413)
	if !reflect.DeepEqual(before, state(t, db)) {
		t.Fatal("413 committed receipt/relay/writer/clock/cursor/ACK")
	}
	post(t, s, "reader-a", req, nil, 200)
	// Even with no pulled row, the rejection envelope itself must fit before
	// committing its durable quarantine receipts and acknowledgement.
	before = state(t, db)
	var invalid []json.RawMessage
	for seq := int64(3); seq < 12; seq++ {
		invalid = append(invalid, title(SiteA, seq, 42))
	}
	post(t, s, "reader-a", request(SiteA, invalid...), map[string]string{"X-Rhizome-Max-Response-Bytes": "256"}, 413)
	if !reflect.DeepEqual(before, state(t, db)) {
		t.Fatal("oversized rejection envelope committed state")
	}
}

func TestInjectedCommitFailuresAndIdentityReuse(t *testing.T) {
	for _, table := range []string{"reader_store_incoming", "sync_ops", "sync_cursors"} {
		t.Run(table, func(t *testing.T) {
			db, s := open(t, filepath.Join(t.TempDir(), "notedb.sqlite"))
			before := state(t, db)
			exec(t, db, "CREATE TRIGGER fail BEFORE INSERT ON "+table+" BEGIN SELECT RAISE(ABORT,'fixture failure'); END")
			req := request(SiteA, title(SiteA, 1, "original"), notebook(SiteA, 2))
			post(t, s, "reader-a", req, nil, 503)
			if !reflect.DeepEqual(before, state(t, db)) {
				t.Fatal("failed transaction changed state")
			}
			exec(t, db, "DROP TRIGGER fail")
			post(t, s, "reader-a", req, nil, 200)
			before = state(t, db)
			for _, bad := range []bounded.Request{request(SiteA, title(SiteA, 1, "changed")), request(SiteA, notebook(SiteA, 1)), request(SiteA, title(SiteA, 2, "writer collision")), request(SiteA, title(SiteA, 3, "x"), notebook(SiteA, 3))} {
				post(t, s, "reader-a", bad, nil, 409)
				if !reflect.DeepEqual(before, state(t, db)) {
					t.Fatal("identity conflict changed state")
				}
			}
		})
	}
}

func TestCredentialBindingAndCandidateOnlyGate(t *testing.T) {
	db, s := open(t, filepath.Join(t.TempDir(), "notedb.sqlite"))
	before := state(t, db)
	post(t, s, "unknown", request(SiteA), nil, 401)
	post(t, s, "reader-a", request(SiteB), nil, 403)
	post(t, s, "reader-a", request(SiteA, title(SiteB, 1, "forged")), nil, 403)
	post(t, s, "reader-a", request(SiteA, notebook(SiteB, 1)), nil, 403)
	req := request(SiteA)
	req.SchemaHash = syncstore.SchemaHash()
	post(t, s, "reader-a", req, nil, 409)
	if !reflect.DeepEqual(before, state(t, db)) {
		t.Fatal("unauthorized request changed state")
	}
	post(t, s, "reader-a", request(SiteA, title(SiteA, 1, 42)), nil, 200)
	before = state(t, db)
	// Shape-invalid rows have no relay entry; their durable identity still
	// cannot be stolen by a writer row or repaired in place under the same seq.
	post(t, s, "reader-a", request(SiteA, notebook(SiteA, 1)), nil, 409)
	post(t, s, "reader-a", request(SiteA, title(SiteA, 1, "changed")), nil, 409)
	if !reflect.DeepEqual(before, state(t, db)) {
		t.Fatal("quarantine identity overwritten")
	}
}
