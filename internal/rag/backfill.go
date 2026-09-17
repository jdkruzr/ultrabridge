package rag

// FCIS: Imperative Shell

import (
	"context"
	"database/sql"
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

		err := store.admitBackfill(ctx, p.NotePath, func(run context.Context) error {
			// The inventory may predate a library replacement. Re-read after
			// admission so queued global work cannot publish old text afterward.
			var body string
			if err := store.db.QueryRowContext(run, `SELECT body_text FROM note_content WHERE note_path=? AND page=?`, p.NotePath, p.Page).Scan(&body); err != nil {
				if err == sql.ErrNoRows {
					return nil
				}
				return err
			}
			if n := EmbedAndStorePage(run, embedder, store, p.NotePath, p.Page, body, model, logger); n > 0 {
				embedded++
			}
			return nil
		})
		if err != nil {
			return embedded, err
		}
	}

	logger.Info("embedding backfill complete", "embedded", embedded, "total", len(pages))
	return embedded, nil
}
