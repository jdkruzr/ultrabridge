package digestindex

import (
	"context"
	"sync"
	"testing"
	"time"
)

type indexCall struct {
	path, source, body, title, keywords string
	page                                int
}

type fakeIndexer struct {
	mu      sync.Mutex
	indexed []indexCall
	deleted []string
}

func (f *fakeIndexer) IndexPage(_ context.Context, path string, page int, source, body, title, keywords string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.indexed = append(f.indexed, indexCall{path, source, body, title, keywords, page})
	return nil
}
func (f *fakeIndexer) Delete(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, path)
	return nil
}

type fakeEmbed struct {
	mu     sync.Mutex
	saved  []string // paths
	deld   []string
	texts  []string
}

func (f *fakeEmbed) Embed(_ context.Context, text string) ([]float32, error) {
	f.mu.Lock()
	f.texts = append(f.texts, text)
	f.mu.Unlock()
	return []float32{1, 2, 3}, nil
}
func (f *fakeEmbed) Save(_ context.Context, path string, _ int, _ int, _ []float32, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = append(f.saved, path)
	return nil
}
func (f *fakeEmbed) DeletePage(_ context.Context, _ string, _ int) error { return nil }
func (f *fakeEmbed) Delete(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deld = append(f.deld, path)
	return nil
}

func TestPath(t *testing.T) {
	if got := Path("abc-123"); got != "digest://abc-123" {
		t.Fatalf("Path = %q", got)
	}
}

// process is the deterministic core (no goroutine timing).
func TestProcess_IndexesAndEmbeds(t *testing.T) {
	idx := &fakeIndexer{}
	emb := &fakeEmbed{}
	b := New(Deps{Indexer: idx, Embedder: emb, EmbedStore: emb, EmbedModel: "m"}, nil)

	b.process(context.Background(), task{uid: "u1", name: "My Note", content: "the excerpt", comment: "handwriting note", tags: "work,ideas"})

	if len(idx.indexed) != 1 {
		t.Fatalf("expected 1 index call, got %d", len(idx.indexed))
	}
	c := idx.indexed[0]
	if c.path != "digest://u1" || c.page != 0 || c.source != Source {
		t.Errorf("index key wrong: %+v", c)
	}
	if c.title != "My Note" || c.keywords != "work,ideas" {
		t.Errorf("title/keywords wrong: %+v", c)
	}
	if c.body != "the excerpt\nhandwriting note" {
		t.Errorf("body = %q", c.body)
	}
	if len(emb.saved) != 1 || emb.saved[0] != "digest://u1" {
		t.Errorf("expected embed save at digest://u1, got %v", emb.saved)
	}
	if want := "My Note\nthe excerpt\nhandwriting note"; emb.texts[0] != want {
		t.Errorf("embed text = %q, want %q", emb.texts[0], want)
	}
}

func TestProcess_EmptyTextDrops(t *testing.T) {
	idx := &fakeIndexer{}
	emb := &fakeEmbed{}
	b := New(Deps{Indexer: idx, Embedder: emb, EmbedStore: emb}, nil)

	// No name, no content, no comment → nothing searchable → drop, don't index.
	b.process(context.Background(), task{uid: "empty", content: "   "})

	if len(idx.indexed) != 0 {
		t.Errorf("expected no index, got %v", idx.indexed)
	}
	if len(idx.deleted) != 1 || idx.deleted[0] != "digest://empty" {
		t.Errorf("expected delete of digest://empty, got %v", idx.deleted)
	}
	if len(emb.deld) != 1 || emb.deld[0] != "digest://empty" {
		t.Errorf("expected embed delete, got %v", emb.deld)
	}
}

func TestProcess_Deindex(t *testing.T) {
	idx := &fakeIndexer{}
	emb := &fakeEmbed{}
	b := New(Deps{Indexer: idx, Embedder: emb, EmbedStore: emb}, nil)

	b.process(context.Background(), task{deindex: true, uid: "gone", name: "still has a name"})

	if len(idx.indexed) != 0 {
		t.Errorf("deindex must not index, got %v", idx.indexed)
	}
	if len(idx.deleted) != 1 || idx.deleted[0] != "digest://gone" {
		t.Errorf("expected delete, got %v", idx.deleted)
	}
}

func TestProcess_NoEmbedderStillIndexes(t *testing.T) {
	idx := &fakeIndexer{}
	b := New(Deps{Indexer: idx}, nil) // embedder/store nil

	b.process(context.Background(), task{uid: "u2", name: "n", content: "c"})

	if len(idx.indexed) != 1 {
		t.Fatalf("expected index even without embedder, got %d", len(idx.indexed))
	}
}

// Async path: Index → worker eventually indexes.
func TestBridge_AsyncIndex(t *testing.T) {
	idx := &fakeIndexer{}
	b := New(Deps{Indexer: idx}, nil)
	b.Start(context.Background())
	defer b.Stop()

	b.Index("a1", "name", "body", "", "")

	deadline := time.After(2 * time.Second)
	for {
		idx.mu.Lock()
		n := len(idx.indexed)
		idx.mu.Unlock()
		if n == 1 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("index never happened")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestBridge_EnqueueEmptyUIDIgnored(t *testing.T) {
	b := New(Deps{}, nil)
	b.Index("", "n", "c", "", "") // no uid → dropped silently
	b.Deindex("")
	if len(b.queue) != 0 {
		t.Errorf("empty-uid tasks should not enqueue, queue len = %d", len(b.queue))
	}
}
