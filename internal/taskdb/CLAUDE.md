# Task Database

Last verified: 2026-05-30 (ForestNote provenance columns + hard-purge path; HardDeleteOlderThan returns purged + skipped under explicit tx)

## Purpose
Opens and migrates the SQLite database used for task storage.
Implements the `caldav.TaskStore` interface for CalDAV and web UI task operations.

## Contracts
- **Exposes**: `Open(ctx, path) (*sql.DB, error)` -- opens/creates SQLite DB, applies migrations, returns pool. `NewStore(db) *Store` -- creates TaskStore implementation.
- **Guarantees**: WAL mode and foreign keys enabled. Schema is idempotent (safe to call on existing DB). MaxOpenConns=1 (SQLite single-writer). Implements every `caldav.TaskStore` method plus the extended `service.TaskStore` surface (`ListIncludingDeleted`, `HardDeleteOlderThan`). Uses `taskstore.ErrNotFound` sentinel for missing tasks.
- **Expects**: Writable filesystem path. Context for cancellation.

## Schema additions (2026-05-29)
- Four nullable TEXT columns on `tasks` carry ForestNote provenance lifted from inbound X-FORESTNOTE-* VTODO properties: `forestnote_notebook_id`, `forestnote_page_id`, `forestnote_notebook_name`, `forestnote_source`. Columns are added via idempotent `pragma_table_info('tasks')`-guarded ALTERs (same pattern as `task_sync_map.last_seen_at`) so the migration is safe on live deployments.
- Partial index `idx_tasks_forestnote_notebook ON tasks(forestnote_notebook_id) WHERE forestnote_notebook_id IS NOT NULL` powers the `?notebook_id=` filter on `/api/v1/tasks` and the `list_tasks` MCP tool. Created **after** the ALTERs (see schema.go comment) — referencing a column inside `stmts[]` would fail on a pre-ForestNote DB.

## Hard-purge contract
- `HardDeleteOlderThan(ctx, cutoffMs) (purged, skipped int64, err error)` is the **only** code path in the repo that issues `DELETE FROM tasks`. Every other "delete" is a soft tombstone (`is_deleted='Y'`). Predicate is `is_deleted = 'Y' AND last_modified < cutoffMs`; caller picks the cutoff. `skipped` counts soft-deleted rows that were inside the safety window (`last_modified >= cutoffMs`) — surfaced so callers can distinguish "0 purged because nothing was eligible" from "0 purged because the gate broke." No VACUUM — space reclamation is left to SQLite's incremental free-page reuse and out-of-band maintenance.
- Both the COUNT-skipped and DELETE-purged statements run inside an explicit `BeginTx` transaction so the two counters can't drift under concurrent writes. Without the wrapper, a Delete or Update landing between the bare-DB COUNT and DELETE could move a row between buckets and leave the caller with inconsistent totals. (SQLite serializes writers, but each `s.db.Exec*` call otherwise gets its own implicit per-statement transaction.) Rollback is deferred unconditionally; Commit's idempotent-Rollback contract makes the post-Commit Rollback a no-op.

## Dependencies
- **Uses**: `modernc.org/sqlite` (pure-Go, no CGO), `taskstore` (Task model, ErrNotFound, mapping helpers)
- **Used by**: `cmd/ultrabridge` (startup), indirectly by `caldav.Backend`, `web.Handler` via `caldav.TaskStore` interface
- **Boundary**: Owns schema DDL and CRUD. Does not own iCal conversion (that's `caldav/vtodo.go`).

## Key Decisions
- Single-user: no user_id column (one SQLite DB per UltraBridge instance)
- Reuses `taskstore.Task` model: no new type, CalDAV/web code unchanged
- Default values match existing `taskstore.Store` behavior (GenerateTaskID, CompletedTime=now, etc.)

## Invariants
- Timestamps are always millisecond UTC unix (0 = unset)
- `completed_time` holds **creation** time (Supernote quirk preserved for compatibility)
- `is_deleted` is "Y" or "N", never NULL
- Soft deletes only: Delete sets is_deleted='Y', never removes rows
- `ical_blob` column exists but is unused until Phase 2
