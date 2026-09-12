package readersearch

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/readercontract"
	"github.com/sysop/ultrabridge/internal/readerstore"
	"github.com/sysop/ultrabridge/internal/syncstore"
	_ "modernc.org/sqlite"
)

var ctx = context.Background()

const site = "0000000000000000000000000A"
const siteB = "0000000000000000000000000B"
const book = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const anchor = `{ "version":1,"section":0,"start":0,"end":7,"quote":"passage","prefix":"","suffix":"" }`

type fixture struct {
	db     *sql.DB
	search *Store
	reader *readerstore.Store
	seq    int64
}

func open(t *testing.T, path string) *fixture {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, install := range []func(context.Context, *sql.DB) error{syncstore.Migrate, readerstore.Install, Install} {
		if err := install(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	f := &fixture{db: db, search: New(db), reader: readerstore.New(db)}
	if err := db.QueryRow("SELECT COALESCE(MAX(op_seq),0) FROM reader_store_incoming").Scan(&f.seq); err != nil {
		t.Fatal(err)
	}
	return f
}
func setup(t *testing.T) *fixture { return open(t, filepath.Join(t.TempDir(), "reader.db")) }
func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}
func scalar(t *testing.T, db *sql.DB, q string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func (f *fixture) put(t *testing.T, table, id string, cols map[string]any) {
	f.author(t, site, table, id, cols)
}
func (f *fixture) author(t *testing.T, author, table, id string, cols map[string]any) {
	t.Helper()
	f.seq++
	raw, err := json.Marshal(map[string]any{"table": table, "pk": id, "site_id": author, "op_seq": f.seq, "op_ts": f.seq, "cols": cols})
	if err != nil {
		t.Fatal(err)
	}
	p, err := readerstore.Prepare(author, [][]byte{raw})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.reader.Stage(ctx, p); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err = f.reader.Drain(ctx, 0, 128); err != nil {
			t.Fatal(err)
		}
	}
}
func (f *fixture) annotation(t *testing.T, id string) {
	f.put(t, "reader_book", book, map[string]any{"asset_id": book, "byte_length": 100, "media_type": "application/epub+zip", "metadata_json": `{"version":1,"title":"No Pancakes"}`})
	f.put(t, "reader_annotation", id, map[string]any{"book_id": book, "initial_anchor_json": anchor, "canvas_width": 10000, "initial_height": 1000, "creator_session_id": "session-" + id})
	f.put(t, "reader_edit_session", "session-"+id, map[string]any{"annotation_id": id, "kind": "interactive", "owner_site": site, "state": "open"})
	points := make([]byte, 40)
	for i := 0; i < 2; i++ {
		binary.LittleEndian.PutUint32(points[i*20:], 100+uint32(i)*100)
		binary.LittleEndian.PutUint32(points[i*20+4:], 200)
		binary.LittleEndian.PutUint32(points[i*20+8:], 1000)
	}
	f.put(t, "reader_stroke", "ink-"+id, map[string]any{"annotation_id": id, "session_id": "session-" + id, "paint_order": 1, "paint_site": site, "color": uint32(0xff000000), "pen_width_min": 10, "pen_width_max": 20, "brush_kind": "ballpoint", "brush_version": 1, "brush_seed": 7, "points": points, "point_dynamics": nil})
}
func (f *fixture) hash(t *testing.T, id string) string {
	t.Helper()
	p, err := f.reader.Projection(ctx, id, readerstore.DefaultLimits())
	if err != nil || p == nil || p.InputHash == nil {
		t.Fatalf("projection: %+v %v", p, err)
	}
	return *p.InputHash
}
func (f *fixture) recognition(t *testing.T, author, id, hash, text, status string) {
	key, _ := readercontract.CompositeID(id, "client:"+author)
	f.author(t, author, "reader_recognition", key, map[string]any{"annotation_id": id, "producer_id": "client:" + author, "input_hash": hash, "engine": "fixture", "model": "English", "language": "en", "status": status, "text": text})
}
func (f *fixture) schedule(t *testing.T) {
	t.Helper()
	for {
		changes, err := f.reader.Changes(ctx, 128)
		if err != nil {
			t.Fatal(err)
		}
		if len(changes) == 0 {
			return
		}
		for _, c := range changes {
			if err = f.search.Schedule(ctx, c); err != nil {
				t.Fatal(err)
			}
			if err = f.reader.CompleteChange(ctx, c.Seq); err != nil {
				t.Fatal(err)
			}
		}
	}
}
func (f *fixture) drain(t *testing.T) {
	t.Helper()
	f.schedule(t)
	for i := 0; i < 500; i++ {
		more, err := f.search.Step(ctx, 2)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			return
		}
	}
	t.Fatal("search jobs did not finish")
}
func (f *fixture) hits(t *testing.T, q string) []Result {
	t.Helper()
	results, err := f.search.Search(ctx, q, "", 25)
	if err != nil {
		t.Fatal(err)
	}
	return results
}

