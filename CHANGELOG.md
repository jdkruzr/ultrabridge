# Changelog

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
