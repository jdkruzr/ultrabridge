package syncbridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image/jpeg"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sysop/ultrabridge/internal/syncstore"
)

const (
	siteA = "0000000000000000000000000A"
	nb1   = "00000000000000000000000NB1"
	pg1   = "00000000000000000000000PG1"
	st1   = "00000000000000000000000ST1"
)

func newStore(t *testing.T) *syncstore.Store {
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
	return syncstore.New(db)
}

func twoPointBlob() string {
	b := make([]byte, 0, 40)
	for _, p := range [][3]int32{{10, 10, 2000}, {60, 60, 2000}} {
		var buf [20]byte
		binary.LittleEndian.PutUint32(buf[0:4], uint32(p[0]))
		binary.LittleEndian.PutUint32(buf[4:8], uint32(p[1]))
		binary.LittleEndian.PutUint32(buf[8:12], uint32(p[2]))
		b = append(b, buf[:]...)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// seedPageWithStroke applies notebook+page+stroke ops so the mirror has a live page.
func seedPageWithStroke(t *testing.T, s *syncstore.Store) {
	t.Helper()
	ops := []syncstore.Op{
		{Table: "notebook", PK: nb1, SiteID: siteA, OpSeq: 1, WallTS: 1000,
			Cols: map[string]any{"name": "NB", "sort_order": float64(0), "created_at": float64(1000), "deleted_at": nil, "folder_id": nil}},
		{Table: "page", PK: pg1, SiteID: siteA, OpSeq: 2, WallTS: 1010,
			Cols: map[string]any{"notebook_id": nb1, "sort_order": float64(0), "created_at": float64(1010), "deleted_at": nil, "template": nil, "template_pitch_mm": nil}},
		{Table: "stroke", PK: st1, SiteID: siteA, OpSeq: 3, WallTS: 1020,
			Cols: map[string]any{"page_id": pg1, "color": float64(4278190080), "pen_width_min": float64(2),
				"pen_width_max": float64(6), "points": twoPointBlob(), "z": float64(0), "created_at": float64(1020), "deleted_at": nil}},
	}
	if _, err := s.ApplyBatch(context.Background(), siteA, ops); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

type indexCall struct{ path, source, body string }

type fakeIndexer struct {
	mu      sync.Mutex
	calls   []indexCall
	deleted []string
	notify  chan string // optional: receives path on each call
}

func (f *fakeIndexer) IndexPage(_ context.Context, path string, _ int, source, body, _, _ string) error {
	f.mu.Lock()
	f.calls = append(f.calls, indexCall{path, source, body})
	f.mu.Unlock()
	if f.notify != nil {
		f.notify <- path
	}
	return nil
}
func (f *fakeIndexer) Delete(_ context.Context, path string) error {
	f.mu.Lock()
	f.deleted = append(f.deleted, path)
	f.mu.Unlock()
	return nil
}
func (f *fakeIndexer) count() int    { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }
func (f *fakeIndexer) delCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.deleted) }

type fakeOCR struct{ text string }

func (f fakeOCR) Recognize(context.Context, []byte, string) (string, error) { return f.text, nil }

// recordingOCR captures every JPEG passed to Recognize so a test can assert what
// pixels the OCR pipeline actually saw (used by TestProcessPage_OCRJPEGOmitsTextBoxes).
type recordingOCR struct {
	mu    sync.Mutex
	text  string
	jpegs [][]byte
}

func (r *recordingOCR) Recognize(_ context.Context, j []byte, _ string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jpegs = append(r.jpegs, append([]byte(nil), j...))
	return r.text, nil
}

type fakeEmbedder struct{}

func (fakeEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{0.1, 0.2}, nil
}

type fakeEmbedStore struct {
	mu      sync.Mutex
	saved   []string // paths
	deleted []string // paths
}

func (f *fakeEmbedStore) Save(_ context.Context, path string, _ int, _ int, _ []float32, _ string) error {
	f.mu.Lock()
	f.saved = append(f.saved, path)
	f.mu.Unlock()
	return nil
}
func (f *fakeEmbedStore) DeletePage(_ context.Context, _ string, _ int) error { return nil }
func (f *fakeEmbedStore) Delete(_ context.Context, path string) error {
	f.mu.Lock()
	f.deleted = append(f.deleted, path)
	f.mu.Unlock()
	return nil
}

func TestProcessPage_RendersOCRsIndexesEmbeds(t *testing.T) {
	s := newStore(t)
	seedPageWithStroke(t, s)
	fi := &fakeIndexer{}
	fe := &fakeEmbedStore{}
	b := New(s, Deps{Indexer: fi, OCR: fakeOCR{text: "hello world"}, Embedder: fakeEmbedder{}, EmbedStore: fe, EmbedModel: "m"}, nil)

	b.processPage(context.Background(), pg1)

	if fi.count() != 1 {
		t.Fatalf("want 1 index call, got %d", fi.count())
	}
	want := "forestnote://" + nb1 + "/" + pg1
	if fi.calls[0].path != want {
		t.Errorf("index path = %q, want %q", fi.calls[0].path, want)
	}
	if fi.calls[0].source != "forestnote" || fi.calls[0].body != "hello world" {
		t.Errorf("index call = %+v", fi.calls[0])
	}
	if len(fe.saved) != 1 || fe.saved[0] != want {
		t.Errorf("embed saved = %v, want [%s]", fe.saved, want)
	}
}

func TestProcessPage_DeletedPageSkipped(t *testing.T) {
	s := newStore(t)
	seedPageWithStroke(t, s)
	// delete the page (higher op_seq/wall_ts)
	_, err := s.ApplyBatch(context.Background(), siteA, []syncstore.Op{
		{Table: "page", PK: pg1, SiteID: siteA, OpSeq: 4, WallTS: 2000,
			Cols: map[string]any{"notebook_id": nb1, "sort_order": float64(0), "created_at": float64(1010), "deleted_at": float64(2000), "template": nil, "template_pitch_mm": nil}},
	})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	fi := &fakeIndexer{}
	fe := &fakeEmbedStore{}
	b := New(s, Deps{Indexer: fi, OCR: fakeOCR{text: "x"}, Embedder: fakeEmbedder{}, EmbedStore: fe}, nil)
	b.processPage(context.Background(), pg1)
	if fi.count() != 0 {
		t.Errorf("deleted page should not be indexed, got %d calls", fi.count())
	}
	want := "forestnote://" + nb1 + "/" + pg1
	if fi.delCount() != 1 || fi.deleted[0] != want {
		t.Errorf("deleted page should drop its index entry %q, got %v", want, fi.deleted)
	}
	if len(fe.deleted) != 1 || fe.deleted[0] != want {
		t.Errorf("deleted page should drop its embedding %q, got %v", want, fe.deleted)
	}
}

func TestProcessPage_NoStrokesSkipped(t *testing.T) {
	s := newStore(t)
	// page but no strokes
	_, err := s.ApplyBatch(context.Background(), siteA, []syncstore.Op{
		{Table: "page", PK: pg1, SiteID: siteA, OpSeq: 1, WallTS: 1000,
			Cols: map[string]any{"notebook_id": nb1, "sort_order": float64(0), "created_at": float64(1000), "deleted_at": nil, "template": nil, "template_pitch_mm": nil}},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	fi := &fakeIndexer{}
	b := New(s, Deps{Indexer: fi, OCR: fakeOCR{text: "x"}}, nil)
	b.processPage(context.Background(), pg1)
	if fi.count() != 0 {
		t.Errorf("strokeless page should not be indexed, got %d calls", fi.count())
	}
}

// seedPageWithTextBox adds a live page (no strokes) plus one text box carrying
// `text`, so a text-only page can be exercised.
func seedPageWithTextBox(t *testing.T, s *syncstore.Store, text string) {
	t.Helper()
	const tb1 = "00000000000000000000000TB1"
	_, err := s.ApplyBatch(context.Background(), siteA, []syncstore.Op{
		{Table: "page", PK: pg1, SiteID: siteA, OpSeq: 1, WallTS: 1000,
			Cols: map[string]any{"notebook_id": nb1, "sort_order": float64(0), "created_at": float64(1000), "deleted_at": nil, "template": nil, "template_pitch_mm": nil}},
		{Table: "text_box", PK: tb1, SiteID: siteA, OpSeq: 2, WallTS: 1010,
			Cols: map[string]any{
				"page_id": pg1, "x": float64(0), "y": float64(0), "width": float64(1000), "height": float64(500),
				"text": text, "font_name": "", "font_size": float64(200), "color": float64(4278190080),
				"weight": float64(400), "border_width": float64(0), "z": float64(0),
				"created_at": float64(1000), "deleted_at": nil,
			}},
	})
	if err != nil {
		t.Fatalf("seed text box: %v", err)
	}
}

// seedTextBoxOnExistingPage adds a text box to a page already seeded (e.g. by
// seedPageWithStroke, which uses op_seq 1..3 — so this uses 10).
func seedTextBoxOnExistingPage(t *testing.T, s *syncstore.Store, text string) {
	t.Helper()
	const tb1 = "00000000000000000000000TB1"
	_, err := s.ApplyBatch(context.Background(), siteA, []syncstore.Op{
		{Table: "text_box", PK: tb1, SiteID: siteA, OpSeq: 10, WallTS: 1100,
			Cols: map[string]any{
				"page_id": pg1, "x": float64(0), "y": float64(0), "width": float64(1000), "height": float64(500),
				"text": text, "font_name": "", "font_size": float64(200), "color": float64(4278190080),
				"weight": float64(400), "border_width": float64(0), "z": float64(0),
				"created_at": float64(1000), "deleted_at": nil,
			}},
	})
	if err != nil {
		t.Fatalf("seed text box: %v", err)
	}
}

func seedClientPageText(t *testing.T, s *syncstore.Store, text string) {
	t.Helper()
	_, err := s.ApplyBatch(context.Background(), siteA, []syncstore.Op{
		{Table: "page_text_from_client", PK: pg1, SiteID: siteA, OpSeq: 20, WallTS: 1200,
			Cols: map[string]any{
				"text": text, "ocr_at": float64(1200), "model": "mlkit-digital-ink:en-US",
				"created_at": float64(1200), "deleted_at": nil,
			}},
	})
	if err != nil {
		t.Fatalf("seed client page text: %v", err)
	}
}

// seedDistantTextBox seeds a text box positioned far from seedPageWithStroke's
// strokes (~10..60) so that drawing it into the rendered JPEG would expand the
// bounding box dramatically. Used by TestProcessPage_OCRJPEGOmitsTextBoxes —
// distance-from-strokes is what makes the test detectable.
func seedDistantTextBox(t *testing.T, s *syncstore.Store, text string) {
	t.Helper()
	const tb1 = "00000000000000000000000TB1"
	_, err := s.ApplyBatch(context.Background(), siteA, []syncstore.Op{
		{Table: "text_box", PK: tb1, SiteID: siteA, OpSeq: 11, WallTS: 1110,
			Cols: map[string]any{
				"page_id": pg1, "x": float64(5000), "y": float64(5000), "width": float64(1000), "height": float64(500),
				"text": text, "font_name": "", "font_size": float64(200), "color": float64(4278190080),
				"weight": float64(400), "border_width": float64(0), "z": float64(0),
				"created_at": float64(1000), "deleted_at": nil,
			}},
	})
	if err != nil {
		t.Fatalf("seed distant text box: %v", err)
	}
}

// Regression: the OCR-bound JPEG must NOT include text boxes. If it did, the
// recognizer would transcribe their glyphs AND joinTextBoxes would append the
// text again, stacking each box's content 2-3× in the body the dialog renders
// verbatim to the user. (See ocr-staleness-followup memory + commit history.)
//
// We assert by byte-equality between two captured JPEGs: strokes-only vs same
// strokes plus a text box positioned far from the strokes. If text boxes were
// drawn, the bounding box would grow to include (5000,5000) and the JPEG would
// differ. Identical bytes ⇒ text boxes were skipped in the OCR-bound render.
func TestProcessPage_OCRJPEGOmitsTextBoxes(t *testing.T) {
	s1 := newStore(t)
	seedPageWithStroke(t, s1)
	rec1 := &recordingOCR{text: "ink text"}
	b1 := New(s1, Deps{OCR: rec1}, nil)
	b1.processPage(context.Background(), pg1)

	s2 := newStore(t)
	seedPageWithStroke(t, s2)
	seedDistantTextBox(t, s2, "should not appear in jpeg")
	rec2 := &recordingOCR{text: "ink text"}
	b2 := New(s2, Deps{OCR: rec2}, nil)
	b2.processPage(context.Background(), pg1)

	if len(rec1.jpegs) != 1 || len(rec2.jpegs) != 1 {
		t.Fatalf("want 1 OCR call each, got %d and %d", len(rec1.jpegs), len(rec2.jpegs))
	}
	if !bytes.Equal(rec1.jpegs[0], rec2.jpegs[0]) {
		d1, _ := jpeg.Decode(bytes.NewReader(rec1.jpegs[0]))
		d2, _ := jpeg.Decode(bytes.NewReader(rec2.jpegs[0]))
		var b1Bounds, b2Bounds string
		if d1 != nil {
			b1Bounds = d1.Bounds().String()
		}
		if d2 != nil {
			b2Bounds = d2.Bounds().String()
		}
		t.Errorf("OCR JPEG differs when a text box is present (strokes-only bounds=%s vs strokes+box bounds=%s) — text box leaked into the OCR-bound render",
			b1Bounds, b2Bounds)
	}
}

// A page with no strokes but a text box must still render + index (the native box
// text becomes the body), not be dropped as "blank".
func TestProcessPage_TextBoxOnlyIndexed(t *testing.T) {
	s := newStore(t)
	seedPageWithTextBox(t, s, "typed note")
	fi := &fakeIndexer{}
	b := New(s, Deps{Indexer: fi}, nil) // no OCR — body comes purely from the box
	b.processPage(context.Background(), pg1)
	if fi.count() != 1 {
		t.Fatalf("text-box-only page should be indexed once, got %d", fi.count())
	}
	if fi.calls[0].body != "typed note" {
		t.Errorf("body = %q, want %q (native box text)", fi.calls[0].body, "typed note")
	}
}

// Native box text is appended to the OCR text in the indexed body.
func TestProcessPage_BoxTextAppendedToOCR(t *testing.T) {
	s := newStore(t)
	seedPageWithStroke(t, s)
	seedTextBoxOnExistingPage(t, s, "typed note")
	fi := &fakeIndexer{}
	b := New(s, Deps{Indexer: fi, OCR: fakeOCR{text: "ink text"}}, nil)
	b.processPage(context.Background(), pg1)
	if fi.count() != 1 {
		t.Fatalf("want 1 index call, got %d", fi.count())
	}
	if fi.calls[0].body != "ink text\ntyped note" {
		t.Errorf("body = %q, want %q", fi.calls[0].body, "ink text\ntyped note")
	}
}

func TestProcessPage_ClientOCRAppendedOnlyToIndexBody(t *testing.T) {
	s := newStore(t)
	seedPageWithStroke(t, s)
	seedClientPageText(t, s, "device words")
	fi := &fakeIndexer{}
	b := New(s, Deps{Indexer: fi, OCR: fakeOCR{text: "server words"}}, nil)

	b.processPage(context.Background(), pg1)

	if fi.count() != 1 {
		t.Fatalf("want 1 index call, got %d", fi.count())
	}
	if fi.calls[0].body != "server words\ndevice words" {
		t.Errorf("index body = %q, want server+client text", fi.calls[0].body)
	}
	ops, _, _, err := s.OpsSince(context.Background(), 0, siteA, 100)
	if err != nil {
		t.Fatalf("ops since: %v", err)
	}
	var serverText any
	for _, op := range ops {
		if op.Table == "page_text_from_server" && op.PK == pg1 {
			serverText = op.Cols["text"]
		}
	}
	if serverText != "server words" {
		t.Errorf("server row text = %v, want only server words", serverText)
	}
}

func TestProcessPage_NilOCRIndexesEmptyText(t *testing.T) {
	s := newStore(t)
	seedPageWithStroke(t, s)
	fi := &fakeIndexer{}
	b := New(s, Deps{Indexer: fi}, nil) // no OCR
	b.processPage(context.Background(), pg1)
	if fi.count() != 1 || fi.calls[0].body != "" {
		t.Errorf("nil OCR should index empty body once, got %+v", fi.calls)
	}
}

func TestBridge_StartPagesChangedStop(t *testing.T) {
	s := newStore(t)
	seedPageWithStroke(t, s)
	fi := &fakeIndexer{notify: make(chan string, 1)}
	b := New(s, Deps{Indexer: fi, OCR: fakeOCR{text: "async"}}, nil)
	b.Start(context.Background())
	defer b.Stop()

	b.PagesChanged(context.Background(), []syncstore.TablePK{{Table: "page", PK: pg1}, {Table: "page", PK: pg1}}) // dup → deduped

	select {
	case got := <-fi.notify:
		if got != "forestnote://"+nb1+"/"+pg1 {
			t.Errorf("indexed path = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridge did not process the page within 2s")
	}
}

// TestStatus_TracksProcessAndQueueLifecycle confirms the new Status counters
// move in the right directions: Pending matches channel depth, Processed
// increments once per processPage completion, and Dropped fires when a
// PagesChanged enqueue can't fit. InFlight is harder to assert
// deterministically (it's nonzero only mid-processPage) so we rely on the
// processed counter to confirm the defer ran at all.
func TestStatus_TracksProcessAndQueueLifecycle(t *testing.T) {
	s := newStore(t)
	seedPageWithStroke(t, s)
	fi := &fakeIndexer{}
	b := New(s, Deps{Indexer: fi, OCR: fakeOCR{text: "x"}}, nil)

	// Fresh bridge: every counter is zero, but Capacity is set.
	st := b.Status()
	if st.Pending != 0 || st.InFlight != 0 || st.Processed != 0 || st.Dropped != 0 {
		t.Errorf("fresh status non-zero: %+v", st)
	}
	if st.Capacity != 256 {
		t.Errorf("Capacity: got %d, want 256", st.Capacity)
	}

	// Drive a page through processPage directly — the inFlight defer
	// must run and Processed must tick up by one.
	b.processPage(context.Background(), pg1)
	st = b.Status()
	if st.Processed != 1 {
		t.Errorf("Processed after one page: got %d, want 1", st.Processed)
	}
	if st.InFlight != 0 {
		t.Errorf("InFlight should be 0 after processPage returns: got %d", st.InFlight)
	}
	if fi.count() != 1 {
		t.Errorf("sanity: indexer should have been called once, got %d", fi.count())
	}
}

// TestStatus_Dropped_IncrementsOnFullQueue verifies the queue-full path
// in PagesChanged bumps the Dropped counter rather than silently warning.
// We fill the channel by enqueueing capacity+overflow pages BEFORE
// starting the worker, so nothing drains.
func TestStatus_Dropped_IncrementsOnFullQueue(t *testing.T) {
	s := newStore(t)
	b := New(s, Deps{}, nil)

	pages := make([]syncstore.TablePK, b.Status().Capacity+5)
	for i := range pages {
		pages[i] = syncstore.TablePK{Table: "page", PK: fmt.Sprintf("PK_%03d", i)}
	}
	b.PagesChanged(context.Background(), pages)

	st := b.Status()
	if st.Pending != st.Capacity {
		t.Errorf("Pending should be at capacity: got %d, want %d", st.Pending, st.Capacity)
	}
	if st.Dropped != 5 {
		t.Errorf("Dropped: got %d, want 5", st.Dropped)
	}
}

// TestStatus_NilBridge confirms the nil-Bridge guard on Status — used by
// the source-level Status() passthrough when the bridge hasn't been
// constructed yet (source not started).
func TestStatus_NilBridge(t *testing.T) {
	var b *Bridge
	st := b.Status()
	if (st != Status{}) {
		t.Errorf("nil bridge Status: got %+v, want zero value", st)
	}
}
