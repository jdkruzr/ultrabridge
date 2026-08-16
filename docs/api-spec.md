# UltraBridge API Specification (v1)

This document defines the API contract for the UltraBridge headless platform. All routes sit behind the same authentication as the web UI (Basic auth or an MCP bearer token), except the signed attachment routes noted below. Every mutation is audit-logged with the caller's auth method and label.

## Core Entities

### Task

```json
{
  "id": "string",
  "title": "string",
  "status": "needsAction | inProcess | completed | cancelled",
  "created_at": "ISO8601 string",
  "due_at": "ISO8601 string | null",
  "completed_at": "ISO8601 string (present only when status is completed)",
  "detail": "string | null",
  "url": "string | null",
  "priority": "string | null",
  "categories": ["string"],
  "comment": "string | null",
  "deleted": false,
  "forestnote": {
    "notebook_id": "string",
    "page_id": "string",
    "notebook_name": "string",
    "source": "string",
    "native_url": "string"
  },
  "attachments": [
    {"url": "string", "fmt_type": "string", "filename": "string", "size": 1024, "inline": false}
  ],
  "links": {}
}
```

Notes: `forestnote` is present only for tasks with ForestNote provenance (`X-FORESTNOTE-*` on the inbound VTODO). `links` is reserved and never populated. The `status` values above are the JSON emission forms; the `?status=` query filter uses snake_case (`needs_action`, `in_process`, `completed`, `cancelled`, `all`).

### NoteFile

Represents a Supernote or Boox file. ForestNote and reMarkable inventories are not served by `/api/v1/files` — reMarkable has its own `/api/v1/remarkable/documents`.

```json
{
  "name": "string",
  "path": "string",
  "rel_path": "string",
  "is_dir": false,
  "file_type": "note | pdf | epub | other",
  "size_bytes": 1024,
  "created_at": "ISO8601 string",
  "modified_at": "ISO8601 string",
  "source": "supernote | boox",
  "device_info": "string | null",
  "job_status": "pending | in_progress | done | failed | skipped",
  "last_error": "string | null"
}
```

### EmbeddingJob

Background-processing snapshot (OCR and vector embeddings), returned by `GET /api/v1/status` as `{"jobs": ...}`. The top-level counters are Supernote's. Optional per-source blocks `boox`, `forestnote`, and `remarkable` are omitted entirely when that source isn't configured — **gate on key presence, not zero values**. ForestNote's durable "done" figure is `forestnote.indexed`; its `processed` counter resets on restart.

```json
{
  "running": true,
  "pending_count": 5,
  "in_flight_count": 1,
  "processed_count": 120,
  "failed_count": 2,
  "active_task": {"path": "string", "started_at": "ISO8601 string"},
  "boox": {"...": "optional"},
  "forestnote": {"pending": 0, "in_flight": 0, "processed": 0, "dropped": 0, "capacity": 0, "indexed": 0},
  "remarkable": {"pending": 0, "in_progress": 0, "done": 0, "failed": 0}
}
```

### Configuration

`GET /api/v1/config` returns a **flat** redacted map (secrets shown as `"[set]"`/`"[not set]"`): `username`, `password_hash`, `ocr_enabled`, `ocr_api_url`, `ocr_api_key`, `ocr_model`, `ocr_concurrency`, `ocr_max_file_mb`, `ocr_format`, `embed_enabled`, `ollama_url`, `ollama_embed_model`, `chat_enabled`, `chat_api_url`, `chat_model`, `log_*`, `caldav_collection_name`, `due_time_mode`, `web_enabled`. `PUT /api/v1/config` accepts the same keys and reports which changed and whether a restart is required.

Source rows are managed separately at `GET/POST /api/sources` and `PUT/DELETE /api/sources/{id}` (unversioned). Each source row carries a derived, read-only `sync_model`.

## Endpoints

### Tasks

- `GET /api/v1/tasks` - List tasks. Filters: `status=needs_action|in_process|completed|cancelled|all` (default all); `due_before`/`due_after` (RFC3339 — tasks with no due date are excluded when either is set); `notebook_id`, `notebook_name`, `source` (ForestNote provenance); `category` (single CATEGORIES entry, case-sensitive equality); `priority` (`"1"`-`"9"`); `include_deleted` (`1/true/yes/on` — includes tombstoned rows, flagged `deleted`).
- `GET /api/v1/tasks/{id}` - Fetch a single task; 404 on unknown id.
- `POST /api/v1/tasks` - Create. Body: `{title, due_at?, detail?, url?, priority?, categories?, comment?}`. Returns `201` with the created task; empty title is a 400.
- `PATCH /api/v1/tasks/{id}` - Partial update. Body: `{title?, due_at?, clear_due_at?, detail?, url?, clear_url?, priority?, clear_priority?, categories?, comment?, clear_comment?}`. `clear_*` flags win over the paired value; `categories` is wholesale (`[]` clears, omit = unchanged). Returns 200 with the updated task.
- `POST /api/v1/tasks/{id}/complete` - Mark completed. Returns 204.
- `DELETE /api/v1/tasks/{id}` - Soft-delete (tombstone). Returns 204.
- `POST /api/v1/tasks/purge-completed` - Soft-delete every completed task. Returns `200` with `{"deleted": N}`.
- `POST /api/v1/tasks/purge-deleted?older_than_days=N` - Hard-purge soft-deleted rows older than the cutoff (default 30; `N <= 0` is a 400). Returns `200` with `{"deleted": N, "skipped": M}` — `skipped` counts tombstones still inside the safety window. This is the only endpoint that permanently removes rows, and it invalidates outstanding CalDAV sync tokens (clients resync in full once).
- `POST /api/v1/tasks/bulk` - Bulk operations. Body: `{action: "complete"|"delete", ids: [...]}`. Returns 204; invalid action is a 400.

