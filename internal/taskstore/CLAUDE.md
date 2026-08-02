# Task Store

Last verified: 2026-08-02 (task semantics went native: completed_at, updated_at, four-state status)

## Purpose
The `Task` model and the pure mapping helpers around it. Task semantics here are
**native**: RFC 5545 VTODO shapes, not Supernote's. The device's wire format is a
lossy projection applied at the SPC boundary (`internal/spcserver/mapping`), not
the vocabulary this package speaks.

## Contracts
- **Exposes**: `Store` (List, Get, Create, Update, Delete, MaxLastModified), `Task` model, mapping helpers (GenerateTaskID, ComputeETag, ComputeCTag, CalDAVStatus, FromCalDAVStatus, MsToTime, TimeToMs, SqlStr, NullStr, CompletionTime) and the four `Status*` constants
- **Guarantees**: All queries scoped to a single user_id. List/Get exclude soft-deleted rows. Create sets defaults for missing fields. Update always bumps updated_at (and mirrors it into last_modified). `completed_at` is caller-owned and never rewritten by the store. Delete is soft-delete.
- **Expects**: Valid `*sql.DB` connected to supernotedb. Single user_id from `db.DiscoverUserID`.

## Dependencies
- **Uses**: `database/sql` only (no other internal packages)
- **Used by**: `caldav.Backend`, `web.Handler` (both via `caldav.TaskStore` interface)
- **Boundary**: No HTTP, no iCal -- pure data layer

## Key Decisions
- Sort columns omitted: Task model skips 8 sort columns; device repopulates on sync
- MD5 task IDs: `GenerateTaskID` uses MD5(title+timestamp) to match device convention
- ETag from mutable fields: title + status + due_time + **updated_at**. updated_at is the only field guaranteed to move on every write, so this also covers columns not named in the hash (detail, importance, ical_blob).
- Status is stored in lowerCamelCase across all four RFC 5545 states (`needsAction`, `inProcess`, `completed`, `cancelled`). RFC casing was rejected because the REST/MCP JSON already exposed the lowerCamelCase forms; widening is additive, recasing would break clients.

## Invariants
- Timestamps are always millisecond UTC unix (0 = unset)
- `completed_at` holds the real completion instant, NULL unless status is `completed`
- `updated_at` is the store-owned write watermark: the ETag input and the SPC sync token
- `completed_time` still holds **creation** time and `last_modified` is now only an SPC-facing mirror of updated_at. Neither is the completion time. They persist because the device wire format needs them (completed_time feeds MD5 id generation; a zero lastModified hides the task on-device)
- `is_deleted` is "Y" or "N", never NULL
- `is_reminder_on` defaults to "N"
- `status` is one of the four `Status*` constants. Supernote's two states are a projection, produced by `spcserver/mapping.StatusToSPC`
- `ical_blob` (ICalBlob field) is optional and NULL for tasks from Supernote; populated by CalDAV write path for round-trip VTODO fidelity
- `CreatedAt` (int64 ms UTC) and the four `ForestNote*` nullable strings are **taskdb-only** — they mirror columns that only exist in UB's `tasks` table. The SPC mapping layer (`internal/spcserver/mapping`) leaves them at zero / `sql.NullString{}` and the device-side `t_schedule_task` schema is unchanged.

## Gotchas
- `ErrNotFound` sentinel: use `taskstore.IsNotFound(err)` to check, not type assertion
- CompletionTime reads `completed_at`. It formerly read last_modified, per the Supernote convention documented in `docs/PRIVATE_CLOUD_REFERENCE.md` — a convention that only holds for a client that never edits a task after completing it. UB does, so completion needed its own column
- `FromCalDAVStatus` was named `SupernoteStatus`. It runs on the **CalDAV** inbound path; the old name (and its two-value range) is how the vendor's model came to govern UB's own store
- The REST API's `created_at` field is sourced from `Task.CreatedAt`; prior to 2026-05-29 it was mis-mapped from `DueTime`. Code reading the old field via `mapInternalTask` must use the new column.
