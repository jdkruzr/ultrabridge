# internal/web

Last verified: 2026-05-30 (REST v1 task surface: ForestNote-provenance + category/priority filters, include_deleted, write-side url/priority/categories/comment + Clear* sentinels, POST /api/v1/tasks/purge-deleted; legacy form-route POST /tasks/purge-deleted + Tasks-tab trash view; /files/status `forestnote` block + Re-OCR transient feedback; GET /api/search `?limit=` clamp)

## REST v1 task API — write/read surface extensions (2026-05-29)

`GET /api/v1/tasks` gained six query parameters in addition to the existing
status/due filters:

- `notebook_id`, `notebook_name`, `source` — ForestNote provenance filters
  (match on the structured columns extracted from `X-FORESTNOTE-*`).
- `category` — single VTODO CATEGORIES entry (case-sensitive equality,
  not substring). Post-fetch filter — see `containsCategory` for the
  scale-ceiling note.
- `priority` — VTODO PRIORITY value verbatim (`"1"`..`"9"`).
- `include_deleted` — when truthy (`1`/`true`/`yes`/`on`), pulls via
  `TaskService.ListIncludingDeleted` so soft-tombstoned rows surface
  alongside live ones; the response `deleted` flag distinguishes them.

`POST /api/v1/tasks` and `PATCH /api/v1/tasks/{id}` accept the new write
fields: `url`, `priority`, `categories`, `comment`. PATCH additionally
accepts `clear_url`, `clear_priority`, `clear_comment` sentinels (Clear
wins over value when both are set; mirrors the existing `clear_due_at`
shape). `categories` on PATCH is wholesale — send `[]` to clear, omit to
leave unchanged. The handler decodes directly into `service.TaskCreate` /
`service.TaskPatch` so JSON tags stay single-sourced.

New route: `POST /api/v1/tasks/purge-deleted?older_than_days=N`
(default 30; rejects `N <= 0`). Hard-purges soft-deleted rows whose
`last_modified` is older than the cutoff. Returns `200` with
`{"deleted": N}`. This is the only endpoint that triggers a real `DELETE
FROM tasks` — every other "delete" tombstones.

## Sidebar nav (device-grouped)

The left nav (`layout.html`) groups source-specific tabs under static
device headers: **Supernote** → Files (`HasSupernoteSource`) + Digests
(`HasDigests`); **Boox** → Files (`HasBooxSource`); **ForestNote** → Files
(`HasForestNote`). Globals (Tasks, Search, Chat, Logs, Settings) sit outside
the groups. `baseTemplateData` sets `HasDigests` (= digest service wired) and
`HasForestNote` (= a `forestnote` source is wired via
`NoteService.HasForestNoteSource()`, falling back to the legacy `sync_enabled`
setting); these also drive the search facet checkboxes. When no
Supernote/Boox/digest/ForestNote source exists, a single flat "Files" link is
shown (legacy fallback).

HTTP handler and HTML templates for the UltraBridge web UI.

## Handler contract

`NewHandler(tasks, notes, search, config, noteDB, notesPathPrefix, booxNotesPath, logger, broadcaster) *Handler`

Post-decoupling the Handler takes four service interfaces instead of individual domain stores; the RAG/chat/sync dependencies are now encapsulated inside those services rather than being constructor arguments.

- `tasks service.TaskService` — required.
- `notes service.NoteService` — required. Nil-safe downstream: if the service is constructed with no Supernote store and no Boox store, the Files tabs render informative empty states rather than crashing.
- `search service.SearchService` — required. When embedding / chat infrastructure isn't wired, `SearchService.HasEmbeddingPipeline()` returns false and the chat tab hides accordingly.
- `config service.ConfigService` — required; surfaces the sync-status provider and the running-config drift flag.
- `noteDB *sql.DB` — nil-safe; when nil, config/sources API and MCP token routes are not registered.
- `notesPathPrefix string` — device file path prefix for rendering note page images in the API.
- `booxNotesPath string` — root of the Boox catalog on disk; used by `respondFileRowOrRedirect` to dispatch fragment + redirect target by path prefix, and by the `BooxNotesPath` template key.
- `logger *slog.Logger`, `broadcaster *logging.LogBroadcaster` — required.
- `Handler` implements `http.Handler` via an internal `*http.ServeMux`.

