package forestnote

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/sysop/ultrabridge/internal/fnpath"
	"github.com/sysop/ultrabridge/internal/syncstore"
)

// backfillPageText authors page_text_from_server rows for ForestNote pages that were
// OCR'd before this feature existed: their text sits in note_content (the private search
// index) but never reached the device. It reads the indexed body for each forestnote://
// page and authors it through the store, so an updated client receives the historical
// text on its next pull without any re-OCR.
//
// Idempotent without a global once-flag: it skips any page that already has a
// fn_page_text_from_server row (every live OCR pass authors one), so it only fills the
// pre-feature gap and a restart re-authors nothing. Safe to run on every Start. Reads are
// outside any transaction; a page added between the preload and the scan simply gets
// picked up on the next run. Returns the number of pages authored.
func backfillPageText(ctx context.Context, db *sql.DB, store *syncstore.Store, logger *slog.Logger) (int, error) {
	// Pages that already have a recognized-text row (live OR tombstoned) — skip them.
	have := make(map[string]bool)
	hrows, err := db.QueryContext(ctx, `SELECT id FROM fn_page_text_from_server`)
	if err != nil {
		return 0, fmt.Errorf("preload page-text ids: %w", err)
	}
	for hrows.Next() {
		var id string
		if err := hrows.Scan(&id); err != nil {
			hrows.Close()
			return 0, fmt.Errorf("scan page-text id: %w", err)
		}
		have[id] = true
	}
	hrows.Close()
	if err := hrows.Err(); err != nil {
		return 0, err
	}

	// Live pages only — don't resurrect text for a deleted page.
	live := make(map[string]bool)
	prows, err := db.QueryContext(ctx, `SELECT id FROM fn_page WHERE deleted_at IS NULL`)
	if err != nil {
		return 0, fmt.Errorf("preload live page ids: %w", err)
	}
	for prows.Next() {
		var id string
		if err := prows.Scan(&id); err != nil {
			prows.Close()
			return 0, fmt.Errorf("scan live page id: %w", err)
		}
		live[id] = true
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return 0, err
	}

	// Indexed ForestNote page text. FN indexes one row per page at page 0 (source
	// "forestnote"); an empty body has nothing to push, so the live bridge tombstones
	// it — backfill simply skips it.
	//
	// The candidates are fully drained into a slice BEFORE any AuthorPageText call:
	// notedb runs MaxOpenConns(1), so an open cursor owns the only connection, and
	// AuthorOps' BeginTx would deadlock waiting for a second one. (Same discipline as
	// SoftDeleteNotebook — read, close, then author.)
	type pending struct {
		pageID, body, model string
		ocrAt               int64
	}
	rows, err := db.QueryContext(ctx,
		`SELECT note_path, COALESCE(body_text, ''), COALESCE(model, ''), indexed_at
		   FROM note_content WHERE note_path LIKE 'forestnote://%'`)
	if err != nil {
		return 0, fmt.Errorf("scan note_content: %w", err)
	}
	var todo []pending
	for rows.Next() {
		var notePath, body, model string
		var indexedAt int64
		if err := rows.Scan(&notePath, &body, &model, &indexedAt); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan content row: %w", err)
		}
		pageID := fnpath.PageID(notePath)
		if body == "" || have[pageID] || !live[pageID] || !syncstore.IsULID(pageID) {
			continue
		}
		// Unit boundary: note_content.indexed_at is SECONDS (search.Store.Index
		// stamps time.Now().Unix()), but the sync layer is milliseconds
		// throughout — the live path (syncbridge) and the tombstone path both
		// pass UnixMilli into AuthorPageText. Passing the raw value through
		// would relay an ocr_at/created_at ~1000x too small (reading as Jan
		// 1970) to every device that pulls a backfilled page.
		todo = append(todo, pending{pageID: pageID, body: body, model: model, ocrAt: indexedAt * 1000})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate note_content: %w", err)
	}

	authored := 0
	for _, p := range todo {
		if err := ctx.Err(); err != nil {
			return authored, err
		}
		if err := store.AuthorPageText(ctx, p.pageID, p.body, p.ocrAt, p.model); err != nil {
			// Best-effort: log and continue so one bad row doesn't abort the whole backfill.
			logger.Warn("forestnote: page-text backfill author failed", "page", p.pageID, "err", err)
			continue
		}
		authored++
	}
	return authored, nil
}
