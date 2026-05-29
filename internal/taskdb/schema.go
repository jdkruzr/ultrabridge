package taskdb

import (
	"context"
	"database/sql"
	"fmt"
)

func migrate(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS tasks (
			task_id        TEXT PRIMARY KEY,
			title          TEXT,
			detail         TEXT,
			status         TEXT NOT NULL DEFAULT 'needsAction',
			importance     TEXT,
			due_time       INTEGER NOT NULL DEFAULT 0,
			completed_time INTEGER NOT NULL DEFAULT 0,
			last_modified  INTEGER NOT NULL DEFAULT 0,
			recurrence     TEXT,
			is_reminder_on TEXT NOT NULL DEFAULT 'N',
			links          TEXT,
			is_deleted     TEXT NOT NULL DEFAULT 'N',
			ical_blob      TEXT,
			created_at     INTEGER NOT NULL,
			updated_at     INTEGER NOT NULL,
			-- ForestNote provenance, extracted from X-FORESTNOTE-* on inbound VTODOs.
			-- Indexed (idx_tasks_forestnote_notebook) for "tasks from notebook X" filters.
			forestnote_notebook_id    TEXT,
			forestnote_page_id        TEXT,
			forestnote_notebook_name  TEXT,
			forestnote_source         TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS sync_state (
			adapter_id      TEXT PRIMARY KEY,
			last_sync_token TEXT,
			last_sync_at    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS task_sync_map (
			task_id     TEXT NOT NULL REFERENCES tasks(task_id),
			adapter_id  TEXT NOT NULL,
			remote_id   TEXT NOT NULL,
			remote_etag TEXT,
			last_pushed_at  INTEGER NOT NULL DEFAULT 0,
			last_pulled_at  INTEGER NOT NULL DEFAULT 0,
			last_seen_at    INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (task_id, adapter_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_task_sync_map_remote ON task_sync_map(adapter_id, remote_id)`,
		// NOTE: The partial index on tasks.forestnote_notebook_id is created AFTER the
		// idempotent ALTERs below — on a pre-ForestNote DB the column doesn't exist yet
		// when this slice runs, so an index referencing it would fail.
	}
	for i, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration statement %d: %w", i, err)
		}
	}

	// Idempotent ALTER for existing DBs that pre-date last_seen_at.
	var count int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('task_sync_map') WHERE name='last_seen_at'`).Scan(&count)
	if count == 0 {
		if _, err := db.ExecContext(ctx, `ALTER TABLE task_sync_map ADD COLUMN last_seen_at INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add last_seen_at column: %w", err)
		}
	}

	// Idempotent ALTERs for the four ForestNote provenance columns. Existing live deployments
	// pre-date these — added 2026-05-29 alongside FN-side X-FORESTNOTE-* emission. The columns
	// are nullable with no default; pre-existing rows stay NULL until a fresh PUT overwrites them.
	for _, col := range []string{
		"forestnote_notebook_id",
		"forestnote_page_id",
		"forestnote_notebook_name",
		"forestnote_source",
	} {
		var c int
		_ = db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name=?`, col).Scan(&c)
		if c == 0 {
			if _, err := db.ExecContext(ctx,
				fmt.Sprintf(`ALTER TABLE tasks ADD COLUMN %s TEXT`, col)); err != nil {
				return fmt.Errorf("add %s column: %w", col, err)
			}
		}
	}

	// Partial index on the now-guaranteed-to-exist column. Only rows with a ForestNote origin
	// carry the value, so this stays cheap even on the SPC-dominated row population. Powers the
	// "list_tasks ?notebook_id=…" filter. Must run AFTER the ALTERs above (see note in stmts).
	if _, err := db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_tasks_forestnote_notebook
			ON tasks(forestnote_notebook_id) WHERE forestnote_notebook_id IS NOT NULL`); err != nil {
		return fmt.Errorf("create idx_tasks_forestnote_notebook: %w", err)
	}

	return nil
}
