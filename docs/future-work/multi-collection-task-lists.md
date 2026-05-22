# Future build session — multi-collection task lists (multiple SPC task groups)

**Status:** Deferred by decision (2026-05-22), during UB-as-SPC Phase 1 planning. Not part of the 6-phase UB-as-SPC refactor; its own session afterward.

## Why deferred

SPC has first-class task **groups** (task lists): `/api/file/schedule/group/*` with `taskListId`/`title`/`lastModified`. UB's task model is **single-collection**: `taskdb` has only a `tasks` table (no `task_lists`), and the CalDAV backend (`internal/caldav`) exposes exactly one collection (`CalDAVCollectionName`, default "Tasks"). UB-as-SPC Phase 1 (sub-phase 1d) therefore models groups as **one synthesized group** (Option A): `/group/all` returns a single list, all tasks belong to it, device group create/update/delete are accepted as no-op success.

The user wants genuine multi-collection support eventually — preserving multiple device-created task lists through UB and out to CalDAV clients — but agreed it should not block Phase 1.

## What "done" looks like

1. The Supernote device can have N task lists; each round-trips through UB and appears as a distinct list to CalDAV clients (separate CalDAV collections) and in UB's web UI.
2. Device group CRUD (`POST/PUT/DELETE /schedule/group`, `/group/clear`) persists.
3. Existing single-list installs migrate cleanly (the current sole list becomes the default collection).

## Seams already in place (built in Phase 1 to make this swap-in clean)

- `internal/spcserver/groups.GroupProvider` — the schedule handlers depend on this interface, not a hardcoded single group. Phase 1 ships a single-group impl; this session adds a DB-backed impl.
- Tasks already carry `task_list_id` (`taskstore.Task.TaskListID`); today it defaults to the single group.

## Scope to touch (investigate fresh — these are pointers, verify before relying)

- **`internal/taskdb`** — add a `task_lists` table (taskListId, title, lastModified, isDeleted) + migration; CRUD.
- **`internal/taskstore`** — group-aware queries; `Task.TaskListID` becomes meaningful.
- **`internal/caldav`** — the big one: go from one collection to **multiple collections** (one per task list). go-webdav CalDAV multi-collection support; collection discovery/PROPFIND; mapping list↔collection. This is the largest and least-understood piece — scope it first.
- **`internal/spcserver/handlers/schedule.go`** + `groups` — DB-backed `GroupProvider`; real group CRUD instead of no-ops.
- **`internal/web`** — task UI surfacing multiple lists.

## Starting points

- Read this doc, `docs/implementation-plans/spc-phase-1.md` (1d "Decision — single synthesized task group" + the `GroupProvider` seam), and `docs/spc-protocol.md` §5/§8 (schedule group endpoints + DTO field names).
- Memory: `project_ub_multicollection_future`.
- The hardest unknown is CalDAV multi-collection; spike that before committing to a plan.
