package forestnote

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/fnpath"
	"github.com/sysop/ultrabridge/internal/syncstore"
)

// TestBackfillPageText_NoDeadlockUnderSingleConn reproduces the startup hang: with
// the production MaxOpenConns(1) cap, holding the note_content cursor open while
// calling AuthorPageText (which BeginTx's on the same pool) deadlocks — the cursor
// owns the only connection and BeginTx waits for it forever. The guard fails fast
// instead of hanging the suite; the fix (drain the cursor before authoring) makes it
// return and author the row.
func TestBackfillPageText_NoDeadlockUnderSingleConn(t *testing.T) {
	db := testDB(t) // SetMaxOpenConns(1), as notedb.Open does in production
	ctx := context.Background()

	if err := syncstore.Migrate(ctx, db); err != nil {
		t.Fatalf("syncstore migrate: %v", err)
	}
	store := syncstore.New(db)

	// Minimal note_content (the columns the backfill selects).
	if _, err := db.ExecContext(ctx, `CREATE TABLE note_content (
		id INTEGER PRIMARY KEY, note_path TEXT NOT NULL, page INTEGER NOT NULL,
		body_text TEXT, model TEXT, indexed_at INTEGER NOT NULL,
		UNIQUE(note_path, page))`); err != nil {
		t.Fatalf("create note_content: %v", err)
	}

	const (
		site = "0000000000000000000000SYTE"
		nb   = "00000000000000000000000NBA"
		pg   = "00000000000000000000000PGA"
	)
	// A live page (so backfill doesn't skip it as deleted).
	if _, err := store.ApplyBatch(ctx, site, []syncstore.Op{
		{Table: "page", PK: pg, SiteID: site, OpSeq: 1, WallTS: 1000,
			Cols: map[string]any{"notebook_id": nb, "sort_order": float64(0), "created_at": float64(1000), "deleted_at": nil, "template": nil, "template_pitch_mm": nil}},
	}); err != nil {
		t.Fatalf("seed page: %v", err)
	}
	// Its indexed OCR text, awaiting backfill.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO note_content (note_path, page, body_text, model, indexed_at) VALUES (?, 0, ?, ?, ?)`,
		fnpath.Page(nb, pg), "hello world", "modelX", 1234); err != nil {
		t.Fatalf("seed note_content: %v", err)
	}

	done := make(chan int, 1)
	var bferr error
	go func() {
		n, err := backfillPageText(ctx, db, store, slog.Default())
		bferr = err
		done <- n
	}()

	select {
	case n := <-done:
		if bferr != nil {
			t.Fatalf("backfill: %v", bferr)
		}
		if n != 1 {
			t.Fatalf("authored = %d, want 1", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("backfillPageText deadlocked: held the single connection across AuthorOps")
	}

	var text string
	var ocrAt, createdAt int64
	if err := db.QueryRowContext(ctx,
		`SELECT text, ocr_at, created_at FROM fn_page_text_from_server WHERE id = ?`, pg).
		Scan(&text, &ocrAt, &createdAt); err != nil {
		t.Fatalf("read materialized row: %v", err)
	}
	if text != "hello world" {
		t.Errorf("materialized text = %q, want %q", text, "hello world")
	}
	// Unit boundary: the seeded indexed_at is SECONDS (search.Store.Index
	// stamps time.Now().Unix()), but the sync layer is milliseconds — the live
	// syncbridge path and the tombstone path both pass UnixMilli into
	// AuthorPageText. Passing the raw value through relayed an ocr_at ~1000x
	// too small (reading as Jan 1970) to every device pulling a backfilled page.
	if want := int64(1234 * 1000); ocrAt != want {
		t.Errorf("ocr_at = %d, want %d (seconds converted to millis)", ocrAt, want)
	}
	if createdAt != int64(1234*1000) {
		t.Errorf("created_at = %d, want %d", createdAt, 1234*1000)
	}

	// Idempotent: a second run authors nothing (row already present).
	if n, err := backfillPageText(ctx, db, store, slog.Default()); err != nil || n != 0 {
		t.Fatalf("second run authored = %d, err = %v, want 0/nil", n, err)
	}
}
