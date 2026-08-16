# Changelog

## v1.6.0 - 2026-08-16

### Added

- **reMarkable file management — the tablet as an e-reader.** UltraBridge could receive everything from a reMarkable and give nothing back: no way to put a book on the device, get a file off it, or delete one. The Files tab (and `/api/v1/remarkable/`) now uploads PDF/EPUB into any folder, downloads the original payload back out, deletes (a move to the tablet's own trash, recoverable on-device), and manages folders — create, rename, move. Server-authored changes commit a new root generation through the same generation-CAS the device uses and push a SyncComplete to every connected tablet, so the device updates within seconds. Index serialization and composite hashing follow the device's own algorithm byte-for-byte (pinned by test vectors), per the rmfakecloud reference.

  A visibility change rides along: documents whose parent is the tablet's trash no longer appear in UltraBridge listing or search — including items trashed on-device before this release.

- **Collection-level change detection (RFC 6578).** Every CalDAV client was re-enumerating the whole task collection on every sync, because UltraBridge served no way to ask "did anything change?". `PROPFIND` for `CS:getctag` or `DAV:sync-token` came back 404, and a `sync-collection` REPORT came back `400 unsupported REPORT root`.

  Now: an empty token returns the live set and a token to resume from; a subsequent token returns only what changed, with deletions reported as removals; `DAV:limit` truncates and the token resumes at the boundary; `calendar-data` can be requested inline, rendered through the same path as a `GET` so attachments behave identically. `DAV:supported-report-set` is served too — go-webdav never emitted it, so clients had no way to discover that *any* report was supported.

  The token is `MAX(updated_at)` across all rows **including tombstones**. Soft deletes stamp `updated_at`, so it moves on delete; a live-only maximum does not, because deleting any task except the most recent leaves it unchanged and the client never finds out. That is the trap that makes naive ctags wrong, and it is why the long-dead `ComputeCTag` (`MAX(last_modified)` over non-deleted rows) was retired rather than wired up.

  `CS:getctag` is served alongside, carrying the same value, so clients that ask for it first — Cfait does — get their fast path without a wasted round trip.

### Fixed

- **A lost root-commit race could corrupt the reMarkable tree.** The generation-checked blob write stored its payload file *before* checking the generation, so a writer that lost the optimistic-concurrency race (a device's 412, or now a server-side commit) still overwrote the root payload on disk while the database kept the winner's generation — the winner's committed tree silently replaced by the loser's. Surfaced by the new concurrent-commit tests; CAS writes now land in uniquely-named files whose path commits transactionally with the generation.

- **Editing a completed task moved its completion date.** Verified against a live instance: a title-only edit shifted `COMPLETED` from `20260701T120000Z` to the moment of the write, discarding the client's correct value even though it was re-sent. Completion time was read from `last_modified`, following the Supernote convention documented in `docs/PRIVATE_CLOUD_REFERENCE.md` ("use as COMPLETED when status=completed") — but that convention only holds for a client that never edits a task after completing it. The device doesn't; UltraBridge does, and its own `Update` stamped that column on every write. Completion now lives in a dedicated `completed_at` column, end to end.

- **Every task reported a `completed_at`, including ones never completed.** The REST and MCP surfaces mapped it from `completed_time` — SPC's creation-time field, despite the name — with no status guard, so `completed_at` was always just the creation timestamp. It now comes from the real column and is absent unless the task is completed.

- **`IN-PROCESS` and `CANCELLED` collapsed to `NEEDS-ACTION`.** The store held Supernote's two states, so those two were flattened on the way in, and because the VTODO emit path overlays `STATUS` from the database after decoding the iCal blob, the blob couldn't preserve them either. A CalDAV client's running timer stopped and a cancelled task un-cancelled itself on the next sync. The store now holds all four RFC 5545 states.

- **Device writes wiped everything the device doesn't understand.** The SPC handlers built a task from wire fields alone and handed it to an update that writes every column, so completing a task *on the Supernote* nulled its `ical_blob` and ForestNote provenance — destroying recurrence rules, alarms, parent/child hierarchy, dependencies and all `X-CFAIT-*` state. Device writes are now read-modify-write.

- **`PUT /api/file/schedule/task` failed for payloads omitting `completedTime`**, hitting a `NOT NULL` violation. Pre-existing; resolved by the same merge, since that column now comes from the stored row.

### Changed

- `/api/v1/tasks?status=` accepts `in_process` and `cancelled` alongside the existing values, so every status the API can return can also be filtered on.
- The Tasks table's "Created" column reads the creation timestamp directly. It rendered `CompletedAt`, which only looked right because that field was sourced from the creation-time column.
- `taskstore.SupernoteStatus` is now `FromCalDAVStatus`, and `taskdb.MaxLastModified` is now `MaxUpdatedAt`. Both old names described the wrong thing; the first ran on the CalDAV inbound path, which is how the vendor's two-state model came to govern the store in the first place.

### Notes

- The architectural rule this restores was in the original design (`docs/design-plans/2026-04-04-caldav-native-taskstore.md`, DoD #1): UltraBridge owns task storage with full VTODO fidelity, and Supernote's quirks stay isolated in one boundary package. That package was never built — the "UB as SPC" refactor replaced it with `internal/spcserver/mapping`, which declared it carried the quirks "straight through", and so the wire format quietly became the schema's meaning.
- `completed_time` and `last_modified` are retained for SPC wire compatibility only: the first feeds MD5 task-id generation, and a zero `lastModified` makes a task invisible on-device. Neither is a completion time any more.
- The one-time `completed_at` backfill seeds from `last_modified` and logs how many rows are **suspect** — where `last_modified == updated_at`, meaning the value was written by an `Update` and may be an edit time rather than a completion. Those are unrecoverable; the count is reported rather than guessed at.

### Verified

- `go test ./...` (53 packages), `go vet ./...`
- New coverage pins each symptom: completion time surviving an unrelated edit, all four statuses round-tripping through the blob-overlay path, `completed_at` absent unless completed, the non-destructive device status merge, and blob/provenance survival through both device write paths.
- End-to-end against a live instance over raw CalDAV: the `COMPLETED` drift, the two collapsed statuses, and the spurious `completed_at` are all gone; blob passthrough still intact.

## v1.5.0 - 2026-07-28

### Added

- **Name your synced devices.** Settings → Devices now lets you give each ForestNote and reMarkable device a label you recognize, instead of picking it out of a 26-character ULID. The label is stored in its own `operator_label` column and is never written by the sync path, so nothing a device reports can overwrite it. Clearing the field falls back to the device-reported name. Also available as `PATCH /api/v1/sync/devices/{id}` and `PATCH /api/v1/remarkable/devices/{id}` with a `{"label": "…"}` body.

  Both registries needed this for the same reason: `RecordCursor` refreshes ForestNote's `device_name` from the request envelope on any sync that carries one, and reMarkable's `touchDevice` rewrites `device_desc` on *every* check-in. A hand-set name stored in either field would not have survived. It also unblocks the still-owed ForestNote client work — `device_name` is now purely additive and cannot clobber an operator's label.

### Fixed

- **ForestNote's pipeline "done" count read 0 after every restart.** Not a stuck pipeline: every other source counts done jobs from a persistent table, but ForestNote has no job table, so the bar was rendering the sync bridge's in-memory counter — monotonic since process start by design. It now shows `indexed`, a live count of pages carrying OCR text, which survives restarts and means the same thing as the other sources' "done". The since-boot figure is still shown as "N this session" while work is flowing.

### Notes

- The indexed count joins through page **and** notebook, so a soft-deleted notebook's pages drop out. That matches Boox (deleting a note removes its job rows) and matches what the Files tab lists — on a development instance it was the difference between 109 raw text rows and 85 live ones.
- Device labels are deliberately not seeded by any migration: fleet naming is per-instance state, not something to ship in a schema.

### Verified

- `go test ./...`, `go vet ./...`
- New coverage pins the reason the separate column exists: label a device, then drive a real `RecordCursor` sync (ForestNote) or `touchDevice` heartbeat (reMarkable) and assert the label survives while the device-reported name still lands in its own field.
- `./rebuild.sh`, then live: both columns migrated onto the existing database, three devices named through the UI, and a subsequent device sync left the labels intact.

## v1.4.0 - 2026-07-27

### Added

- **ForestNote sync schema v4: `notebook.aspect_long_axis`.** A notebook now carries the native page aspect of the device that created it, so a note authored on one device letterboxes rather than distorts when opened on another. Schema hash moves to `74e6b5d7…`; the prior v3 hash stays in the `AcceptsSchemaHash` grace window for one release.

### Changed

- **v2 and v1 sync clients are now rejected.** This bump closes v2's grace window — a v2 client gets a hard 409. All devices were on v3 or later before release.

### Notes

- The ForestNote client half shipped ahead of the server and had been sending `aspect_long_axis` since 2026-07-04. Because `sync_ops.payload` stores each op verbatim, the column survived in the changelog while the server was still on v3, and the mirror backfilled from that history on deploy — 24 notebooks had ever been sent a non-null value, and 24 carried one afterward. Devices did not re-author anything. Recorded in `docs/sync/aspect-ratio-client-handoff.md`.

### Verified

- `go test ./...`, `go vet ./...`
- Runtime `SchemaHash()` matches the v4 constant; `AcceptsSchemaHash` accepts v4 and v3, rejects v2.
- `./rebuild.sh`, then a live device sync: 1046 ops pushed (1031 stroke, 7 page_text_from_client, 4 notebook, 4 page), relay high-water 67641 → 68688, no 409s, and a server-authored `page_text_from_server` op back — full round trip on v4.

## v1.3.0 - 2026-07-27

### Added

- **One persistent pipeline status bar on every page.** The status bar is now rendered once by the layout, outside the HTMX swap target, so it appears identically on Tasks, Search, every Files tab, Digests, Chat, Logs, and all four Settings groups. It is sticky, always visible (a quiet source reads "idle" so page content never shifts), carries Supernote and Boox start/stop controls everywhere, and has a collapsible per-source detail row whose state persists across navigation.
- `booxpipeline.Processor.Running()` and `booxpipeline.QueueStatus.Running`, so the Boox worker reports run state the way the Supernote processor already did.
- Canonical note reference links: `service.ReferenceForPath` maps an internal note path to a UB detail URL plus, where the source has one, a native opener URL. Surfaced through MCP note tools.

### Fixed

- **Pipeline status bar disappeared after the first navigation.** The old `#global-status` element lived inside `<main id="main-content">`, the target of every sidebar link's `hx-get` with the default `innerHTML` swap, so the first client-side navigation destroyed it and it stayed gone until a full page reload.
- **reMarkable OCR jobs abandoned mid-run were never recovered.** Nothing moved a row out of `in_progress` except the goroutine that claimed it, and the ordinary re-enqueue path is revision-gated, so a process that died mid-job wedged the job permanently — two had been stuck for a month. Added a startup reclaim plus a watchdog that requeues jobs stuck past 15 minutes, with an attempt cap so a page that reliably kills the worker fails visibly instead of cycling forever.
- **A momentary OCR-backend outage permanently failed Boox notes.** `FailJob` was the only terminal path, so a connection refusal or a 500 stranded a note in `failed` until someone pressed Retry Failed. OCR failures are now classified: dial/timeout failures and 5xx/429/408 responses are retried with backoff doubling from 1 minute (capped at 30), while a 4xx is a verdict and still fails immediately. The long-dormant `requeue_after` column finally drives this.
- **Empty Boox notebooks were reported as permanent failures.** A notebook created on the device and never drawn in parsed as `read virtual page: entry not found`. It is now skipped with a recorded reason via the new `booxnote.ErrEmptyNotebook`. An archive where only *some* declared pages are missing is still a hard error — that is truncation, and it stays loud.
- **Stopping the Boox worker mid-note marked the note failed.** Shutdown now leaves the row `in_progress` for the startup reclaim.
- **`booxpipeline.Processor` panicked on a stop/start/stop cycle** — its shutdown channel was minted once at construction and closed by the worker, so the second stop closed an already-closed channel. Start and Stop are now idempotent with a per-start channel. Same fix applied to the reMarkable OCR processor, whose Start silently no-op'd after any Stop.
- **ForestNote page-text backfill relayed timestamps ~1000x too small.** `note_content.indexed_at` is stored in seconds while the sync layer is milliseconds; the backfill passed the raw column through, so backfilled pages carried an `ocr_at`/`created_at` reading as January 1970. Converted at the boundary.
- Note search now defaults to most-recent ordering.

### Verified

- `go test ./...`
- `go vet ./...`
- `./rebuild.sh`
- Live: the two month-old reMarkable jobs reclaimed on startup and OCR'd successfully; the two transient Boox failures recovered on retry and indexed ~600 characters of previously unsearchable handwriting; a newly synced note flowed enqueue -> OCR -> index in about a second.

## v1.2.1 - 2026-06-30

### Fixed

- Improve filtered search recall for source/device/date/location filters by fetching a larger candidate set before post-merge filtering.
- Make multi-word keyword queries match both exact phrases and all separated terms, so queries like "Froster Glacier" can find notes where the terms appear apart.
- Improve hybrid search relevance by weighting lexical matches above vector-only matches and suppressing ultra-short vector-only tail results when lexical hits exist.

### Verified

- `go test ./internal/rag ./internal/search`
- `go test ./...`
- `./rebuild.sh`
- Live MCP checks for ForestNote storage and Froster Glacier searches

## v1.2.0 - 2026-06-30

### Changed

- Rationalized the MCP tool surface with structured MCP output for note, ForestNote text-box, and task tools.
- `search_notes` now supports first-class `source`/`sources`, `location`, `device_model`, created/modified date, sort, mode, and limit filters.
- `/api/search` and `/api/v1/search` now pass `device_model` through to RAG search and return device metadata when available.
- Deprecated MCP/API search aliases remain accepted: `device` maps to `device_model`, and `date_from`/`date_to` map to modified-date filters.
- MCP search results now include source type, folder, device model, timestamps, detail URLs, and follow-up image hints.

### Verified

- `go test ./...`
- `./rebuild.sh`

## v1.1.0 - 2026-06-30

### Changed

- MCP is now a first-class built-in UltraBridge endpoint at `/mcp`.
- Removed the separate `ub-mcp` sidecar, stdio transport, sidecar Docker target, sidecar Compose service, and `mcp_port` configuration.
- Updated Settings -> Integrations to show the built-in `/mcp` endpoint and MCP token management only.
- `install.sh` and `rebuild.sh` now build and manage only the main UltraBridge container.

### Added

- Built-in `/mcp` now exposes ForestNote text box tools:
  - `list_text_boxes`
  - `edit_text_box`

### Verified

- `go test ./...`
- `go build ./cmd/ultrabridge/`
- `bash -n install.sh`
- `bash -n rebuild.sh`
- `docker compose -f docker-compose.yml config --quiet`

## v1.0.0 - 2026-06-29

UltraBridge's first major release. This release turns the project from a Supernote/Boox helper into a multi-source sync, search, task, and MCP hub.

### Highlights

- **Built-in Supernote SPC server:** UltraBridge now serves the Supernote Private Cloud protocol directly. Supernote devices and the Partner App sync to UltraBridge without an external SPC stack or MariaDB.
- **ForestNote sync:** added `/sync/v1`, Rhizome-backed relay state, device management, relay-log compaction, ForestNote Files UI, page rendering, PDF export, client OCR indexing, text-box sync, task provenance, and native page links.
- **reMarkable support:** added a reMarkable source with protocol-compatible sync routes, blob/document storage, device registration, Files UI, page rendering, server-side OCR, metadata indexing, tablet search compatibility, MyScript HWR proxying, notifications, and compatibility probes.
- **CalDAV attachments:** tasks can expose signed attachment URLs, including ForestNote-rendered task context and generic CalDAV attachment flows. MCP task output now surfaces attachment summaries.
- **Settings and source model overhaul:** Settings are grouped into Devices, AI & Processing, Integrations, and System. Source rows expose sync-model descriptors and per-source device slots.
- **Search/RAG/chat improvements:** search is keyword-first, source-aware, and wired to richer ForestNote and reMarkable content. Embedding chunking, retrieval behavior, and local chat integration received substantial test coverage and fixes.
- **MCP/API polish:** expanded task APIs and MCP tools for filtering, task details, provenance, URLs, attachments, purge operations, and safer bearer-token usage.

### Device Sources

- Added first-class ForestNote source support, including sync admin seams, mirror reads, backfill, compaction controls, and web/API device management.
- Added first-class reMarkable source support, including protocol routes for sync roots, blobs, signed URLs, search, telemetry stubs, beta/settings probes, device-management probes, and token recovery paths.
- Added Supernote Partner App SPC routes and a dedicated reverse-proxy hostname model for SPC traffic.
- Added source sync-model descriptors for Supernote, Boox, ForestNote, and reMarkable.
- Improved Boox file handling with move/reindex/delete behavior, source-consistency cleanup, and shared detail rendering.

### Tasks, CalDAV, And MCP

- Added signed task attachment serving and CalDAV `ATTACH` presentation.
- Added ForestNote task provenance, native links, rendered attachment support, and task purge/trash parity.
- Added task attachment visibility to MCP task output.
- Expanded task list filtering by ForestNote metadata, category, priority, deleted state, and combined filters.
- Added audit-friendly task mutation behavior across API and MCP surfaces.

### Search, OCR, RAG, And Chat

- Indexed ForestNote client OCR for search and RAG while preserving server-owned render-trigger semantics.
- Added reMarkable OCR queueing, manual reprocess, rendered page OCR, and metadata indexing.
- Added retrieval chunking, search limit fixes, keyword-first web search, and source filters.
- Disabled Qwen3 thinking tokens for OCR requests.
- Improved processor status and Re-OCR feedback in the web UI.

### Web UI

- Added Files tabs for ForestNote and reMarkable.
- Converged note detail pages into a shared in-tab page grid.
- Added digest viewer, source-page rendering, status panels, sync-model banners, sidebar search improvements, and trash/purge UI parity.
- Split Settings into deep-linkable grouped pages with scoped saves.
- Added ForestNote device registry controls and reMarkable device/document admin APIs.

### Deployment And Operations

- Added Apache 2.0 `LICENSE` and `NOTICE`.
- Vendored the private Rhizome Go dependency in-tree so Docker builds do not need private credentials.
- Added DB read/write connection pooling to avoid indexing starving reads.
- Updated Docker/Compose defaults for built-in SPC, reMarkable mounts, log files, and MCP bearer-token auth.
- Added extensive docs for SPC protocol behavior, ForestNote sync, reMarkable cutover, sync vectors, and source/settings IA.

### Tests

- Added broad coverage across source registry behavior, syncstore/Rhizome parity, ForestNote sync, reMarkable protocol/search/OCR/rendering/storage, CalDAV attachments, task APIs, MCP tools, RAG retrieval, web settings, Files views, and service seams.
- Fixed test harness assumptions around Rhizome vector locations and older shared-vector schema shape.

## v0.5.0 - 2026-04-05

First public release.

### Features

**CalDAV Task Sync**
- Full CalDAV VTODO collection at `/caldav/tasks/`
- Compatible with DAVx5, GNOME Evolution, Apple Reminders, 2Do, and other CalDAV clients
- Bidirectional sync with Supernote device via SPC REST API
- SQLite-backed task store (works standalone without MariaDB)

**Supernote Notes Pipeline**
- Automatic `.note` file discovery via fsnotify watcher + 15-minute reconciler
- Handwritten text extraction from MyScript RECOGNTEXT
- Optional vision-API OCR (Anthropic, OpenRouter, vLLM/Ollama)
- JIIX RECOGNTEXT injection back into `.note` files for on-device display
- Backup before modification
- SPC catalog sync after file changes

**Boox Notes Pipeline**
- WebDAV server at `/webdav/` for Boox device uploads
- Parses Boox `.note` ZIP format (protobuf metadata, nested shape ZIPs, V1 binary point files)
- Renders pages with pressure-sensitive strokes, 10 pen types, geometric shapes, affine transforms
- OCR via shared vision API
- Version-on-overwrite: old files archived to `.versions/` with nanosecond timestamps
- Device model, note type, and folder extracted from upload path

**Red Ink To-Do Extraction**
- Optional second OCR pass on Boox notes looking for red handwriting
- Red text automatically created as CalDAV tasks
- Duplicate detection against both incomplete and completed tasks
- Configurable prompt via Settings tab

**Unified Search**
- FTS5 full-text search across both Supernote and Boox notes
- Source badges (SN / B) on search results
- Folder filter dropdown
- BM25 ranking consistent across sources

**Web UI**
- Five tabs: Tasks, Files, Search, Logs, Settings
- Source badges distinguish Supernote and Boox notes throughout
- Rendered Boox page viewing with version history
- Per-pipeline OCR prompt configuration
- Live WebSocket log streaming with level filter
- Scan Now, Purge Completed, and bulk task actions

**Deployment**
- Interactive `install.sh` with auto-detection of Supernote Private Cloud
- Standalone mode for Boox-only users (no SPC/MariaDB required)
- `rebuild.sh` with `--fresh` (preserves versions) and `--nuke` (clears all)
- Polling health checks with progress reporting

### Technical Details

- Pure Go, single binary, Docker deployment
- SQLite (WAL mode, pure-Go via modernc.org/sqlite) for tasks, notes pipeline, and settings
- 145+ automated tests across 8 packages
- Protobuf wire-format parsing tolerant of non-UTF-8 device firmware output
- Shared `Indexer` interface for unified search across both pipelines
