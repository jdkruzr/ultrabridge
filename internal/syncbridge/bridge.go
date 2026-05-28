// Package syncbridge turns synced ForestNote strokes into searchable content:
// for each changed live page it renders the strokes, OCRs the image, and indexes
// + embeds the text on an opaque "forestnote://{notebook}/{page}" path — no
// filesystem writes (v1). It is the syncsvc.Bridge implementation, run off the
// /sync/v1 request path (after the apply commits) so OCR/embed latency and the
// single-writer notedb never stall a sync. See the Phase 2 plan in
// docs/implementation-plans/2026-05-26-forestnote-sync-ub-server.md.
package syncbridge

import (
	"bytes"
	"context"
	"image/jpeg"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sysop/ultrabridge/internal/fnpath"
	"github.com/sysop/ultrabridge/internal/forestrender"
	"github.com/sysop/ultrabridge/internal/rag"
	"github.com/sysop/ultrabridge/internal/syncstore"
)

// defaultOCRPrompt is used when no prompt override is configured.
const defaultOCRPrompt = "Transcribe all handwritten and printed text in this image. Output only the text."

// Narrow interfaces (satisfied by processor.Indexer, *processor.OCRClient,
// rag.Embedder, rag.EmbedStore). Local definitions keep syncbridge decoupled and
// trivially fakeable in tests.
type Indexer interface {
	IndexPage(ctx context.Context, path string, pageIdx int, source, bodyText, titleText, keywords string) error
	Delete(ctx context.Context, path string) error
}
type OCR interface {
	Recognize(ctx context.Context, jpegData []byte, prompt string) (string, error)
}
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}
type EmbedStore interface {
	Save(ctx context.Context, notePath string, page, chunk int, embedding []float32, model string) error
	DeletePage(ctx context.Context, notePath string, page int) error
	Delete(ctx context.Context, notePath string) error
}

// Deps bundles the pipeline collaborators. Any may be nil: a nil OCR renders +
// indexes empty text; a nil Embedder/EmbedStore skips embedding; a nil Indexer
// skips indexing. OCRPrompt (if set) is read per page so Settings changes apply
// without restart (mirrors the Boox worker).
type Deps struct {
	Indexer    Indexer
	OCR        OCR
	Embedder   Embedder
	EmbedStore EmbedStore
	EmbedModel string
	OCRPrompt  func() string
}

// Bridge consumes changed-page notifications and processes them on a worker
// goroutine. Construct with New, then Start; Stop drains and halts it.
type Bridge struct {
	store  *syncstore.Store
	deps   Deps
	logger *slog.Logger

	queue  chan string
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func New(store *syncstore.Store, deps Deps, logger *slog.Logger) *Bridge {
	if logger == nil {
		logger = slog.Default()
	}
	return &Bridge{store: store, deps: deps, logger: logger, queue: make(chan string, 256)}
}

// Start launches the worker. ctx bounds its lifetime (in addition to Stop).
func (b *Bridge) Start(ctx context.Context) {
	wctx, cancel := context.WithCancel(ctx)
	b.cancel = cancel
	b.wg.Add(1)
	go b.run(wctx)
}

// Stop cancels the worker and waits for the in-flight page to finish.
func (b *Bridge) Stop() {
	if b.cancel != nil {
		b.cancel()
	}
	b.wg.Wait()
}

// PagesChanged enqueues changed pages (deduped). Non-blocking: if the queue is
// full the page is dropped with a warning — it will be re-rendered on its next
// change, so this degrades gracefully rather than stalling /sync/v1.
func (b *Bridge) PagesChanged(_ context.Context, pages []syncstore.TablePK) {
	seen := make(map[string]bool, len(pages))
	for _, p := range pages {
		if p.Table != "page" || seen[p.PK] {
			continue
		}
		seen[p.PK] = true
		select {
		case b.queue <- p.PK:
		default:
			b.logger.Warn("syncbridge: queue full, dropping page (will re-render on next change)", "page", p.PK)
		}
	}
}

func (b *Bridge) run(ctx context.Context) {
	defer b.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case pagePK := <-b.queue:
			b.processPage(ctx, pagePK)
		}
	}
}

