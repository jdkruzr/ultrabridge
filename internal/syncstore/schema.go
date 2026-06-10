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

		// sync_site holds UB's OWN authoring identity: a persistent ULID site_id
		// (so server-authored ops are wire-legal — see newULID/AuthorOps), a
		// monotonic per-site op_seq counter, and last_hlc, the durable op_ts Hybrid
		// Logical Clock (spec/hlc.md) seeded from max(last_hlc, max sync_ops.wall_ts)
		// below. Seeded once; the ULID survives restarts so the device sees a stable
		// origin for UB's ops.
		`CREATE TABLE IF NOT EXISTS sync_site (
			id          INTEGER PRIMARY KEY CHECK (id = 1),
			site_id     TEXT    NOT NULL,
			last_op_seq INTEGER NOT NULL DEFAULT 0,
			last_hlc    INTEGER NOT NULL DEFAULT 0
		)`,

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

		// fn_folder carries the same lww_* provenance triple as the other mirror
		// tables — uniform LWW rule, no special-casing. parent_folder_id is a plain
		// LWW column (re-parenting resolves by greatest key); the mirror enforces no
		// FK, so apply order is irrelevant. (spec §3.1)
		`CREATE TABLE IF NOT EXISTS fn_folder (
			id               TEXT PRIMARY KEY,
			name             TEXT,
			sort_order       INTEGER,
			created_at       INTEGER,
			deleted_at       INTEGER,
			parent_folder_id TEXT,
			lww_wall_ts      INTEGER NOT NULL,
			lww_op_seq       INTEGER NOT NULL,
			lww_site_id      TEXT    NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_fn_folder_parent ON fn_folder(parent_folder_id)`,
		// New-DB column sets include folder_id (notebook) and template/
		// template_pitch_mm (page); existing DBs get them via ensureColumn below.
		`CREATE TABLE IF NOT EXISTS fn_notebook (
			id          TEXT PRIMARY KEY,
			name        TEXT,
			sort_order  INTEGER,
			created_at  INTEGER,
			deleted_at  INTEGER,
			folder_id   TEXT,
			lww_wall_ts INTEGER NOT NULL,
			lww_op_seq  INTEGER NOT NULL,
			lww_site_id TEXT    NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS fn_page (
			id                TEXT PRIMARY KEY,
			notebook_id       TEXT,
			sort_order        INTEGER,
			created_at        INTEGER,
			deleted_at        INTEGER,
			template          TEXT,
			template_pitch_mm INTEGER,
			lww_wall_ts       INTEGER NOT NULL,
			lww_op_seq        INTEGER NOT NULL,
			lww_site_id       TEXT    NOT NULL
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
		// fn_text_box mirrors a z-ordered text element on a page (schema v2). Same
		// shape discipline as fn_stroke: all 14 synced value columns + the provenance
		// trio. Geometry/font_size are virtual units (page short axis = 10,000), color
		// is the unsigned ARGB int64 stored verbatim (identical to stroke.color), z is
		// the paint band (0 = below ink, 1 = above). No FK on page_id — the wire is
		// row-level LWW (the client's local ON DELETE CASCADE does not replicate).
		`CREATE TABLE IF NOT EXISTS fn_text_box (
			id           TEXT PRIMARY KEY,
			page_id      TEXT,
			x            INTEGER,
			y            INTEGER,
			width        INTEGER,
			height       INTEGER,
			text         TEXT,
			font_name    TEXT,
			font_size    INTEGER,
			color        INTEGER,
			weight       INTEGER,
			border_width INTEGER,
			z            INTEGER,
			created_at   INTEGER,
			deleted_at   INTEGER,
			lww_wall_ts  INTEGER NOT NULL,
			lww_op_seq   INTEGER NOT NULL,
			lww_site_id  TEXT    NOT NULL
		)`,
		// fn_page_text_from_server / fn_page_text_from_client carry per-page recognized
		// text (schema v3). pk == the page ULID (1:1 with fn_page), so re-OCR
		// re-authors the SAME row and converges by LWW. _from_server is authored only
		// by UB (the OCR result pushed down to the device); _from_client is RESERVED
		// for a future on-device-recognition feature — its columns are baked into the
		// v3 hash now so adopting client-authoring needs no second schema bump, but
		// nothing authors it yet. No FK on the page (row-level LWW), no secondary index
		// (the only lookup is by the PK, which IS the page id).
		`CREATE TABLE IF NOT EXISTS fn_page_text_from_server (
			id          TEXT PRIMARY KEY,
			text        TEXT,
			ocr_at      INTEGER,
			model       TEXT,
			created_at  INTEGER,
			deleted_at  INTEGER,
			lww_wall_ts INTEGER NOT NULL,
			lww_op_seq  INTEGER NOT NULL,
			lww_site_id TEXT    NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS fn_page_text_from_client (
			id          TEXT PRIMARY KEY,
			text        TEXT,
			ocr_at      INTEGER,
			model       TEXT,
			created_at  INTEGER,
			deleted_at  INTEGER,
			lww_wall_ts INTEGER NOT NULL,
			lww_op_seq  INTEGER NOT NULL,
			lww_site_id TEXT    NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_fn_page_nb ON fn_page(notebook_id)`,
		`CREATE INDEX IF NOT EXISTS idx_fn_stroke_pg ON fn_stroke(page_id, z)`,
		`CREATE INDEX IF NOT EXISTS idx_fn_text_box_page ON fn_text_box(page_id, z)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("syncstore migrate: %w", err)
		}
	}

	// Mint UB's authoring ULID exactly once. INSERT OR IGNORE keeps the first
	// value forever, so a fresh ULID is generated per startup but only the
	// initial one persists — UB's site_id is stable across restarts.
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO sync_site (id, site_id, last_op_seq) VALUES (1, ?, 0)`,
		newULID()); err != nil {
		return fmt.Errorf("syncstore migrate: seed site: %w", err)
	}

	// Additive v1 columns for DBs created before the folder/template amendment.
	// CREATE TABLE IF NOT EXISTS above is a no-op on an existing table, so the new
	// columns must be added here. Idempotent: ensureColumn skips if already present.
	type addCol struct{ table, col, decl string }
	for _, a := range []addCol{
		{"fn_notebook", "folder_id", "TEXT"},
		{"fn_page", "template", "TEXT"},
		{"fn_page", "template_pitch_mm", "INTEGER"},
		// Phase 8 cutover: the durable op_ts HLC on the authoring-site row.
		{"sync_site", "last_hlc", "INTEGER NOT NULL DEFAULT 0"},
		// Device management: optional human-readable label refreshed on every sync
		// that carries one ('' = the device never sent a name).
		{"sync_cursors", "device_name", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := ensureColumn(ctx, db, a.table, a.col, a.decl); err != nil {
			return fmt.Errorf("syncstore migrate: %w", err)
		}
	}

	// Seed the HLC so it never starts below an op_ts already on the wire: on a DB that predates the
	// column, sync_ops may already hold device wall_ts values while last_hlc defaults to 0. Floor it
	// at the greatest recorded wall_ts so UB's first authored op after the cutover still sorts after
	// everything relayed so far. Runtime ReceiveEvent/LocalEvent maintain the invariant thereafter.
	if _, err := db.ExecContext(ctx,
		`UPDATE sync_site SET last_hlc = MAX(last_hlc, (SELECT COALESCE(MAX(wall_ts), 0) FROM sync_ops)) WHERE id = 1`); err != nil {
		return fmt.Errorf("syncstore migrate: seed last_hlc: %w", err)
	}

	// Index on the notebook→folder edge; created after the column is guaranteed.
	if _, err := db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_fn_notebook_folder ON fn_notebook(folder_id)`); err != nil {
		return fmt.Errorf("syncstore migrate: %w", err)
	}
	return nil
}

// ensureColumn adds col to table if it is not already present, guarded by
// pragma_table_info so it is safe to run on every startup. SQLite has no
// ADD COLUMN IF NOT EXISTS, hence the explicit existence check.
func ensureColumn(ctx context.Context, db *sql.DB, table, col, decl string) error {
	var present int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, col).
		Scan(&present); err != nil {
		return fmt.Errorf("check %s.%s: %w", table, col, err)
	}
	if present > 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, col, decl)); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, col, err)
	}
	return nil
}
