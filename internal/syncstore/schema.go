package syncstore

import (
	"context"
	"database/sql"
	"fmt"
)

// Migrate creates the sync mirror, op changelog, and cursor tables idempotently.
// Called from main when device sync is enabled (precedent: digeststore.Migrate /
// staging.Migrate), NOT by notedb.Open — this keeps sync storage gated to the
// SyncEnabled setting. Tables live in the shared notedb (WAL, MaxOpenConns=1).
func Migrate(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS sync_seq (
			id       INTEGER PRIMARY KEY CHECK (id = 1),
			last_seq INTEGER NOT NULL
		)`,
		`INSERT OR IGNORE INTO sync_seq (id, last_seq) VALUES (1, 0)`,

		`CREATE TABLE IF NOT EXISTS sync_ops (
			seq        INTEGER PRIMARY KEY,
			site_id    TEXT    NOT NULL,
			op_seq     INTEGER NOT NULL,
			table_name TEXT    NOT NULL,
			pk         TEXT    NOT NULL,
			wall_ts    INTEGER NOT NULL,
			payload    TEXT    NOT NULL,
			applied_at INTEGER NOT NULL,
			UNIQUE (site_id, op_seq)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sync_ops_seq ON sync_ops(seq)`,

		`CREATE TABLE IF NOT EXISTS sync_cursors (
			site_id       TEXT PRIMARY KEY,
			last_pull_seq INTEGER NOT NULL DEFAULT 0, -- global relay high-water this device has pulled
			acked_op_seq  INTEGER NOT NULL DEFAULT 0, -- contiguous accepted_through for this device (spec §4.1)
			updated_at    INTEGER NOT NULL
		)`,

		`CREATE TABLE IF NOT EXISTS fn_notebook (
			id          TEXT PRIMARY KEY,
			name        TEXT,
			sort_order  INTEGER,
			created_at  INTEGER,
			deleted_at  INTEGER,
			lww_wall_ts INTEGER NOT NULL,
			lww_op_seq  INTEGER NOT NULL,
			lww_site_id TEXT    NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS fn_page (
			id          TEXT PRIMARY KEY,
			notebook_id TEXT,
			sort_order  INTEGER,
			created_at  INTEGER,
			deleted_at  INTEGER,
			lww_wall_ts INTEGER NOT NULL,
			lww_op_seq  INTEGER NOT NULL,
			lww_site_id TEXT    NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS fn_stroke (
			id            TEXT PRIMARY KEY,
			page_id       TEXT,
			color         INTEGER,
			pen_width_min INTEGER,
			pen_width_max INTEGER,
			points        BLOB,
			z             INTEGER,
			created_at    INTEGER,
			deleted_at    INTEGER,
			lww_wall_ts   INTEGER NOT NULL,
			lww_op_seq    INTEGER NOT NULL,
			lww_site_id   TEXT    NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_fn_page_nb ON fn_page(notebook_id)`,
		`CREATE INDEX IF NOT EXISTS idx_fn_stroke_pg ON fn_stroke(page_id, z)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("syncstore migrate: %w", err)
		}
	}
	return nil
}
