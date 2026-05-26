package rag

// FCIS: Imperative Shell

import (
	"context"
	"log/slog"
)

// Backfill embeds all pages in note_content that don't have a corresponding
// note_embeddings row. Returns the number of pages embedded.
func Backfill(ctx context.Context, store *Store, embedder Embedder, model string, logger *slog.Logger) (int, error) {
	pages, err := store.UnembeddedPages(ctx)
	if err != nil {
		return 0, err
	}

	if len(pages) == 0 {
		logger.Info("embedding backfill: all pages already embedded")
		return 0, nil
	}

	logger.Info("starting embedding backfill", "pages", len(pages))

	embedded := 0
	for _, p := range pages {
		if ctx.Err() != nil {
			return embedded, ctx.Err()
		}

		if n := EmbedAndStorePage(ctx, embedder, store, p.NotePath, p.Page, p.BodyText, model, logger); n > 0 {
			embedded++
		}
	}

	logger.Info("embedding backfill complete", "embedded", embedded, "total", len(pages))
	return embedded, nil
}
