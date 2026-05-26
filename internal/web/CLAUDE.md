# internal/web

Last verified: 2026-05-26 (Digests tab + device-grouped sidebar nav + source-faceted search added; UB-as-SPC server settings card; routes, fragment rendering, source-split Files tabs, Boox processor controls)

## Sidebar nav (device-grouped)

The left nav (`layout.html`) groups source-specific tabs under static
device headers: **Supernote** → Files (`HasSupernoteSource`) + Digests
(`HasDigests`); **Boox** → Files (`HasBooxSource`). Globals (Tasks, Search,
Chat, Logs, Settings) sit outside the groups. `baseTemplateData` sets
`HasDigests` (= digest service wired) and `HasForestNote` (= `sync_enabled`);
these also drive the search facet checkboxes. When no Supernote/Boox/digest
source exists, a single flat "Files" link is shown (legacy fallback).

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
| GET | `/logs` | `handleLogs` (SSE) | Log stream |
| GET | `/settings` | `handleSettings` | Settings page (config + MCP tokens + UB-as-SPC server card) |
| POST | `/settings/save` | `handleSettingsSave` | Save config changes. Routes by hidden `section` field: `supernote`, `general`, `boox`, `ub-spc` (UB-as-SPC server config — all restart-required; secret fields keep current value when left blank). |
| GET | `/files` | `handleFiles` | Legacy entry point; 303-redirects to `/files/supernote` or `/files/boox` based on configured sources. Renders an empty-state placeholder when neither is configured. |
| GET | `/files/supernote` | `handleFilesSupernote` | Supernote file browser (directory tree, breadcrumbs, sort, pagination). Path traversal guarded. |
| GET | `/files/boox` | `handleFilesBoox` | Boox catalog listing (flat, Title/Folder/Device/NoteType/Pages columns, sort, pagination). |
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

- `GET /api/search?q=...&folder=...&device=...&from=...&to=...&limit=...` -- hybrid search using SearchRetriever
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
- `tasks` — list of tasks for the task list page
- `BooxNotesPath` — the Boox notes root directory path (may be empty if disabled); used by JavaScript to detect Boox notes

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