func TestCurrentRecognitionAlternativesAndStaleSuppression(t *testing.T) {
	f := setup(t)
	f.annotation(t, "n")
	hash := f.hash(t, "n")
	f.recognition(t, site, "n", hash, "electric marmalade", "ready")
	f.recognition(t, siteB, "n", hash, "alternative marmalade", "ready")
	f.drain(t)
	r := f.hits(t, "marmalade")
	if len(r) != 1 || len(r[0].Alternatives) != 2 || r[0].Alternatives[0].Producer != "client:"+siteB || r[0].Anchor != anchor {
		t.Fatalf("lost alternatives/anchor: %+v", r)
	}
	if len(f.hits(t, "passage")) != 1 || len(f.hits(t, "Pancakes")) != 1 {
		t.Fatal("quote/title not indexed")
	}
	f.recognition(t, site, "n", strings.Repeat("b", 64), "stale spectrometer", "ready")
	if len(f.hits(t, "marmalade")) != 0 {
		t.Fatal("dirty cached text leaked before scheduling")
	}
	f.drain(t)
	if len(f.hits(t, "electric")) != 0 || len(f.hits(t, "spectrometer")) != 0 || len(f.hits(t, "alternative")) != 1 {
		t.Fatal("stale recognition indexed")
	}
	f.recognition(t, siteB, "n", hash, "failed recognition", "failed")
	f.drain(t)
	if len(f.hits(t, "alternative")) != 0 || len(f.hits(t, "failed")) != 0 {
		t.Fatal("failed recognition indexed")
	}
	if scalar(t, f.db, "SELECT count(*) FROM sync_ops") != 0 || scalar(t, f.db, "SELECT count(*) FROM fn_reader_recognition") != 2 {
		t.Fatal("search re-authored or synthesized OCR")
	}
}

func TestLifecycleCancelAndBookFanoutResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.db")
	f := open(t, path)
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		f.annotation(t, id)
		f.recognition(t, site, id, f.hash(t, id), "searchable honey", "ready")
	}
	f.drain(t)
	f.put(t, "reader_book_title", book, map[string]any{"title": "Renamed Orchard"})
	f.schedule(t)
	if len(f.hits(t, "honey")) != 0 {
		t.Fatal("book fan-out exposed stale index")
	}
	if more, err := f.search.Step(ctx, 2); err != nil || !more {
		t.Fatal(err)
	}
	if scalar(t, f.db, "SELECT count(*) FROM reader_search_documents WHERE title='Renamed Orchard'") != 2 {
		t.Fatal("unbounded fan-out page")
	}
	f.db.Close()
	f = open(t, path)
	f.drain(t)
	if len(f.hits(t, "Orchard")) != 5 {
		t.Fatal("restart lost fan-out progress")
	}
	f.put(t, "reader_book_lifecycle", book, map[string]any{"deleted": 1, "changed_at": 1})
	if len(f.hits(t, "honey")) != 0 {
		t.Fatal("deletion exposed before job processing")
	}
	f.drain(t)
	if scalar(t, f.db, "SELECT count(*) FROM reader_search_documents") != 0 {
		t.Fatal("deleted book indexed")
	}
	f.put(t, "reader_book_lifecycle", book, map[string]any{"deleted": 0, "changed_at": 2})
	f.drain(t)
	if len(f.hits(t, "honey")) != 5 {
		t.Fatal("restore did not rebuild")
	}
	f.put(t, "reader_edit_session", "session-a", map[string]any{"annotation_id": "a", "kind": "interactive", "owner_site": site, "state": "cancelled"})
	f.drain(t)
	if len(f.hits(t, "honey")) != 4 {
		t.Fatal("canceled annotation still searchable")
	}
}

