// Package digestindex makes synced Supernote digests ("summary" excerpts)
// searchable: it indexes each digest's text into the shared FTS5 index and
// embeds it for RAG, on an opaque "digest://<uniqueIdentifier>" path — the
// digest analogue of syncbridge's "forestnote://" pages. Indexing runs on a
// worker goroutine off the device-sync request path so OCR/embed latency and the
// single-writer notedb never stall a digest sync (mirrors internal/syncbridge).
package digestindex

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/sysop/ultrabridge/internal/rag"
)

// Source is the provenance tag written to note_content.source for digest rows,
// and the value the search facet filters on (cf. "forestnote", "myScript", "api").
const Source = "digest"

// Path is the opaque note_content key for a digest, keyed by its stable
// uniqueIdentifier so re-indexing upserts and deindex targets the same row.
func Path(uid string) string { return "digest://" + uid }

// Narrow interfaces (satisfied by search.Store, rag.Embedder, rag.Store). Local
// definitions keep digestindex decoupled and trivially fakeable in tests.
type Indexer interface {
	IndexPage(ctx context.Context, path string, pageIdx int, source, bodyText, titleText, keywords string) error
	Delete(ctx context.Context, path string) error
}
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}
type EmbedStore interface {
	Save(ctx context.Context, notePath string, page, chunk int, embedding []float32, model string) error
	DeletePage(ctx context.Context, notePath string, page int) error
	Delete(ctx context.Context, notePath string) error
}

// Deps bundles the collaborators. Any may be nil: a nil Indexer skips FTS
// indexing; a nil Embedder/EmbedStore skips embedding (the digest is still
// keyword-searchable). EmbedModel labels stored embeddings.
type Deps struct {
	Indexer    Indexer
	Embedder   Embedder
	EmbedStore EmbedStore
	EmbedModel string
}

// Bridge consumes index/deindex requests and processes them on a worker
// goroutine. Construct with New, then Start; Stop drains the in-flight item.
type Bridge struct {
	deps   Deps
	logger *slog.Logger

	queue  chan task
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// task is one unit of work: index a digest's fields, or (deindex) drop its row.
type task struct {
	deindex bool
	uid     string
	name    string
	content string
	comment string
	tags    string
}

func New(deps Deps, logger *slog.Logger) *Bridge {
	if logger == nil {
		logger = slog.Default()
	}
	return &Bridge{deps: deps, logger: logger, queue: make(chan task, 256)}
}

// Start launches the worker. ctx bounds its lifetime (in addition to Stop).
func (b *Bridge) Start(ctx context.Context) {
	wctx, cancel := context.WithCancel(ctx)
	b.cancel = cancel
	b.wg.Add(1)
	go b.run(wctx)
}

// Stop cancels the worker and waits for the in-flight item to finish.
func (b *Bridge) Stop() {
	if b.cancel != nil {
		b.cancel()
	}
	b.wg.Wait()
}

// Index enqueues a (re)index of a digest item. Non-blocking: if the queue is
// full the item is dropped with a warning — it will be re-indexed on its next
// change or the next startup backfill, so this degrades gracefully rather than
// stalling the digest sync. Satisfies handlers.DigestIndexer.
func (b *Bridge) Index(uid, name, content, comment, tags string) {
	b.enqueue(task{uid: uid, name: name, content: content, comment: comment, tags: tags})
}

// Deindex enqueues removal of a digest's search + embedding rows (on delete).
// Satisfies handlers.DigestIndexer.
func (b *Bridge) Deindex(uid string) {
	b.enqueue(task{deindex: true, uid: uid})
}

func (b *Bridge) enqueue(t task) {
	if t.uid == "" {
		return
	}
	select {
	case b.queue <- t:
	default:
		b.logger.Warn("digestindex: queue full, dropping (will re-index on next change/backfill)", "uid", t.uid)
	}
}

func (b *Bridge) run(ctx context.Context) {
	defer b.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-b.queue:
			b.process(ctx, t)
		}
	}
}

// process is best-effort: every failure is logged, never propagated.
func (b *Bridge) process(ctx context.Context, t task) {
	path := Path(t.uid)

	// Body is the excerpt + handwriting comment; the digest name is the title.
	body := strings.TrimSpace(strings.Join(nonEmpty(t.content, t.comment), "\n"))

	// A deindex, or a digest with no searchable text at all, drops the row so
	// neither keyword search nor RAG returns stale content.
	if t.deindex || (body == "" && strings.TrimSpace(t.name) == "") {
		b.drop(ctx, t.uid, path)
		return
	}

	if b.deps.Indexer != nil {
		if err := b.deps.Indexer.IndexPage(ctx, path, 0, Source, body, t.name, t.tags); err != nil {
			b.logger.Warn("digestindex: index failed", "uid", t.uid, "err", err)
		}
	}

	// Embed the full searchable text (name + body), chunked, so digests
	// participate in semantic retrieval and chat context.
	embedText := strings.TrimSpace(strings.Join(nonEmpty(t.name, body), "\n"))
	if embedText != "" && b.deps.Embedder != nil && b.deps.EmbedStore != nil {
		rag.EmbedAndStorePage(ctx, b.deps.Embedder, b.deps.EmbedStore, path, 0, embedText, b.deps.EmbedModel, b.logger)
	}
}

// drop removes a digest from the search index and the embedding store
// (best-effort) so neither keyword search nor RAG returns deleted content.
func (b *Bridge) drop(ctx context.Context, uid, path string) {
	if b.deps.Indexer != nil {
		if err := b.deps.Indexer.Delete(ctx, path); err != nil {
			b.logger.Warn("digestindex: index delete failed", "uid", uid, "err", err)
		}
	}
	if b.deps.EmbedStore != nil {
		if err := b.deps.EmbedStore.Delete(ctx, path); err != nil {
			b.logger.Warn("digestindex: embedding delete failed", "uid", uid, "err", err)
		}
	}
}

func nonEmpty(ss ...string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}
