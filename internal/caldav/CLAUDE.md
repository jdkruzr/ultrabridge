# CalDAV Backend

Last verified: 2026-08-02 (COMPLETED sourced from completed_at; STATUS carries all four VTODO states)

## Purpose
Exposes tasks as a standard CalDAV VTODO collection so any CalDAV client
(Apple Reminders, Thunderbird, DAVx5, 2Do, Tasks.org) can read and write tasks.

## Contracts
- **Exposes**: `Backend` (implements `gocaldav.Backend`), `TaskStore` interface, `SyncNotifier` interface, `ProppatchStub` http middleware. Plus the metadata helpers: `BlobMetadata`, `ParseBlobMetadata(blob) BlobMetadata`, `BuildBlobWithMetadata(taskID, meta) string`, `BlobMetadataPatch`, `MergeBlobMetadataPatch(taskID, existingBlob, patch) string`.
- **Guarantees**: Single fixed collection at `{prefix}/tasks/`. Only VTODO supported (VEVENT rejected). Writes trigger sync notification (graceful degradation if notifier down). ETags computed from mutable fields (via `updated_at`). All four RFC 5545 STATUS values round-trip (`NEEDS-ACTION`, `IN-PROCESS`, `COMPLETED`, `CANCELLED`); `COMPLETED` is sourced from and written to `completed_at`, while `LAST-MODIFIED`/`DTSTAMP` track `updated_at`. Collection display name is mutable at runtime via client PROPPATCH of `DAV:displayname`. `VTODOToTask` populates the four `Task.ForestNote*` columns from `X-FORESTNOTE-*` properties via `extractForestNoteMetadata` (defensive: missing props leave the columns NULL). `ParseBlobMetadata` and the merge helpers never error — corrupt/blank blobs degrade to zero-value output.
- **Expects**: A `TaskStore` implementation and a `SyncNotifier`. Caller sets HTTP prefix. Caller wraps the CalDAV `http.Handler` with `ProppatchStub` if PROPPATCH acceptance is desired (it is — see Gotchas).

## Dependencies
- **Uses**: `taskstore` (via `TaskStore` interface), `sync` (via `SyncNotifier` interface)
- **Used by**: `cmd/ultrabridge` (HTTP mount), `web` (reuses `TaskStore` and `SyncNotifier` interfaces)
- **Boundary**: Does not import `config`, `db`, `auth`, or `logging`

## Key Decisions
- Single collection: one flat task list, no sub-calendars
- TaskStore as interface: decouples from SQL, enables test doubles
- DueTimeMode config: "preserve" keeps time component, "date_only" strips it
- iCal blob overlay: TaskToVTODO checks ICalBlob first; if present, deserializes and overlays DB-authoritative fields (UID, SUMMARY, STATUS, DUE, DTSTAMP, LAST-MODIFIED, PERCENT-COMPLETE). Falls back to field-only build on corrupt blob.
- VTODOToTask serializes the full iCal calendar to ICalBlob for round-trip fidelity of non-modeled properties (DESCRIPTION, PRIORITY, RRULE, X-props, etc.)
- `ProppatchStub` HTTP middleware intercepts PROPPATCH *before* the go-webdav/caldav handler because that library hard-codes `PropPatch → 501` in its internal backend wrapper (`caldav/server.go:~664`), and that path is not overridable via the public `Backend` interface. Stub emits 207 Multi-Status with HTTP/1.1 200 OK per requested property, and fires an `OnDisplayName` callback for `DAV:displayname` so clients can rename the collection. Callback persists to the settings DB and updates the backend's in-memory `collectionName` via `SetCollectionName` (RWMutex-guarded), so subsequent PROPFINDs reflect the new name without a container restart.
- Collection name is editable from three surfaces that stay in sync: Settings UI (`caldav_collection_name` field), `PUT /api/config`, and CalDAV PROPPATCH of `DAV:displayname`.
- **Blob-only metadata split** (2026-05-29): `CATEGORIES`, `COMMENT`, and `X-FORESTNOTE-NATIVE-URL` deliberately have no structured columns and ride in `ical_blob`. The service layer parses them on read via `ParseBlobMetadata`. `X-FORESTNOTE-{NOTEBOOK-ID,PAGE-ID,NOTEBOOK-NAME,SOURCE}` *are* lifted into columns (for indexed filter queries) but the blob still carries the raw bytes — column writes and blob writes both happen.
- **`BuildBlobWithMetadata` deliberately omits `SUMMARY`**: the blob-overlay path (`taskToVTODOFromBlob`) injects the live Title column at serve time. Emitting an empty `SUMMARY:` would round-trip through strict CalDAV clients as a malformed VTODO (RFC 5545 §3.6.2 disallows empty TEXT SUMMARY).
- **Patch-shaped merge**: `MergeBlobMetadataPatch` uses pointer fields + explicit `Clear*` flags so the merge can distinguish "leave alone" from "set to empty"; this is load-bearing for partial PATCH on the REST API and prevents silent loss of `CATEGORIES` / `COMMENT` when a partially-corrupt blob slips through `ParseBlobMetadata`'s zero-value fallback.
- **CATEGORIES uses `TextList()`**, not `Text()` — `Text()` truncates at the first list-separator comma; `TextList()` does the §3.3.11 unescape-and-split correctly.