func TestJobAndIndexCheckpointRollbackRetry(t *testing.T) {
	f := setup(t)
	f.annotation(t, "n")
	exec(t, f.db, "CREATE TRIGGER fail BEFORE INSERT ON reader_search_jobs BEGIN SELECT RAISE(ABORT,'full'); END")
	changes, err := f.reader.Changes(ctx, 128)
	if err != nil {
		t.Fatal(err)
	}
	if f.search.Schedule(ctx, changes[0]) == nil {
		t.Fatal("queue failure acknowledged")
	}
	if scalar(t, f.db, "SELECT seq FROM reader_store_change_cursor") != 0 {
		t.Fatal("handoff failure advanced journal")
	}
	exec(t, f.db, "DROP TRIGGER fail")
	f.drain(t)
	f.put(t, "reader_book_title", book, map[string]any{"title": "Recovered Title"})
	f.schedule(t)
	exec(t, f.db, "CREATE TRIGGER fail BEFORE UPDATE OF after_id ON reader_search_jobs BEGIN SELECT RAISE(ABORT,'full'); END")
	if _, err = f.search.Step(ctx, 2); err == nil {
		t.Fatal("index checkpoint failure accepted")
	}
	if scalar(t, f.db, "SELECT count(*) FROM reader_search_documents WHERE title='Recovered Title'") != 0 {
		t.Fatal("index committed without job cursor")
	}
	exec(t, f.db, "DROP TRIGGER fail")
	exec(t, f.db, "UPDATE reader_search_jobs SET retry_at=0")
	f.drain(t)
	if len(f.hits(t, "Recovered")) != 1 {
		t.Fatal("failed page did not retry")
	}
	if err = f.search.Schedule(ctx, changes[0]); err != nil {
		t.Fatal(err)
	}
	if scalar(t, f.db, "SELECT count(*) FROM reader_search_jobs WHERE seq=1") != 1 {
		t.Fatal("replay duplicated job")
	}
}

func TestSnapshotPublicationRaceAndIndependentJobs(t *testing.T) {
	f := setup(t)
	f.annotation(t, "n")
	f.drain(t)
	input, err := f.reader.SearchSnapshot(ctx, "n", readerstore.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	old, err := project(input)
	if err != nil {
		t.Fatal(err)
	}
	f.put(t, "reader_annotation_lifecycle", "n", map[string]any{"deleted": 1, "changed_at": 1})
	if err = f.search.publish(ctx, 1, "n", input.Revision, old); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale projection published: %v", err)
	}
	f.drain(t)
	if len(f.hits(t, "passage")) != 0 {
		t.Fatal("stale result survived deletion")
	}
	// A deliberately over-budget recognition keeps its job retryable and fails closed;
	// unrelated ready jobs still get their turn rather than being held behind it.
	f.put(t, "reader_annotation_lifecycle", "n", map[string]any{"deleted": 0, "changed_at": 2})
	f.drain(t)
	f.recognition(t, site, "n", f.hash(t, "n"), strings.Repeat("x", 300<<10), "ready")
	f.schedule(t)
	if _, err = f.search.Step(ctx, 1); !errors.Is(err, readerstore.ErrBudget) {
		t.Fatalf("oversize: %v", err)
	}
	if len(f.hits(t, "passage")) != 0 {
		t.Fatal("oversized failed job exposed stale cache")
	}
	other := strings.Repeat("b", 64)
	f.put(t, "reader_book", other, map[string]any{"asset_id": other, "byte_length": 100, "media_type": "application/epub+zip", "metadata_json": `{"version":1,"title":"Healthy"}`})
	f.put(t, "reader_annotation", "healthy", map[string]any{"book_id": other, "initial_anchor_json": anchor, "canvas_width": 10000, "initial_height": 1000, "creator_session_id": "healthy-session"})
	f.put(t, "reader_edit_session", "healthy-session", map[string]any{"annotation_id": "healthy", "kind": "interactive", "owner_site": site, "state": "finished"})
	f.schedule(t)
	for i := 0; i < 10 && len(f.hits(t, "Healthy")) == 0; i++ {
		if _, err := f.search.Step(ctx, 2); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.hits(t, "Healthy")) != 1 {
		t.Fatal("poison job starved another book")
	}
	if scalar(t, f.db, "SELECT count(*) FROM reader_search_jobs WHERE done=0 AND error_code='projection_failed'") == 0 {
		t.Fatal("failure not inspectable")
	}
}

