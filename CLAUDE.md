# UltraBridge

Last verified: 2026-03-20

Go sidecar service for Supernote Private Cloud. Two subsystems:
1. **CalDAV task sync** -- reads/writes the Supernote MariaDB, pushes STARTSYNC via Engine.IO
2. **Notes pipeline** -- scans .note files, extracts/OCRs text, indexes for full-text search

## Bash Commands: No `cd &&` Compounds

**NEVER** use `cd /path && command` compound bash statements. This triggers a Claude Code bug where the permission prompt fires on `cd` instead of the actual command.

Instead: `git -C /path`, `go -C /path build`, or absolute paths.

## Project Structure

- `cmd/ultrabridge/` -- entry point, wires all components
- `internal/caldav/` -- CalDAV backend (go-webdav), VTODO conversion (see domain CLAUDE.md)
- `internal/taskstore/` -- task CRUD against t_schedule_task, field mapping (see domain CLAUDE.md)
- `internal/sync/` -- Engine.IO v3 notifier: STARTSYNC push + inbound events (see domain CLAUDE.md)
- `internal/auth/` -- Basic Auth middleware (bcrypt)
- `internal/config/` -- env vars (UB_ prefix) + .dbenv file loading + pipeline config
- `internal/db/` -- MariaDB pool + single-user discovery
- `internal/logging/` -- structured slog, file rotation, syslog, WebSocket broadcast
- `internal/web/` -- HTML UI: task list, Files tab, Search tab, processor C&C, SSE log stream
- `internal/notedb/` -- SQLite DB opener + schema migrations for notes pipeline (see domain CLAUDE.md)
- `internal/notestore/` -- file inventory (scan, list, get) against SQLite notes table (see domain CLAUDE.md)
- `internal/processor/` -- background OCR job queue: backup, extract, render, OCR, inject, SPC catalog sync (see domain CLAUDE.md)
- `internal/search/` -- FTS5 full-text search over note content (see domain CLAUDE.md)
- `internal/pipeline/` -- file detection: fsnotify watcher, reconciler, Engine.IO listener (see domain CLAUDE.md)
- `tests/` -- integration tests (require real DB)

## Build & Test

```bash
go build -C /home/sysop/src/ultrabridge/.worktrees/spc-catalog-sync ./cmd/ultrabridge/
go test -C /home/sysop/src/ultrabridge/.worktrees/spc-catalog-sync ./...
go vet -C /home/sysop/src/ultrabridge/.worktrees/spc-catalog-sync ./...
```

## Conventions

- Module: `github.com/sysop/ultrabridge`
- Config: all env vars prefixed `UB_`, DB creds from shared `.dbenv` file
- Auth: single-user Basic Auth, password stored as bcrypt hash

### CalDAV Subsystem (MariaDB)
- DB timestamps: millisecond UTC unix timestamps, 0 = unset
- IDs: MD5(title + timestamp) for task IDs (matches Supernote device convention)
- Supernote quirk: `completed_time` holds creation time; `last_modified` holds actual completion time
- Soft deletes only: `is_deleted = 'Y'`, never hard delete

### Notes Pipeline (SQLite + MariaDB catalog sync)
- Two databases: MariaDB for tasks, SQLite for notes pipeline (separate concerns)
- After OCR injection, processor updates SPC MariaDB catalog (f_user_file, f_file_action, f_capacity) so the device sees correct file size/md5 -- best-effort, failures logged not propagated
- SQLite in WAL mode, MaxOpenConns=1 (single-writer)
- Job statuses: pending -> in_progress -> done|failed|skipped
- Backup before modification: original .note copied to backup tree, never overwritten
- OCR source tracking: "myScript" (device RECOGNTEXT) vs "api" (vision API result)
- Pipeline env vars: UB_NOTES_PATH, UB_DB_PATH, UB_BACKUP_PATH, UB_OCR_*