// processPage is best-effort: every failure is logged, never propagated.
func (b *Bridge) processPage(ctx context.Context, pagePK string) {
	notebookID, live, err := b.store.LivePage(ctx, pagePK)
	if err != nil {
		b.logger.Error("syncbridge: live-page lookup failed", "page", pagePK, "err", err)
		return
	}
	path := fnpath.Page(notebookID, pagePK)

	if !live {
		// Deleted/missing page → drop any prior index + embedding so neither search
		// nor RAG returns it. (notebookID is still the deleted row's, so the path
		// matches what was indexed; a missing page yields an empty notebook id and
		// a harmless no-op delete.)
		b.dropPage(ctx, pagePK, path)
		return
	}

	strokes, err := b.store.LivePageStrokes(ctx, pagePK)
	if err != nil {
		b.logger.Error("syncbridge: stroke read failed", "page", pagePK, "err", err)
		return
	}
	boxes, err := b.store.LivePageTextBoxes(ctx, pagePK)
	if err != nil {
		b.logger.Error("syncbridge: text box read failed", "page", pagePK, "err", err)
		return
	}
	if len(strokes) == 0 && len(boxes) == 0 {
		// All strokes erased and no text boxes → the page is now blank; drop it.
		b.dropPage(ctx, pagePK, path)
		return
	}

	// Render strokes only for OCR. Text boxes are appended as canonical native UTF-8
	// further down, so drawing them here too would round-trip them through OCR and
	// stack each box's text 2-3× in the authored body (which the dialog renders to
	// the user verbatim). See the body-composition note below. The web-UI render path
	// in service/note.go keeps drawing text boxes — only the OCR-bound JPEG omits them.
	img, err := forestrender.RenderPage(MapStrokes(strokes), nil)
	if err != nil {
		b.logger.Error("syncbridge: render failed", "page", pagePK, "err", err)
		return
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		b.logger.Error("syncbridge: jpeg encode failed", "page", pagePK, "err", err)
		return
	}

	var text string
	if b.deps.OCR != nil {
		prompt := defaultOCRPrompt
		if b.deps.OCRPrompt != nil {
			if p := b.deps.OCRPrompt(); p != "" {
				prompt = p
			}
		}
		if t, err := b.deps.OCR.Recognize(ctx, buf.Bytes(), prompt); err != nil {
			b.logger.Warn("syncbridge: OCR failed", "page", pagePK, "err", err)
		} else {
			text = t
		}
	}

	// Text-box content is native UTF-8 — higher quality than OCR'ing the rendered
	// glyphs — so append it to the indexed/embedded body. The OCR-bound JPEG above
	// is rendered WITHOUT text boxes (boxes=nil) so OCR never sees them; this append
	// is now the canonical (single) source of text-box content in the body — no
	// round-trip duplication into the dialog.
	body := text
	if bt := joinTextBoxes(boxes); bt != "" {
		if body != "" {
			body += "\n"
		}
		body += bt
	}

	if b.deps.Indexer != nil {
		if err := b.deps.Indexer.IndexPage(ctx, path, 0, "forestnote", body, "", ""); err != nil {
			b.logger.Warn("syncbridge: index failed", "page", pagePK, "err", err)
		}
	}

	// Push the recognized text down to the device (page_text_from_server). On a page
	// that OCR'd to nothing (and has no text boxes), tombstone instead so stale text
	// does not linger on-device. Best-effort: a failure here must not stall the bridge.
	// model is "" because the narrow OCR interface does not expose one (v1).
	if body != "" {
		if err := b.store.AuthorPageText(ctx, pagePK, body, time.Now().UnixMilli(), ""); err != nil {
			b.logger.Warn("syncbridge: page text author failed", "page", pagePK, "err", err)
		}
	} else if err := b.store.AuthorPageTextTombstone(ctx, pagePK); err != nil {
		b.logger.Warn("syncbridge: page text tombstone failed", "page", pagePK, "err", err)
	}

	if body != "" && b.deps.Embedder != nil && b.deps.EmbedStore != nil {
		rag.EmbedAndStorePage(ctx, b.deps.Embedder, b.deps.EmbedStore, path, 0, body, b.deps.EmbedModel, b.logger)
	}
}

// joinTextBoxes concatenates the non-empty text of a page's boxes, newline-joined,
// for the search/embedding body. Order follows the z-sorted read (stable enough
// for a search payload; exact reading order is not required).
func joinTextBoxes(boxes []syncstore.TextBoxData) string {
	var parts []string
	for _, b := range boxes {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// dropPage removes a page from the search index and the embedding store
// (best-effort) so neither keyword search nor RAG returns deleted content.
func (b *Bridge) dropPage(ctx context.Context, pagePK, path string) {
	if b.deps.Indexer != nil {
		if err := b.deps.Indexer.Delete(ctx, path); err != nil {
			b.logger.Warn("syncbridge: index delete failed", "page", pagePK, "err", err)
		}
	}
	if b.deps.EmbedStore != nil {
		if err := b.deps.EmbedStore.Delete(ctx, path); err != nil {
			b.logger.Warn("syncbridge: embedding delete failed", "page", pagePK, "err", err)
		}
	}
	// Tombstone the page's recognized-text row so a deleted/blank page stops carrying
	// OCR text to devices (best-effort, like the index/embedding deletes above).
	if err := b.store.AuthorPageTextTombstone(ctx, pagePK); err != nil {
		b.logger.Warn("syncbridge: page text tombstone failed", "page", pagePK, "err", err)
	}
}

// MapStrokes maps stored mirror strokes onto forestrender's input. Exported so
// the note service's on-the-fly page renderer shares this exact mapping instead
// of duplicating it (the two would otherwise drift if a stroke field is added).
func MapStrokes(sd []syncstore.StrokeData) []forestrender.Stroke {
	out := make([]forestrender.Stroke, len(sd))
	for i, s := range sd {
		out[i] = forestrender.Stroke{
			Color: s.Color, PenWidthMin: s.PenWidthMin, PenWidthMax: s.PenWidthMax,
			Points: s.Points, Z: s.Z,
		}
	}
	return out
}

// MapTextBoxes maps stored mirror text boxes onto forestrender's input. Exported
// for the same reason as MapStrokes: the note service's on-the-fly renderer reuses
// this exact mapping so the two paths can't drift.
func MapTextBoxes(td []syncstore.TextBoxData) []forestrender.TextBox {
	out := make([]forestrender.TextBox, len(td))
	for i, t := range td {
		out[i] = forestrender.TextBox{
			X: t.X, Y: t.Y, Width: t.Width, Height: t.Height,
			Text: t.Text, FontName: t.FontName, FontSize: t.FontSize,
			Color: t.Color, Weight: t.Weight, BorderWidth: t.BorderWidth, Z: t.Z,
		}
	}
	return out
}
