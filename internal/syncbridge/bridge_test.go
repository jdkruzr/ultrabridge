package syncbridge

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"fmt"
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
			Cols: map[string]any{"name": "NB", "sort_order": float64(0), "created_at": float64(1000), "deleted_at": nil}},
		{Table: "page", PK: pg1, SiteID: siteA, OpSeq: 2, WallTS: 1010,
			Cols: map[string]any{"notebook_id": nb1, "sort_order": float64(0), "created_at": float64(1010), "deleted_at": nil}},
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
			Cols: map[string]any{"notebook_id": nb1, "sort_order": float64(0), "created_at": float64(1010), "deleted_at": float64(2000)}},
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
			Cols: map[string]any{"notebook_id": nb1, "sort_order": float64(0), "created_at": float64(1000), "deleted_at": nil}},
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