func TestSearchBoundsAndRecoveryBootstrap(t *testing.T) {
	f := setup(t)
	f.annotation(t, "n")
	f.drain(t)
	for _, q := range []string{"", `" OR *`, "NEAR(foo)", "notfound"} {
		if _, err := f.search.Search(ctx, q, "", 25); err != nil {
			t.Fatalf("literal FTS query %q: %v", q, err)
		}
	}
	for _, limit := range []int{0, 101} {
		if _, err := f.search.Search(ctx, "passage", "", limit); err == nil {
			t.Fatal("unbounded results")
		}
	}
	if _, err := f.search.Search(ctx, strings.Repeat("a", 257), "", 25); err == nil {
		t.Fatal("unbounded query")
	}
	// Explicit fixture reconstruction: only disposable derived search tables.
	for _, table := range []string{"reader_search_fts", "reader_search_documents", "reader_search_jobs", "reader_search_version"} {
		exec(t, f.db, "DROP TABLE "+table)
	}
	if err := Install(ctx, f.db); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	if len(f.hits(t, "passage")) != 1 {
		t.Fatal("already-acked journal was not bootstrapped")
	}
	exec(t, f.db, "UPDATE reader_search_version SET version=99")
	if Install(ctx, f.db) == nil {
		t.Fatal("future search schema accepted")
	}
}

func TestContributionInvalidationEraseCancelAndAnchor(t *testing.T) {
	f := setup(t)
	f.annotation(t, "n")
	original := f.hash(t, "n")
	f.recognition(t, site, "n", original, "enduring notebook", "ready")
	f.drain(t)
	f.put(t, "reader_edit_session", "eraser", map[string]any{"annotation_id": "n", "kind": "interactive", "owner_site": site, "state": "open"})
	claim, _ := readercontract.CompositeID("eraser", "ink-n")
	f.put(t, "reader_erase_claim", claim, map[string]any{"session_id": "eraser", "stroke_id": "ink-n", "active": 1})
	if len(f.hits(t, "enduring")) != 0 {
		t.Fatal("erase did not invalidate cached recognition")
	}
	f.drain(t)
	if len(f.hits(t, "enduring")) != 0 {
		t.Fatal("erased ink retained old recognition")
	}
	f.put(t, "reader_edit_session", "eraser", map[string]any{"annotation_id": "n", "kind": "interactive", "owner_site": site, "state": "cancelled"})
	f.drain(t)
	if len(f.hits(t, "enduring")) != 1 {
		t.Fatal("cancel erase did not recover matching recognition")
	}
	key, _ := readercontract.CompositeID("session-n", "height")
	f.put(t, "reader_annotation_value", key, map[string]any{"session_id": "session-n", "property": "height", "value_json": `{"version":1,"height":2000}`})
	f.drain(t)
	if len(f.hits(t, "enduring")) != 0 {
		t.Fatal("height change did not invalidate fingerprint")
	}
	key, _ = readercontract.CompositeID("session-n", "anchor")
	newAnchor := strings.Replace(anchor, "passage", "boundary", 1)
	f.put(t, "reader_annotation_value", key, map[string]any{"session_id": "session-n", "property": "anchor", "value_json": newAnchor})
	if len(f.hits(t, "passage")) != 0 {
		t.Fatal("anchor edit leaked cached quote")
	}
	f.drain(t)
	if len(f.hits(t, "boundary")) != 1 {
		t.Fatal("anchor edit not indexed")
	}
	f.put(t, "reader_annotation_value", key, map[string]any{"session_id": "session-n", "property": "anchor", "value_json": `{"version":2,"future":true}`})
	f.drain(t)
	if len(f.hits(t, "boundary")) != 0 {
		t.Fatal("unsupported projection remained searchable")
	}
}

func TestRunCancellationAndDuplicateOwner(t *testing.T) {
	f := setup(t)
	f.annotation(t, "n")
	f.schedule(t)
	c, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- f.search.Run(c, nil) }()
	deadline := time.Now().Add(3 * time.Second)
	for !f.search.running.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := f.search.Run(ctx, nil); err == nil {
		t.Fatal("duplicate owner started")
	}
	for len(f.hits(t, "passage")) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("index worker did not stop")
	}
	if len(f.hits(t, "passage")) != 1 {
		t.Fatal("background job was not indexed")
	}
}
