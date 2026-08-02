package taskdb

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
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

	// Idempotent ALTER for `completed_at` — the real completion instant, added
	// 2026-08-02 when task timestamps moved to native semantics. Same
	// pragma_table_info guard as the ForestNote columns above. Nullable with no
	// default: NULL means "not completed", which is distinguishable from the 0
	// that `last_modified` used to carry for never-synced rows.
	var completedAtCol int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name='completed_at'`).Scan(&completedAtCol)
	if completedAtCol == 0 {
		if _, err := db.ExecContext(ctx,
			`ALTER TABLE tasks ADD COLUMN completed_at INTEGER`); err != nil {
			return fmt.Errorf("add completed_at column: %w", err)
		}
		total, suspect, err := backfillCompletedAt(ctx, db)
		if err != nil {
			return err
		}
		if total > 0 {
			slog.Info("backfilled completed_at from last_modified",
				"rows", total,
				"suspect", suspect,
				"note", "suspect rows were last written by Update, so their completion time may be an edit time")
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

// backfillCompletedAt seeds the new completed_at column from last_modified for
// already-completed tasks. Runs exactly once, immediately after the ALTER that
// creates the column.
//
// last_modified is the best available source: under the Supernote convention it
// held the completion instant. But UB's Update stamped it on *every* write, so
// for any task edited after it was completed the stored value is the edit time,
// not the completion time — and the true value is unrecoverable.
//
// Rather than guess, we report. A row whose last_modified exactly equals its
// updated_at was written by Update (both are stamped from the same `now`), so
// its backfilled completion time is suspect. A row where they differ was last
// touched by Create, which preserves the caller's value, so it is trustworthy.
// Returns the number of rows backfilled and how many of those are suspect;
// the caller logs. Nothing is skipped or altered on the strength of the
// heuristic — it only informs the operator.
func backfillCompletedAt(ctx context.Context, db *sql.DB) (total, suspect int64, err error) {
	const eligible = `status = 'completed' AND completed_at IS NULL AND last_modified > 0`

	if err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks WHERE `+eligible).Scan(&total); err != nil {
		return 0, 0, fmt.Errorf("count completed_at backfill candidates: %w", err)
	}
	if err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks WHERE `+eligible+` AND last_modified = updated_at`).Scan(&suspect); err != nil {
		return 0, 0, fmt.Errorf("count suspect completed_at candidates: %w", err)
	}

	if _, err = db.ExecContext(ctx,
		`UPDATE tasks SET completed_at = last_modified WHERE `+eligible); err != nil {
		return 0, 0, fmt.Errorf("backfill completed_at: %w", err)
	}
	return total, suspect, nil
}
