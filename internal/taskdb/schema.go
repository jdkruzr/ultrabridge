package taskdb

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"time"
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
		fromBlob, fromWatermark, err := backfillCompletedAt(ctx, db)
		if err != nil {
			return err
		}
		if fromBlob+fromWatermark > 0 {
			slog.Info("backfilled completed_at",
				"exact_from_ical_blob", fromBlob,
				"approximate_from_last_modified", fromWatermark,
				"note", "approximate rows have no client-supplied COMPLETED to recover; their value is the last write while completed, which may be an edit rather than the completion")
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
// There are two possible sources, and they are not equally good:
//
//  1. The COMPLETED property inside the stored ical_blob. This is what the
//     CalDAV client actually sent, untouched by UB's write path, so it is the
//     true completion instant. Preferred wherever present.
//  2. last_modified. Under the Supernote convention this held the completion
//     time, but UB's Update stamped it on every write, so for any task edited
//     after completion it is the edit time. Approximate, and unrecoverable
//     where it has drifted.
//
// The gap between the two is not hypothetical: on a production database, 44 of
// the 52 completed tasks carrying a blob COMPLETED disagreed with last_modified,
// several by weeks. Reading the blob first recovers those.
//
// Returns how many rows were backfilled from each source; the caller logs.
func backfillCompletedAt(ctx context.Context, db *sql.DB) (fromBlob, fromWatermark int64, err error) {
	rows, err := db.QueryContext(ctx,
		`SELECT task_id, last_modified, ical_blob FROM tasks
		 WHERE status = 'completed' AND completed_at IS NULL`)
	if err != nil {
		return 0, 0, fmt.Errorf("select completed_at backfill candidates: %w", err)
	}
	type seed struct {
		id string
		at int64
	}
	var seeds []seed
	for rows.Next() {
		var id string
		var lastModified sql.NullInt64
		var blob sql.NullString
		if err := rows.Scan(&id, &lastModified, &blob); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("scan backfill candidate: %w", err)
		}
		if at, ok := completedFromBlob(blob.String); ok {
			seeds = append(seeds, seed{id, at})
			fromBlob++
		} else if lastModified.Valid && lastModified.Int64 > 0 {
			seeds = append(seeds, seed{id, lastModified.Int64})
			fromWatermark++
		}
		// Neither source available → leave NULL rather than invent an instant.
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, fmt.Errorf("iterate backfill candidates: %w", err)
	}
	rows.Close()

	for _, s := range seeds {
		if _, err := db.ExecContext(ctx,
			`UPDATE tasks SET completed_at = ? WHERE task_id = ?`, s.at, s.id); err != nil {
			return 0, 0, fmt.Errorf("backfill completed_at for %s: %w", s.id, err)
		}
	}
	return fromBlob, fromWatermark, nil
}

// completedRE matches the COMPLETED property line in a stored VCALENDAR. Read
// with a regex rather than the iCal decoder so the storage layer doesn't take a
// dependency on the CalDAV package for one migration; any shape it doesn't
// recognise simply falls through to the last_modified path.
var completedRE = regexp.MustCompile(`(?mi)^COMPLETED[^:\r\n]*:\s*([0-9TZ]+)`)

// completedFromBlob extracts the client-supplied COMPLETED instant from a
// stored VCALENDAR, in ms UTC.
func completedFromBlob(blob string) (int64, bool) {
	if blob == "" {
		return 0, false
	}
	m := completedRE.FindStringSubmatch(blob)
	if m == nil {
		return 0, false
	}
	// RFC 5545 DATE-TIME: UTC ("...Z") or, less commonly, floating local time.
	// Floating is read as UTC — the same assumption the rest of the task path
	// makes for timestamps without a zone.
	for _, layout := range []string{"20060102T150405Z", "20060102T150405"} {
		if ts, err := time.Parse(layout, m[1]); err == nil {
			return ts.UTC().UnixMilli(), true
		}
	}
	return 0, false
}