## Invariants
- UID in VTODO maps 1:1 to task_id in DB
- Calendar object paths are `{prefix}/tasks/{task_id}.ics`
- PutCalendarObject is upsert: creates if missing, updates if exists
- Delete is soft-delete (delegates to store)
- DB fields always win over blob fields on read (blob is supplementary)
- `extractForestNoteMetadata` runs on every `VTODOToTask`; tasks that never carried X-FORESTNOTE-* see the four columns as `sql.NullString{}` (not empty string) so `WHERE forestnote_notebook_id IS NOT NULL` is the canonical filter.

## Gotchas
- QueryCalendarObjects does basic VTODO filter only; no date-range filtering
- Notify errors are swallowed (logged, not returned) to avoid failing DB writes
- Corrupt iCal blobs silently fall back to field-only rendering (logged at warn level)
- Without `ProppatchStub`, the go-webdav library returns 501 for every PROPPATCH — clients like 2Do, Tasks.org, and Apple Reminders surface this as "unimplemented" and abort sync entirely, even though VTODO PUT/REPORT works fine. The stub is load-bearing for usable client compatibility, not just a nicety.
- `ProppatchStub` responds 200 OK for properties it does not persist (e.g. Apple's `calendar-color`). This is the common server behavior (Radicale, SabreDAV) — honest 403s tend to cause client retry loops.

## Testing & Debugging Notes

### Recommended CalDAV clients for debugging, in order

1. **`curl` against the server directly** — the `.ics` body at `/caldav/user/calendars/tasks/{task_id}.ics` is the authoritative server truth. If a client claims a task is in the wrong state, `curl` first.
2. **DAVx5 on Android** — visible sync state, manual "sync now" button, verbose logs.
3. **Thunderbird Lightning** — tight manual-sync feedback loop; DevTools-equivalent request logging.
4. **Apple Reminders on iOS/macOS** — good for confirming real-world Apple client behavior, but sync cadence is opaque.

### Clients that are ACTIVELY misleading for debugging

- **2Do on macOS**: has a multi-minute background polling interval and no visible indicator of when it last pulled. "I changed X on the web and it didn't appear in 2Do" is almost always just 2Do not having polled yet — hitting its manual Sync button will pull. Do not use 2Do Mac as the downstream observer when verifying that a server-side mutation propagated; you will misread polling latency as a server bug.

### CalDAV sync is intrinsically pull-based

There is no RFC-standard way for UltraBridge to push "something changed" to a CalDAV client. The server's `STARTSYNC` socket.io event is a UB-internal mechanism for the device-pipeline side, not something any CalDAV client speaks. Any real-time-feeling behavior in a CalDAV client is just that client polling aggressively (DAVx5 push sync, Apple Reminders' short polling interval, etc.).