For tests, `LegacyNewHandler` in `handler_test.go` bridges the old 22-argument signature to the new one by constructing each service internally.

## Routes

| Method | Path | Handler | Notes |
|--------|------|---------|-------|
| GET | `/setup` | `handleSetup` | First-run setup page |
| POST | `/setup/save` | `handleSetupSave` | Save initial credentials |
| GET | `/` | `handleIndex` | Task list |
| POST | `/tasks` | `handleCreateTask` | |
| POST | `/tasks/{id}/complete` | `handleCompleteTask` | |
| POST | `/tasks/bulk` | `handleBulkAction` | |
| POST | `/tasks/purge-completed` | `handlePurgeCompleted` | |
| POST | `/tasks/purge-deleted` | `handlePurgeDeleted` | Legacy form-route sibling of `POST /api/v1/tasks/purge-deleted`. Hard-codes the 30-day cutoff via `webPurgeDeletedDays` (paired with `purgeDeletedDefaultDays` in `api_v1.go` — keep numerically in sync). HTMX response is empty 200; non-HX redirects to `/`. Backs the "Purge deleted" button on the Tasks tab's trash view. |
| GET | `/logs` | `handleLogs` (SSE) | Log stream |
| GET | `/settings` | `handleSettings` | Settings page (config + MCP tokens + UB-as-SPC server card) |
| POST | `/settings/save` | `handleSettingsSave` | Save config changes. Routes by hidden `section` field: `supernote`, `general`, `boox`, `ub-spc` (UB-as-SPC server config — all restart-required; secret fields keep current value when left blank). |
| GET | `/files` | `handleFiles` | Legacy entry point; 303-redirects to `/files/supernote` or `/files/boox` based on configured sources. Renders an empty-state placeholder when neither is configured. |
| GET | `/files/supernote` | `handleFilesSupernote` | Supernote file browser (directory tree, breadcrumbs, sort, pagination). Path traversal guarded. |
| GET | `/files/boox` | `handleFilesBoox` | Boox catalog listing (flat, Title/Folder/Device/NoteType/Pages columns, sort, pagination). |
| GET | `/files/forestnote` | `handleFilesForestNote` | ForestNote browser. Default/`?folder=<id>`: a Supernote-style table (Name/Type/Pages/Created/Modified/Status/Actions) of that folder's subfolders + notebooks, with a breadcrumb trail. `?notebook=<id>`: the enriched detail view (metadata header + per-page thumbnail + OCR text + Delete/Re-OCR/Download actions). Inventory is a live projection of the `fn_*` mirror (no filesystem); created_at is synced, "Modified" is derived MAX(lww_wall_ts) over notebook+pages+strokes. |
| GET | `/files/forestnote/render` | `handleForestNoteRender` | JPEG for a `forestnote://{nb}/{page}` path, rendered on the fly from strokes (no cache). `Cache-Control: public, max-age=300`. |
| POST | `/files/forestnote/delete` | `handleForestNoteDelete` | Soft-delete a notebook (UB-local, by `notebook` form value) + de-index its pages. HX: empty 200 (row swaps out); non-HX: 303 to `?folder=<back>`. Device-authoritative source: a re-edited notebook can resurrect on next sync (messaged in UI). |
| POST | `/files/forestnote/reprocess` | `handleForestNoteReprocess` | Re-enqueue a notebook's pages for re-OCR/re-index (fire-and-forget on the sync bridge). |
| GET | `/files/forestnote/export` | `handleForestNoteExport` | Stream a notebook's live pages as a single `application/pdf` (images→PDF via `internal/forestpdf`). |
| GET | `/digests` | `handleDigests` | Digests tab (Phase D2): Supernote "summary" excerpts synced from the device. Flat list + group/tag filter pills. Requires a `DigestService` (set via `SetDigestService`, SPC server mode only); otherwise renders a disabled notice. |
| POST | `/files/queue` | `handleFilesQueue` | Enqueue file for OCR. Row fragment dispatches by path prefix. |
| POST | `/files/skip` | `handleFilesSkip` | Mark skipped (manual). |
| POST | `/files/unskip` | `handleFilesUnskip` | Remove manual skip. |
| POST | `/files/force` | `handleFilesForce` | Unskip + enqueue (overrides size_limit). |
| GET | `/files/status` | `handleFilesStatus` | JSON: ProcessorStatus |
| GET | `/files/history` | `handleFilesHistory` | JSON: Job record for a path |
| GET | `/files/boox/render` | `handleBooxRender` | JPEG page image for Boox note |
| GET | `/files/boox/versions` | `handleBooxVersions` | JSON: []BooxVersion for archived versions |
| POST | `/files/import` | `handleFilesImport` | Bulk import from configured import path (Boox). Non-HX lands on `/files/boox`. |
| POST | `/files/retry-failed` | `handleFilesRetryFailed` | Reset all failed Boox jobs to pending. (SN-side retry is a gap — see follow-up #17.) |
| POST | `/files/delete-note` | `handleFilesDeleteNote` | Delete single Boox note + jobs + content + cache (Boox-only). |
| POST | `/files/delete-bulk` | `handleFilesDeleteBulk` | Delete multiple Boox notes. |
| POST | `/files/migrate-imports` | `handleFilesMigrateImports` | Copy imported files to Boox notes directory. |
| POST | `/files/scan` | `handleFilesScan` | Trigger immediate filesystem scan (Supernote). Non-HX lands on `/files/supernote`. |
| POST | `/processor/supernote/start` | `handleProcessorStart` | Start the Supernote processor worker. |
| POST | `/processor/supernote/stop` | `handleProcessorStop` | Stop the Supernote processor worker. |
| POST | `/processor/boox/start` | `handleBooxProcessorStart` | Start the Boox pipeline worker. |
| POST | `/processor/boox/stop` | `handleBooxProcessorStop` | Stop the Boox pipeline worker. |
| GET | `/search` | `handleSearch` | Hybrid search (via `rag` retriever) with a source-type facet (`?source=` repeated: supernote/boox/forestnote/digest; none = all). Per-row badge from `SourceType`. |
| GET | `/sync/status` | `handleSyncStatus` | JSON: SyncStatus (adapter state, timestamps) |
| POST | `/sync/trigger` | `handleSyncTrigger` | Trigger immediate sync cycle |
| GET | `/api/search` | `handleAPISearch` | JSON: hybrid search results (requires retriever) |
| GET | `/api/notes/pages` | `handleAPIGetPages` | JSON: indexed page content for a note (requires retriever) |
| GET | `/api/notes/pages/image` | `handleAPIGetImage` | JPEG image for a note page (requires retriever) |
| GET | `/api/forestnote/text-boxes` | `handleAPIForestNoteTextBoxes` | JSON: live text boxes (id/page_id/text/z) in a notebook (`?notebook=<id>`); 404 if no ForestNote source. Backs the `list_text_boxes` MCP tool. |
| POST | `/api/forestnote/text-boxes/edit` | `handleAPIForestNoteEditTextBox` | Server-authored edit of a box's text (JSON body `{id,text}`); authors a relayable op + re-renders/re-indexes the page. Backs the `edit_text_box` MCP tool. |
| POST | `/settings/mcp-tokens/create` | `handleMCPTokenCreate` | Create new MCP bearer token; redirect with one-time display (requires noteDB) |
| POST | `/settings/mcp-tokens/revoke` | `handleMCPTokenRevoke` | Revoke MCP token by hash (requires noteDB) |

## Interfaces

### BooxImporter
```go
type BooxImporter interface {
    ScanAndEnqueue(ctx context.Context, cfg ImportConfig, logger *slog.Logger) ImportResult
    MigrateImportedFiles(ctx context.Context, importPath, notesPath string, logger *slog.Logger) MigrateResult
    Enqueue(ctx context.Context, notePath string) error
}
```
Implemented by `booxpipeline.Importer`. Handles bulk import of .note and .pdf files from a configured import path, plus the WebDAV upload enqueue callback.

### BooxProcessor
```go
type BooxProcessor interface {
    Start(ctx context.Context) error
    Stop()
}
```
Narrow handle wrapping `*booxpipeline.Processor`. Plumbed into `NewNoteService` so the `/processor/boox/start|stop` routes can start and stop the Boox pipeline worker on demand, symmetric to the Supernote processor controls.

### BooxStore (extended)
In addition to previously documented methods, `BooxStore` now includes:
- `RetryAllFailed(ctx) (int64, error)` — reset all failed jobs to pending; returns count reset
- `DeleteNote(ctx, path) error` — delete note row, associated jobs, content index entries, and rendered cache
- `SkipNote(ctx, path) error` — mark note's pending job as skipped
- `UnskipNote(ctx, path) error` — reset a skipped job to pending
- `GetQueueStatus(ctx) (QueueStatus, error)` — return counts of jobs by status

## JSON API Endpoints

### Search & Notes API (requires retriever)

- `GET /api/search?q=...&folder=...&source=...&limit=...` -- hybrid search via `SearchService.Search`. `source` is repeated (`supernote|boox|forestnote|digest`; none = all). `limit` is optional: absent/0 → service default (20), positive integer above the ceiling → clamped to 100, non-integer or negative → treated as 0 (intentionally lenient — keeps the surface friendly to MCP callers that occasionally send the param stringly). Returns `400` only when `q` is empty.
- `GET /api/notes/pages?path=...` -- fetch indexed content for a note (all pages)
- `GET /api/notes/pages/image?path=...&page=...` -- render JPEG image for a page

Conditional: only registered if `retriever` is non-nil.

### Config & Sources API (requires noteDB)

- `GET /api/config` -- returns RedactedConfig (secrets shown as "[set]"/"[not set]")
- `PUT /api/config` -- accepts JSON config update, returns SaveResult with changed keys and restart flag
- `GET /api/sources` -- list all source rows
- `POST /api/sources` -- add source (validates type, name, config_json)
- `PUT /api/sources/{id}` -- update source row
- `DELETE /api/sources/{id}` -- remove source row

Conditional: only registered if `noteDB` is non-nil.

Auth: All API routes use the same Basic Auth middleware as the web UI (authMW in main.go).

## Setup Mode

`SetupMiddleware(db, next)` -- HTTP middleware that redirects all requests to `/setup` when no credentials are configured (username + password_hash missing from settings table). Uses atomic flag for fast path after setup completes. Setup page (`/setup`) accepts initial username and password, saves bcrypt hash via appconfig, then allows normal access.

## Path traversal guard

`safeRelPath` validates any user-supplied `?path=` query parameter. Returns `"", false` for absolute paths or anything containing `..`. All file-browser routes call this before touching the filesystem.

## Template functions

Custom `template.FuncMap` functions registered in `NewHandler`:
- `formatDueTime(t time.Time) string`
- `formatCreated(t time.Time) string`
- `formatTimestamp(ms int64) string` — formats millisecond UTC unix timestamp to "2006-01-02 15:04"; returns "Never" if 0
- `fileTypeStr(ft notestore.FileType) string` — converts FileType to its string value for template conditionals
- `noteSource(path string) string` — returns "Boox" if path starts with booxNotesPath, else "Supernote"
- `fileRowID(path string) string` — returns `"file-" + hex(sha1(path))[:12]`. Deterministic path→DOM-id mapping used by both `_sn_file_row.html` and `_boox_file_row.html` (file paths contain characters invalid in HTML `id` attributes). Stable across restarts; shared formula keeps row-id identity across a cross-tab mutation response.
- `makeFileRowCtx(f service.NoteFile, relPath string) fileRowCtx` — constructs the context shape passed into `_sn_file_row`, pairing a Supernote file with the containing directory's relPath so per-row buttons can emit `back=` query strings. Boox rows use `BooxNoteSummary` directly (no RelPath needed; Boox catalog is flat).
- `hasPrefix`, `trimPrefix` — aliases of `strings.HasPrefix` / `strings.TrimPrefix`.
- `add`, `sub` — integer arithmetic helpers for pagination templates.
- `taskLink` — normalizes a task's Links payload (map or struct) into template-friendly map with Path+Page. Used by `_task_row.html` for the "from note" link.

## Fragment rendering

Mutation handlers emit row-scoped HTML fragments on `HX-Request` via
`h.renderFragment(w, r, name, data)`, parallel to `h.renderTemplate` for
tab-level templates. Two invariants enable this:

1. **Embed directive:** `//go:embed all:templates` (handler.go). The `all:`
   prefix is load-bearing — a plain `//go:embed templates` directory embed
   excludes files whose names start with `.` or `_`, which would drop every
   `_*.html` fragment silently. Any new fragment file using the
   `_<name>.html` naming must remain covered by this directive.
2. **Clone-then-Execute:** `renderFragment` calls `h.tmpl.Clone()` and
   executes the clone. `html/template` permanently locks a template tree
   against future Clones once `ExecuteTemplate` has run on it. Since
   `renderTemplate` already clones per request to install a dynamic
   `"content"` template, any method that bypasses Clone and executes
   `h.tmpl` directly would brick every subsequent tab render. New Handler
   methods that touch `h.tmpl` must preserve this invariant.

### Fragment file convention

Fragment templates live in `internal/web/templates/` and follow this shape:

```
// _task_row.html
{{define "_task_row"}}
<tr id="task-{{.ID}}" data-status="{{.Status}}" …>
  …
</tr>
{{end}}
```

- **Filename**: `_<name>.html` (underscore prefix).
- **Define block**: `{{define "_<name>"}}…{{end}}` where the name matches
  the filename (minus `.html`). Underscore-named blocks avoid collision
  with `renderTemplate`'s dynamic `"content"` slot.
- Invoked from tab templates via `{{template "_name" <data>}}` inside the
  outer loop, and from mutation handlers via `h.renderFragment(w, r,
  "_name", data)`.

### Current fragments

- `_task_row.html` — a single task row. Data: `service.Task`.
- `_sn_file_row.html` — a single Supernote row (directory or file). Data:
  `fileRowCtx{File service.NoteFile; RelPath string}` (unexported type in
  handler.go; templates access its exported fields via reflection).
- `_boox_file_row.html` — a single Boox-catalog row. Data:
  `service.BooxNoteSummary` directly (Title, Folder, DeviceModel,
  NoteType, PageCount, SizeBytes, CreatedAt, ModifiedAt, JobStatus).

### Mutation handler contract

On `HX-Request: true`, task/file mutation handlers emit either:

- A single `_task_row`, `_sn_file_row`, or `_boox_file_row` fragment (queue,
  skip, unskip, force, complete, create — the row swaps in place via
  `hx-target="closest tr" hx-swap="outerHTML"` on the originating button,
  or `hx-target="#task-table tbody" hx-swap="afterbegin"` on the create
  form). File-row mutations dispatch fragment + non-HX redirect target by
  path prefix: paths under `h.booxNotesPath` use `_boox_file_row` and
  redirect to `/files/boox`; everything else uses `_sn_file_row` and
  redirects to `/files/supernote?path=<back>`.
- A concatenation of row fragments (bulk complete — client-side JS parses
  the response as `<table><tbody>` + body + `</tbody></table>` and
  replaces matching rows by id).
- An empty 200 body (bulk delete, purge, single-row delete, and the "broad"
  mutations: scan, import, retry-failed, migrate-imports, processor
  start/stop). The originating form's `hx-on:htmx:after-request` handler
  sweeps the DOM or nudges a poller. Each broad-mutation handler supplies
  its own non-HX redirect target to `respondEmptyOrRedirect` — scan lands
  on `/files/supernote`; import/migrate/retry/delete lands on
  `/files/boox`; `/processor/<source>/*` lands on the matching tab.

Non-HX paths continue to redirect (303) to the relevant tab with query
strings preserved where applicable.

### HTMX 1.9 pitfalls

- Use **`hx-on:htmx:after-request`** (single colon, `htmx:` prefix). The
  HTMX 2.x `hx-on::after-request` shorthand is not recognized by the
  bundled 1.9.10; using it causes the form-hijack to silently fail.
- When parsing concatenated `<tr>` fragments client-side, **wrap the
  response in `<table><tbody>…</tbody></table>`** before `DOMParser`.
  HTML5's "in body" insertion mode strips orphan `<tr>` tokens, so
  `new DOMParser().parseFromString(body, 'text/html').querySelectorAll('tr')`
  returns empty on raw row strings.

### `/files/status` shape: per-source-additive (2026-05-30)

`handleFilesStatus` returns `service.EmbeddingJobStatus` verbatim. The
response carries optional per-source blocks under stable keys: `boox` (the
`booxpipeline.QueueStatus` shape) and `forestnote` (the
`service.ForestNoteQueueStatus` shape — Pending / InFlight / Processed /
Dropped / Capacity). Both fields are `omitempty`, so non-FN / non-Boox
deployments emit no key for the missing source — JS gate any UI on
presence, not on zero values. `updateProcessorStatus()` (layout.html)
renders both the Files-tab proc-status line and the global status bar from
this single poll; the global bar's visibility gate matches against any
configured source. Re-OCR buttons in `_fn_note_row.html` and
`files_forestnote.html` show a transient "Queued ✓" / "Failed ✗" hint and
call `updateProcessorStatus()` after a successful enqueue so the operator
sees Pending tick up immediately rather than waiting for the next 5 s
poll.

### Design: minimal scope, no OOB

Bulk counts (selected count, processing queue depth) and the processor
status badge are updated by existing client-side listeners and the 5-second
`updateProcessorStatus` poller. HTMX out-of-band (`hx-swap-oob`) responses
are NOT used — the design explicitly stays within a single target per swap
to keep responses auditable and avoid hidden-mutation surprises. Future
work that needs to touch multiple non-row DOM regions in one response
should revisit this decision explicitly.

## Template data

Shared data in `baseTemplateData`:
- `tasks` — list of tasks for the task list page. Pulled via
  `TaskService.ListIncludingDeleted` (not `List`) so the Tasks tab can
  render its trash view. Each row carries the `Deleted bool` flag; only
  `templates/tasks.html` currently consumes this key.
- `DeletedCount` — `int` count of rows in `tasks` with `Deleted == true`.
  Surfaced in the "Show deleted (N)" toggle label so the operator sees
  the backlog size before opting into the view.
- `BooxNotesPath` — the Boox notes root directory path (may be empty if disabled); used by JavaScript to detect Boox notes

### Tasks tab: trash view (2026-05-29)

`tasks.html` has two independent visibility toggles — **Show completed**
and **Show deleted (N)** — that compose via `recomputeTaskVisibility()` in
`layout.html`. `toggleCompleted()` is retained as a back-compat alias.
Deleted rows in `_task_row.html` carry `data-deleted="true"`, render with
line-through title + "Deleted" badge, disable the selection checkbox,
omit the Complete button, and ship with inline `style="display: none;"`
so a JS-off client never sees them. The `purge-deleted-form` is hidden
by default and revealed when the toggle is on; its `confirm()` dialog
states the operation is IRREVERSIBLE (it hits the real `DELETE FROM
tasks` via `PurgeDeleted`).

## MCP Token Management (Phase 3c)

Settings page includes an MCP Tokens card (rendered when noteDB is present):
- Lists all active tokens with label, hash prefix (first 8 chars), creation timestamp, and last-used timestamp
- One-time display of raw token after creation via `?new_token=` query parameter (redirect-after-POST pattern)
- Creates bearer tokens for MCP clients via POST `/settings/mcp-tokens/create` with form field `label`
- Revokes tokens via POST `/settings/mcp-tokens/revoke` with form field `token_hash`
- Both endpoints are **nil-safe**: only registered if `noteDB != nil`
- Handler methods: `handleMCPTokenCreate`, `handleMCPTokenRevoke`
- Uses `mcpauth.CreateToken`, `mcpauth.ListTokens`, `mcpauth.RevokeToken` (Phase 1)
- Data keys: `MCPTokensEnabled` (bool), `MCPTokens` ([]mcpauth.TokenInfo), `NewMCPToken` (raw token string, one-time flash)

## Error handling pattern

All `ExecuteTemplate` calls check and log the error (`h.logger.Error`). Since headers are already written at that point, `http.Error` is not called — logging is the only recovery path.

All POST handlers to processor methods (`Enqueue`, `Skip`, `Unskip`, `Start`, `Stop`) check and log errors via `h.logger.Error`.

## Tests

`handler_test.go` uses:
- `newMockTaskStore()` — in-memory task store
- `mockNotifier` — no-op SyncNotifier
- `mockNoteStore` — configurable file map per relPath
- `mockSearchIndex` — no-op SearchIndex
- `mockProcessor` — in-memory job map; tracks running state
- `mockScanner` — counts ScanNow calls
- `mockSyncProvider` — configurable SyncStatus; tracks TriggerSync call count
- `mockBooxStore` — implements BooxStore interface; returns configurable notes and versions; nil-safe (can be passed as nil to test non-Boox configuration); includes stub implementations of RetryAllFailed, DeleteNote, SkipNote, UnskipNote, GetQueueStatus