All task mutations flow through the service layer; changes propagate to CalDAV clients and device sync surfaces on their next pull. Device-originated writes are merged read-modify-write at the device boundary, so fields the device does not model (recurrence, alarms, X-props, ForestNote provenance) survive a device edit.

### Files

- `GET /api/v1/files?path=&sort=&order=` - List files (Supernote/Boox tree; sorted, unpaginated)
- `POST /api/v1/files/scan` - Trigger filesystem scan
- `POST /api/v1/files/queue` - Enqueue file for processing
- `GET /api/v1/files/content?path={path}` - Get OCR text and page metadata
- `GET /api/v1/files/render?path={path}&page={n}` - Get page image

### Search & Chat

- `GET /api/v1/search?q={query}` - Hybrid keyword+vector search. `q` required (400 if empty). Optional: repeated `source` (`supernote|boox|forestnote|remarkable|digest`), `folder`, repeated `location` (opaque facet tokens), `device_model` (deprecated alias `device`), `created_from`/`created_to`, `modified_from`/`modified_to` (deprecated aliases `date_from`/`date_to`, `from`/`to`), `sort` (`date_desc` default, `date_asc`, `relevance`), `mode` (`hybrid` default, `keyword`), `limit` (default 20, clamped to 100).
- `POST /api/v1/chat/ask` - Conversational RAG interface (SSE stream)

### reMarkable

Device registry:

- `GET /api/v1/remarkable/devices` - List paired tablets.
- `PATCH /api/v1/remarkable/devices/{id}` - Set the operator label. Body `{"label": "..."}`; `""` clears, omitted field is a 400.

Documents and file management (all 404 when no reMarkable source is wired):

- `GET /api/v1/remarkable/documents` - Full synced tree as `{"documents": [...]}`.
- `GET /api/v1/remarkable/documents/{id}` - Document detail (pages, OCR text, render/download availability).
- `POST /api/v1/remarkable/documents` - **Multipart** upload (`file` required — PDF/EPUB; `parent` optional folder id). Returns `201` with the document. 512 MiB cap (413 over); missing file is a 400.
- `GET /api/v1/remarkable/documents/{id}/file` - Streams the original PDF/EPUB payload with `Content-Disposition`. Annotations are not baked in. Notebooks (no payload) are a 404.
- `PATCH /api/v1/remarkable/documents/{id}` - Rename/move. Body `{name?, parent?}` — omitted = unchanged, `parent: ""` = move to My files, `name: ""` is a 400, unknown fields rejected. Returns 204.
- `DELETE /api/v1/remarkable/documents/{id}` - Move to the tablet's trash (restorable on-device). Returns 204; a non-empty folder is a 409.
- `POST /api/v1/remarkable/folders` - Create a folder. Body `{name, parent}`. Returns `201` with the folder.

Error mapping for the file-management routes: not-found family → 404; unsupported file type / target not a folder → 400; folder not empty / no synced hashtree yet / commit lost a race with a device sync → 409.

### ForestNote Sync Devices

All 404 when no ForestNote source is wired:

- `GET /api/v1/sync/devices` - Registry with derived health (pending ops, staleness, watermark pinning).
- `PATCH /api/v1/sync/devices/{id}` - Set the operator label. Body `{"label": "..."}`; `""` clears, omitted field is a 400.
- `DELETE /api/v1/sync/devices/{id}` - Prune a device's registry row (26-char ULID; 400 malformed, 404 unknown). Cleanup-only: authored ops stay; a live device re-registers on its next sync.
- `POST /api/v1/sync/compact` - Run one relay-log compaction pass.

### Attachments

- `GET /api/v1/attachments/{id}` and `GET /api/v1/attachments/fn-render` - Signed task-attachment URLs, deliberately mounted **outside** auth so third-party CalDAV clients can fetch them; the URL signature is the only guard.

### System

- `GET /api/v1/status` - Returns `{"jobs": ...}`, the processing-pipeline snapshot (see EmbeddingJob).
- `GET /api/v1/config` / `PUT /api/v1/config` - Get/update configuration (see Configuration).
- `POST /api/v1/client-error` - Browser error reporting sink. Returns 204.
- `GET /ws/logs?level={level}` - Stream live logs (WebSocket; default level `info`). `GET /logs` serves the log viewer page.

### CalDAV

UltraBridge is itself the CalDAV server; there is no separate sync engine or status entity. The task collection lives at `/caldav/user/calendars/tasks/` (discovery via `/.well-known/caldav`) and serves `DAV:sync-token`, `CS:getctag`, `DAV:supported-report-set`, and the RFC 6578 `sync-collection` REPORT for incremental sync.
