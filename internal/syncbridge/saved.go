package syncbridge

import (
	"context"
	"strings"

	"github.com/sysop/ultrabridge/internal/fnpath"
	"github.com/sysop/ultrabridge/internal/rag"
)

// IndexSavedText rebuilds derived search/embedding state without OCR, rendering,
// or authoring/tombstoning any synchronized transcription. Call on the worker.
func (b *Bridge) IndexSavedText(ctx context.Context, page, serverText string) error {
	notebook, live, err := b.store.LivePage(ctx, page)
	if err != nil || !live {
		return err
	}
	boxes, err := b.store.LivePageTextBoxes(ctx, page)
	if err != nil {
		return err
	}
	client, _, err := b.store.LivePageTextFromClient(ctx, page)
	if err != nil {
		return err
	}
	parts := []string{}
	if strings.TrimSpace(serverText) != "" {
		parts = append(parts, serverText)
	}
	// Historical server text may already contain typed text. Exact duplicate
	// components need not be indexed twice; originals remain untouched.
	for _, text := range []string{joinTextBoxes(boxes), client.Text} {
		if strings.TrimSpace(text) != "" && text != serverText {
			parts = append(parts, text)
		}
	}
	body := strings.Join(parts, "\n")
	path := fnpath.Page(notebook, page)
	if b.deps.Indexer != nil {
		if err = b.deps.Indexer.IndexPage(ctx, path, 0, "forestnote", body, "", ""); err != nil {
			return err
		}
	}
	if body != "" && b.deps.Embedder != nil && b.deps.EmbedStore != nil {
		rag.EmbedAndStorePage(ctx, b.deps.Embedder, b.deps.EmbedStore, path, 0, body, b.deps.EmbedModel, b.logger)
	}
	return ctx.Err()
}
