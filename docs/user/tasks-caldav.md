# Tasks, CalDAV, And Attachments

UltraBridge stores tasks in SQLite and exposes them through the web UI, JSON API, MCP tools, CalDAV, and supported device sync surfaces.

## CalDAV URLs

Use discovery when your client supports it:

```text
https://your-host/.well-known/caldav
```

Direct collection URL:

```text
https://your-host/caldav/user/calendars/tasks/
```

Individual tasks live at `.../tasks/{task_id}.ics`. Use your UltraBridge username/password; bearer tokens (the same tokens issued under **Settings -> Integrations**) are accepted too.

## Incremental Sync

Clients no longer re-download the whole task list on every sync. UltraBridge serves the RFC 6578 `sync-collection` REPORT along with `DAV:sync-token`, `CS:getctag`, and `DAV:supported-report-set`. An empty token returns the current set plus a resume token; a subsequent token returns only what changed, with deletions reported as removals. `getctag` carries the same value as the sync token for clients that check it first. After a hard purge, older tokens are rejected with `DAV:valid-sync-token` and the client resyncs in full once.

CalDAV remains pull-based — anything that feels real-time is the client polling.

## Supported Task Fields

UltraBridge maps the practical task fields used across its integrations:

- Title
- Description/detail/comment
- Status — all four RFC 5545 values (`NEEDS-ACTION`, `IN-PROCESS`, `COMPLETED`, `CANCELLED`) round-trip; a running timer or a cancelled task survives a sync
- Completion time — stored in its own field, so editing a completed task no longer rewrites when it was completed
- Due date
- Priority
- Categories
- URL/native links
- Signed attachments
- ForestNote provenance (`X-FORESTNOTE-*`): which notebook and page a task came from, preserved on the VTODO and filterable via the API and MCP tools

Recurrence rules, alarms, parent/child links, and vendor `X-` properties are not interpreted by UltraBridge, but they are stored verbatim and round-tripped — including across writes from a device that does not understand them.

## Deleting And Purging

Deletion is two-stage:

1. **Delete** tombstones the task. CalDAV clients see the removal on their next sync; the Tasks tab's "Show deleted" view lists tombstones, and the API/MCP surface them with `include_deleted`.
2. **Purge** is the irreversible step. `purge_completed_tasks` soft-deletes every completed task; `purge_deleted_tasks` (default cutoff 30 days) permanently removes already-tombstoned rows and is the only operation that actually frees database rows.

## Attachments

Task attachments are stored internally and exposed as signed URLs from UltraBridge. This is used for ForestNote-rendered task context and generic CalDAV attachment flows. Attachment URLs are served without an auth header on purpose, so third-party CalDAV clients can fetch them — the URL signature is the guard. Inline binary attachments are stored out-of-band and reconstructed byte-for-byte when a client fetches the task.

If a client does not show attachments:

1. Check the task in UltraBridge or MCP output.
2. Fetch the CalDAV object and confirm it contains an `ATTACH` URI.
3. Fetch the attachment URL and confirm it returns `200`.
4. Remember that some task clients ignore `ATTACH` even when calendars support it.

## MCP Task Tools

MCP task mutations use the same service layer as the API and web UI. Creates, updates, completions, and deletions sync through the configured downstream surfaces; deletion is a tombstone, which is how CalDAV clients learn the task is gone. Hard purge is local and irreversible, and invalidates outstanding CalDAV sync tokens so clients resync in full.

Every mutation is audit-logged with the auth method and label where available.
