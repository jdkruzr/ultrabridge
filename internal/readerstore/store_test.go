package readerstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/sysop/ultrabridge/internal/readercontract"
	_ "modernc.org/sqlite"
)

const siteA = "0000000000000000000000000A"
const siteB = "0000000000000000000000000B"
const bookID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

var ctx = context.Background()

func open(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { db.Close() })
	return db
}
func exec(t *testing.T, db *sql.DB, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(sql, args...); err != nil {
		t.Fatal(err)
	}
}
func count(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func installed(t *testing.T) (*sql.DB, *Store) {
	t.Helper()
	db := open(t, filepath.Join(t.TempDir(), "library.db"))
	if err := Install(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db, New(db)
}
func raw(table, id string, seq int64, cols map[string]any) []byte {
	b, err := json.Marshal(map[string]any{"table": table, "pk": id, "site_id": siteA, "op_seq": seq, "op_ts": seq, "cols": cols})
	if err != nil {
		panic(err)
	}
	return b
}
func fixture() [][]byte {
	points := make([]byte, 40)
	for i, x := range []uint32{100, 200} {
		binary.LittleEndian.PutUint32(points[i*20:], x)
		binary.LittleEndian.PutUint32(points[i*20+4:], 200)
		binary.LittleEndian.PutUint32(points[i*20+8:], 1000)
		binary.LittleEndian.PutUint32(points[i*20+16:], x)
	}
	return [][]byte{
		raw("reader_book", bookID, 1, map[string]any{"asset_id": bookID, "byte_length": int64(9007199254740993), "media_type": "application/epub+zip", "metadata_json": `{ "version":1, "title":"Keep Me" }`}),
		raw("reader_annotation", "n", 2, map[string]any{"book_id": bookID, "initial_anchor_json": `{ "version":1,"section":0,"start":0,"end":4,"quote":"text","prefix":"","suffix":"" }`, "canvas_width": 10000, "initial_height": 1000, "creator_session_id": "creator"}),
		raw("reader_edit_session", "creator", 3, map[string]any{"annotation_id": "n", "kind": "interactive", "owner_site": siteA, "state": "open"}),
		raw("reader_stroke", "ink", 4, map[string]any{"annotation_id": "n", "session_id": "creator", "paint_order": 1, "paint_site": siteA, "color": uint32(0xff000000), "pen_width_min": 10, "pen_width_max": 20, "brush_kind": "ballpoint", "brush_version": 1, "brush_seed": 7, "points": base64.StdEncoding.EncodeToString(points), "point_dynamics": nil}),
		raw("reader_edit_session", "creator", 5, map[string]any{"annotation_id": "n", "kind": "interactive", "owner_site": siteA, "state": "finished"}),
	}
}
func prepare(t *testing.T, ops ...[]byte) *Prepared {
	t.Helper()
	p, err := Prepare(siteA, ops)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func stage(t *testing.T, s *Store, ops ...[]byte) {
	t.Helper()
	if err := s.Stage(ctx, prepare(t, ops...)); err != nil {
		t.Fatal(err)
	}
}
func drain(t *testing.T, s *Store) {
	t.Helper()
	for sweep := 0; sweep < 8; sweep++ {
		var after int64
		for {
			page, err := s.Drain(ctx, after, 2)
			if err != nil {
				t.Fatal(err)
			}
			after = page.Next
			if after == 0 {
				break
			}
		}
	}
}

func TestRestartDependenciesAndTerminalBeforeOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reader.db")
	db := open(t, path)
	if err := Install(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := New(db)
	ops := fixture()
	stage(t, s, ops[3], ops[4])
	drain(t, s)
	if count(t, db, "fn_reader_stroke") != 0 {
		t.Fatal("orphan stroke applied")
	}
	db.Close()
	db = open(t, path)
	s = New(db)
	stage(t, s, ops[3], ops[4])
	stage(t, s, ops[2], ops[1], ops[0])
	drain(t, s)
	if count(t, db, "reader_store_incoming") != 5 || count(t, db, "fn_reader_stroke") != 1 {
		t.Fatal("lost/duplicated operations")
	}
	p, err := s.Projection(ctx, "n", DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != readercontract.Ready || p.InputHash == nil || *p.InputHash != "0460b7bc690c89dcdfd18560434dd62321358ad81d63c9b997336302c6c5efe4" {
		t.Fatalf("bad projection %+v", p)
	}
	var state string
	var seq int64
	db.QueryRow("SELECT state,lww_op_seq FROM fn_reader_edit_session WHERE id='creator'").Scan(&state, &seq)
	if state != "finished" || seq != 5 {
		t.Fatal("terminal state reopened/restamped")
	}
	var bytes int64
	db.QueryRow("SELECT byte_length FROM fn_reader_book").Scan(&bytes)
	if bytes != 9007199254740993 {
		t.Fatal("int64 rounded")
	}
	stage(t, s, raw("reader_annotation_lifecycle", "n", 6, map[string]any{"deleted": 1, "changed_at": 6}))
	drain(t, s)
	p, err = s.Projection(ctx, "n", DefaultLimits())
	if err != nil || p.Status != readercontract.Deleted {
		t.Fatal("delete failed")
	}
	stage(t, s, raw("reader_annotation_lifecycle", "n", 7, map[string]any{"deleted": 0, "changed_at": 7}))
	drain(t, s)
	p, err = s.Projection(ctx, "n", DefaultLimits())
	if err != nil || p.Status != readercontract.Ready || len(p.Strokes) != 1 {
		t.Fatal("restore lost ink")
	}
}

func TestAtomicReceiptAndIdentityReuse(t *testing.T) {
	db, s := installed(t)
	exec(t, db, "CREATE TABLE host_ack(value INTEGER)")
	tx, err := writer(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	p := prepare(t, fixture()[0])
	if err = p.CommitTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
	tx.Exec("INSERT INTO host_ack VALUES(1)")
	tx.Rollback()
	if count(t, db, "reader_store_incoming") != 0 || count(t, db, "host_ack") != 0 {
		t.Fatal("host rollback leaked receipt")
	}
	stage(t, s, fixture()[0])
	changed := raw("reader_book_title", bookID, 1, map[string]any{"title": "reuse"})
	if err = s.Stage(ctx, prepare(t, fixture()[1], changed)); err == nil {
		t.Fatal("operation identity reused")
	}
	if count(t, db, "reader_store_incoming") != 1 {
		t.Fatal("partial receipt committed")
	}
	if _, err = Prepare(siteB, fixture()); err == nil {
		t.Fatal("wrong verified site accepted")
	}
	if _, err = Prepare("", nil); err == nil {
		t.Fatal("missing verified identity accepted")
	}
}

func TestApplyFailureRollsBackMirrorAndInboxTogether(t *testing.T) {
	db, s := installed(t)
	stage(t, s, fixture()...)
	exec(t, db, "CREATE TRIGGER fail_status BEFORE UPDATE ON reader_store_incoming BEGIN SELECT RAISE(ABORT,'injected'); END")
	result, err := s.Drain(ctx, 0, 64)
	if err == nil || len(result.Changed) != 0 {
		t.Fatal("failure leaked success")
	}
	if count(t, db, "fn_reader_book") != 0 {
		t.Fatal("mirror escaped rollback")
	}
	var pending int
	db.QueryRow("SELECT count(*) FROM reader_store_incoming WHERE state='pending'").Scan(&pending)
	if pending != 5 {
		t.Fatal("inbox escaped rollback")
	}
	exec(t, db, "DROP TRIGGER fail_status")
	drain(t, s)
	if count(t, db, "fn_reader_stroke") != 1 {
		t.Fatal("retry failed")
	}
}

func TestMalformedAndConflictingRowsStayQuarantined(t *testing.T) {
	db, s := installed(t)
	stage(t, s, fixture()...)
	drain(t, s)
	stage(t, s, raw("reader_book_title", bookID, 6, map[string]any{"title": 42}), raw("reader_annotation", "n", 7, map[string]any{"book_id": bookID, "initial_anchor_json": `{"version":2}`, "canvas_width": 500, "initial_height": 20, "creator_session_id": "other"}))
	drain(t, s)
	var n int
	db.QueryRow("SELECT count(*) FROM reader_store_incoming WHERE state='quarantined'").Scan(&n)
	if n != 2 || count(t, db, "fn_reader_book_title") != 0 {
		t.Fatal("invalid rows applied or disappeared")
	}
	p, err := s.Projection(ctx, "n", DefaultLimits())
	if err != nil || p.CanvasWidth != 10000 {
		t.Fatal("immutable annotation overwritten")
	}
}

func TestSnapshotBudgetsAndPreparedPayloadOwnership(t *testing.T) {
	db, s := installed(t)
	ops := fixture()
	p := prepare(t, ops...)
	for _, b := range ops {
		for i := range b {
			b[i] = 'x'
		}
	}
	if err := s.Stage(ctx, p); err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	for _, limits := range []Limits{{1, MaxBatchBytes}, {4096, 1}, {0, 1}, {16385, MaxBatchBytes}} {
		snapshot, err := s.Snapshot(ctx, "n", limits)
		if !errors.Is(err, ErrBudget) || snapshot != nil {
			t.Fatal("partial/oversized snapshot returned")
		}
	}
	if r, err := s.Snapshot(ctx, "missing", DefaultLimits()); err != nil || r != nil {
		t.Fatal("missing snapshot")
	}
	// A scalar length probe must refuse a corrupted oversized cell before reading it.
	exec(t, db, "UPDATE fn_reader_stroke SET points=zeroblob(?)", MaxRowBytes+1)
	if r, err := s.Snapshot(ctx, "n", DefaultLimits()); !errors.Is(err, ErrBudget) || r != nil {
		t.Fatal("oversized row materialized")
	}
}

func TestConcurrentDuplicateReceiptAndDrain(t *testing.T) {
	db, s := installed(t)
	p := prepare(t, fixture()...)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Stage(ctx, p); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				if _, err := s.Drain(ctx, 0, 64); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if count(t, db, "reader_store_incoming") != 5 || count(t, db, "fn_reader_stroke") != 1 {
		t.Fatal("concurrent replay duplicated rows")
	}
}

func TestReceiptLimitsAndQuarantinePreservesInvalidBytes(t *testing.T) {
	db, s := installed(t)
	if _, err := Prepare(siteA, make([][]byte, 501)); !errors.Is(err, ErrBudget) {
		t.Fatal("batch count unbounded")
	}
	if _, err := Prepare(siteA, [][]byte{make([]byte, MaxRowBytes+1)}); !errors.Is(err, ErrBudget) {
		t.Fatal("row bytes unbounded")
	}
	raw := []byte(`{"table":"reader_book_title","pk":"book","site_id":"0000000000000000000000000A","op_seq":1,"op_ts":1,"cols":{"title":"\ud800"}}`)
	stage(t, s, raw)
	var payload string
	if err := db.QueryRow("SELECT payload FROM reader_store_incoming").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal([]byte(payload), raw) {
		t.Fatal("invalid text was silently repaired")
	}
	exec(t, db, "DROP INDEX reader_store_incoming_identity")
	exec(t, db, "CREATE INDEX reader_store_incoming_identity ON reader_store_incoming(site_id)")
	if Install(ctx, db) == nil {
		t.Fatal("wrong identity index silently accepted")
	}
}

func TestMissingBookStaysPendingWithoutInventingAssetReadiness(t *testing.T) {
	db, s := installed(t)
	ops := fixture()
	stage(t, s, ops[1:]...)
	drain(t, s)
	p, err := s.Projection(ctx, "n", DefaultLimits())
	if err != nil || p.Status != readercontract.Pending || p.InputHash != nil {
		t.Fatalf("missing book claimed ready: %+v %v", p, err)
	}
	stage(t, s, ops[0])
	drain(t, s)
	p, err = s.Projection(ctx, "n", DefaultLimits())
	if err != nil || p.Status != readercontract.Ready {
		t.Fatal("metadata arrival did not resolve pending")
	}
	var n int
	db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='rhizome_asset_descriptor'").Scan(&n)
	if n != 0 {
		t.Fatal("reader snapshot created fake assets")
	}
}

func TestKotlinRowsThroughSQLite(t *testing.T) {
	path := os.Getenv("FORESTREAD_CONTRACT_VECTORS")
	if path == "" {
		t.Skip("use ForestNote full runner for current Kotlin storage vectors")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Wire []struct {
			Name  string
			Op    json.RawMessage
			Valid bool
		}
	}
	if err = json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	inputs := [][]byte{}
	expected := map[Key]readercontract.Record{}
	for _, v := range vectors.Wire {
		op, r, err := readercontract.DecodeJSON(v.Op)
		if v.Name != op.Table {
			continue
		}
		if err != nil || !v.Valid {
			t.Fatal("invalid base vector")
		}
		inputs = append(inputs, v.Op)
		expected[Key{op.Table, op.PK}] = r
	}
	if len(expected) != 15 {
		t.Fatal("missing table coverage")
	}
	for seed := int64(0); seed < 4; seed++ {
		db, s := installed(t)
		order := append([][]byte{}, inputs...)
		rand.New(rand.NewSource(seed)).Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		stage(t, s, order...)
		drain(t, s)
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		b := budget{1024, MaxBatchBytes}
		for key, want := range expected {
			got, err := load(ctx, tx, key.Table, key.ID, &b)
			if err != nil || got == nil || !reflect.DeepEqual(*got, want) {
				tx.Rollback()
				t.Fatalf("stored Kotlin row %s differs: %v", key.Table, err)
			}
		}
		tx.Rollback()
	}
}
