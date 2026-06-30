# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Last verified: 2026-06-12 (SyncModel + Settings IA: `internal/source` gained `SyncModel`/`SyncModelFor` — a typed per-source-type sync-semantics descriptor surfaced as `sync_model` on `GET /api/sources` and as Unicode-glyph banners (⇅/⬇) on the Files tabs; Settings split into four deep-linkable groups `/settings/{devices,ai,integrations,system}` (legacy `/settings` 303s to devices), with the Devices group rendering uniform per-source sections. Prior: ForestNote sync device management — /sync/v1 optional `device_name` envelope field; Settings "Sync Devices" card + /api/v1/sync/{devices,compact}; prune = cleanup-only delete of the sync_cursors row, spec §4.3)

Platform-neutral note management and task synchronization service supporting Supernote (via Supernote Private Cloud) and Onyx Boox devices. Six subsystems:
1. **CalDAV task sync** -- CalDAV VTODO over local SQLite task store
2. **Device sync** -- UB *is* the device-facing Supernote Private Cloud server (`internal/spcserver`); Supernote devices connect to UB directly. (The legacy SPC *client* — `internal/tasksync`, `internal/sync`, `internal/db`, MariaDB catalog write-through — was removed 2026-05-25.)
3. **Supernote notes pipeline** -- scans Supernote .note files, extracts/OCRs text, indexes for full-text search
4. **Boox notes pipeline** -- receives Boox .note files via WebDAV, parses ZIP+protobuf, renders strokes, OCRs, indexes for unified search
5. **RAG retrieval pipeline** -- Ollama embeddings, hybrid FTS5+vector search, vLLM-powered chat with retrieval-augmented context
6. **MCP server** -- Model Context Protocol server exposing note search/retrieval tools for AI agents

## Bash Commands: No `cd &&` Compounds

**NEVER** use `cd /path && command` compound bash statements. This triggers a Claude Code bug where the permission prompt fires on `cd` instead of the actual command.

Instead: `git -C /path`, `go -C /path build`, or absolute paths.

## Project Structure

### Core Components
- `cmd/ultrabridge/` -- entry point, wires all components

### Configuration & Data Management
- `internal/appconfig/` -- SQLite-backed application config with two-stage loading (bootstrap env vars + settings table), restart detection (see domain CLAUDE.md)
- `internal/notedb/` -- SQLite DB opener + schema migrations for notes, settings, and sources tables (see domain CLAUDE.md)
- `internal/source/` -- Platform-neutral source abstraction: `Source` interface, `SourceRow` model, CRUD operations (see domain CLAUDE.md)
- `internal/source/supernote/` -- Supernote source adapter: .note pipeline, Processor creation (see domain CLAUDE.md)
- `internal/source/boox/` -- Boox source adapter: WebDAV receiver, Processor creation (see domain CLAUDE.md)

### Task Synchronization
- `internal/caldav/` -- CalDAV backend (go-webdav), VTODO conversion with iCal blob overlay (see domain CLAUDE.md)
- `internal/taskstore/` -- Task model, field mapping helpers, MariaDB CRUD (legacy), ErrNotFound sentinel (see domain CLAUDE.md)
- `internal/taskdb/` -- SQLite task store: Open/migrate DB, implements caldav.TaskStore (see domain CLAUDE.md)

### Note Processing & Pipelines
- `internal/processor/` -- background OCR job queue: backup, extract, render, OCR, inject, index (see domain CLAUDE.md)
- `internal/search/` -- FTS5 full-text search over note content (see domain CLAUDE.md)
- `internal/notestore/` -- file inventory (scan, list, get), content hashing, job transfer against SQLite notes table (see domain CLAUDE.md)
- `internal/pipeline/` -- file detection: fsnotify watcher, reconciler, Engine.IO listener (see domain CLAUDE.md)
- `internal/booxpipeline/` -- Boox processing pipeline: store, worker, processor (parse/render/OCR/index) (see domain CLAUDE.md)

### Boox-Specific
- `internal/booxnote/` -- Boox .note ZIP parser: protobuf pages, nested shape ZIPs, binary point files (see domain CLAUDE.md)
- `internal/booxnote/proto/` -- Generated protobuf code for Boox .note format (NoteInfo, VirtualPage, ShapeInfoProto)
- `internal/booxnote/testutil/` -- Exported test helper: builds synthetic .note ZIP files for tests
- `internal/booxrender/` -- Stroke renderer: pressure-sensitive scribbles, geometric shapes via fogleman/gg (see domain CLAUDE.md)
- `internal/webdav/` -- WebDAV server for Boox file uploads with versioning (see domain CLAUDE.md)
- `internal/pdfrender/` -- PDF page rendering via pdftoppm (poppler-utils) for bulk import pipeline

### RAG & Chat
- `internal/rag/` -- RAG embedding infrastructure: Ollama embedder, embedding store with in-memory cache, hybrid FTS5+vector retriever, backfill (see domain CLAUDE.md)
- `internal/chat/` -- Chat subsystem: session/message store (SQLite), vLLM streaming handler with RAG context injection (see domain CLAUDE.md)

### Service Layer
- `internal/service/` -- Service interfaces (`TaskService`, `NoteService`, `SearchService`, `ConfigService`) that decouple HTTP handlers from concrete stores and pipelines; plus the store/pipeline interfaces (`TaskStore`, `BooxStore`, `BooxImporter`, `BooxProcessor`, `FileScanner`, `SyncNotifier`, `SyncStatusProvider`) that adapters implement (see domain CLAUDE.md)

### Web UI & API
- `internal/web/` -- HTML UI: setup wizard, settings (four deep-linkable groups: Devices · AI & Processing · Integrations · System), task list, Files tabs (with per-source sync-model banners), Search tab, Chat tab, processor C&C, sync status, Boox render/versions, JSON API, config/sources API, MCP token management, SSE log stream (see domain CLAUDE.md)
- `internal/mcpauth/` -- MCP bearer token store: SHA-256 hashed tokens in SQLite, CRUD + validation (see domain CLAUDE.md)

### SPC Server (UB-as-SPC) — the device-facing integration
- `internal/spcserver/` -- Device-facing Supernote Private Cloud protocol reimplementation: auth, tasks, file listing/download/upload/mutations, digests, and its own Socket.IO registry for STARTSYNC push. Phases 0–4 + digests-D1 complete and hardware-validated; this is now the ONLY way UB talks to Supernote devices (see domain CLAUDE.md). Spec source: `docs/spc-protocol.md` and `/home/sysop/spc-rev/cfr-decrypted/`. Config via Settings → "UB-as-SPC Device Sync Server" (`UB_SPC_*` env overrides). Remaining follow-ups: `docs/spc-followups.md`.

### Infrastructure
- `internal/auth/` -- Basic Auth middleware (bcrypt) + bearer-token validation (mcpauth)
- `internal/logging/` -- structured slog, file rotation, syslog, WebSocket broadcast

## Build & Test

Use `-C` flag to target the repo root without `cd`:

```bash
go build -C /home/jtd/ultrabridge ./cmd/ultrabridge/
go test -C /home/jtd/ultrabridge ./...
go vet -C /home/jtd/ultrabridge ./...
```

Run a single package's tests:
```bash
go test -C /home/jtd/ultrabridge ./internal/taskstore/
```


Docker build:
```bash
docker build -t ultrabridge:dev /home/jtd/ultrabridge
```

## Key Dependencies

- `github.com/jdkruzr/go-sn` -- Supernote .note file parser/writer (rendering, RECOGNTEXT injection, JIIX)
- `github.com/emersion/go-webdav` -- CalDAV protocol handler
- `github.com/fogleman/gg` -- 2D rendering (Boox stroke renderer)
- `google.golang.org/protobuf` -- Boox .note protobuf parsing
- `golang.org/x/net/webdav` -- WebDAV protocol handler (Boox uploads)
- `modernc.org/sqlite` -- pure-Go SQLite (no CGO)
- `github.com/modelcontextprotocol/go-sdk/mcp` -- built-in MCP server at `/mcp`

## Subcommands

- `ultrabridge hash-password "pw"` -- generate bcrypt hash for UB_PASSWORD_HASH
- `ultrabridge seed-user <username> <password>` -- pre-provision credentials in settings DB (headless/Docker setup, skips setup wizard)

## Configuration Architecture

### Two-Stage Loading

Configuration is loaded in two stages:

1. **Bootstrap stage (startup):** Read only `UB_DB_PATH`, `UB_TASK_DB_PATH`, and `UB_LISTEN_ADDR` from environment. These are required to start the database and HTTP server.

2. **Settings stage (runtime):** After DB opens, load all other config from the `settings` table in SQLite. This includes auth, OCR, RAG, logging, and source definitions.

### Source Abstraction

Each note source (Supernote, Boox, etc.) is represented by a `SourceRow` in the database with:
- `type`: "supernote" or "boox"
- `name`: user-provided label
- `enabled`: feature flag
- `config_json`: source-specific settings (e.g., NotesPath, BackupPath for Supernote; NotesPath for Boox)

The `Source` interface abstracts device-specific logic; each source type (supernote.Source, boox.Source) implements Start(), Stop(), Type(), Name(), and provides access to pipelines and processors.

Sources are created dynamically at startup from DB rows via the registry package, allowing hot-plugging of new device types without code changes.

### Environment Variables

Only bootstrap variables are read at startup:
- `UB_DB_PATH` -- SQLite database path
- `UB_TASK_DB_PATH` -- Task sync database path
- `UB_LISTEN_ADDR` -- HTTP server listen address

All other configuration (auth, OCR, sources, logging, RAG, chat) is configured via the Settings UI after first boot.

## Conventions

- Module: `github.com/sysop/ultrabridge`
- Config: all env vars prefixed `UB_`; all non-bootstrap config is DB-backed (SQLite settings table) and editable via the web Settings UI
- Auth: single-user Basic Auth, password stored as bcrypt hash
- Sources: device-agnostic pipelines, platform-specific adapters for Supernote and Boox

### CalDAV Subsystem (SQLite)
- Local SQLite task store (internal/taskdb) replaces direct MariaDB access for CalDAV
- DB timestamps: millisecond UTC unix timestamps, 0 = unset
- IDs: MD5(title + timestamp) for task IDs (matches Supernote device convention)
- Supernote quirk: `completed_time` holds creation time; `last_modified` holds actual completion time
- Soft deletes only on the user-facing path: `is_deleted = 'Y'`. The single exception is `taskdb.HardDeleteOlderThan`, called only via `service.PurgeDeleted` → `POST /api/v1/tasks/purge-deleted` / MCP `purge_deleted_tasks`, which permanently removes already-tombstoned rows older than a caller-specified cutoff (default 30 days; rejects `<= 0`).
- iCal blob: VTODO round-trip fidelity via `ical_blob` column; DB fields overlaid on read. `CATEGORIES`, `COMMENT`, and `X-FORESTNOTE-NATIVE-URL` are deliberately blob-only and parsed at response time via `caldav.ParseBlobMetadata` (no structured column); REST/MCP writes go through `caldav.BuildBlobWithMetadata` (create) / `caldav.MergeBlobMetadataPatch` (update) so other blob properties survive untouched. **The blob never carries `SUMMARY`** — the column-overlay path injects the live Title at serve time (an empty SUMMARY in the blob would round-trip as malformed VTODO per RFC 5545 §3.6.2).
- ForestNote provenance: `X-FORESTNOTE-{NOTEBOOK-ID,PAGE-ID,NOTEBOOK-NAME,SOURCE}` on inbound VTODOs is lifted into nullable TEXT columns on `tasks` (with a partial index on `forestnote_notebook_id`), powering the `notebook_id` / `notebook_name` / `source` filters on `GET /api/v1/tasks` and `list_tasks`. The raw bytes stay in the blob too.

### Device Sync (UB-as-SPC server)
- UB runs the Supernote Private Cloud protocol (`internal/spcserver`); the device connects to UB as its cloud. Tasks, files, and digests sync over that protocol. UB-wins conflict resolution; local SQLite task store (taskdb) is authoritative.
- STARTSYNC push: `internal/spcserver/notify` over the server's own Socket.IO registry (server mode only); a no-op notifier otherwise.
- Config: Settings → "UB-as-SPC Device Sync Server" (DB-backed; `UB_SPC_*` env overrides). Task DB path: UB_TASK_DB_PATH (SQLite).
- The legacy SPC *client* (REST pull via `internal/tasksync`, Engine.IO `internal/sync`, MariaDB `internal/db`) was removed 2026-05-25 — UB no longer connects out to a real SPC.

### Device Sync (ForestNote /sync/v1) — device management
- A device's identity is its client-minted `site_id` ULID; registration is implicit (first sync creates the `sync_cursors` row). A reinstall/factory-reset mints a NEW site_id, orphaning the old row. Optional `device_name` envelope field labels devices (absent/empty preserves the stored name); "first seen" decodes from the ULID timestamp (`syncstore.ULIDTime`).
- Management surface: Settings → "Sync Devices" card + `GET /api/v1/sync/devices`, `DELETE /api/v1/sync/devices/{id}`, `POST /api/v1/sync/compact` (via `service.SyncDeviceService` over `*forestnote.Source`; nil when no FN source).
- **Prune is cleanup-only** (spec §4.3): deletes the cursor row, never the device's authored `sync_ops` (they're content). A pruned-but-alive device re-registers on next sync — `ApplyBatch` reseeds its `accepted_through` from the pre-batch changelog `MAX(op_seq)` so compaction holes can't wedge it below the device's high-water.
- Manual "Compact now" runs even when periodic compaction is off (the press is the operator opt-in); the prune→compact loop reclaims history a dead device was pinning.

### Notes Pipeline (SQLite)
- Two databases: SQLite for tasks (taskdb), SQLite for notes pipeline (notedb). (The MariaDB SPC catalog write-through was removed with the legacy client 2026-05-25; the UB-as-SPC server derives file size from os.Stat and md5 lazily via `spc_file_ids`, so no catalog sync is needed.)
- SQLite in WAL mode; on-disk DBs pool reads (MaxOpenConns=8, busy_timeout=5000) so reads do not serialize behind writes; in-memory DBs pin to MaxOpenConns=1
- Job statuses: pending -> in_progress -> done|failed|skipped
- Backup before modification: original .note copied to backup tree, never overwritten
- OCR source tracking: "myScript" (device RECOGNTEXT) vs "api" (vision API result)
- Standard-only injection: only notes with FILE_RECOGN_TYPE=0 (Standard) get RECOGNTEXT injection (JIIX v3 format); RTR notes (FILE_RECOGN_TYPE=1) are OCR'd and indexed but file is NOT modified
- Requeue with delay: jobs can be set back to pending with a future `requeue_after` timestamp
- Content hash dedup: SHA-256 stored on job completion; pipeline detects moved/renamed files and transfers job records instead of re-processing
- Pipeline config: notes path and backup path from source config_json; OCR settings from settings table

### Boox Notes Pipeline (WebDAV + shared SQLite notedb)
- Boox .note format: ZIP containing protobuf metadata, nested shape ZIPs, binary point files
- WebDAV upload endpoint at `/webdav/` (behind Basic Auth) receives .note files from Boox devices
- On upload: parse ZIP, render pages to JPEG cache, OCR via vision API, index into shared FTS5 tables
- Shares SQLite notedb with Supernote pipeline (boox_notes, boox_jobs tables alongside notes, jobs)
- Shares search index: same note_content/note_fts tables, unified search across both device types
- Shares OCR client: same processor.Indexer and processor.OCRClient interfaces
- File versioning: overwritten .note files archived to `.versions/` directory tree
- Rendered page cache: JPEG images at `{notesPath}/.cache/{noteID}/page_{N}.jpg`
- Bulk import: filesystem paths can be imported in bulk via the web UI; importer scans for .note and .pdf files, enqueues each, and optionally migrates files to the Boox notes directory
- PDF support: .pdf files accepted alongside .note files; pages rendered via pdftoppm (pdfrender package), then OCR'd and indexed identically to .note files
- Config: Boox sources configured via settings UI and sources table (NotesPath, ImportPath in config_json)

### RAG Retrieval Pipeline (Ollama + SQLite)
- Embedding: Ollama `/api/embed` endpoint generates float32 vectors, stored as little-endian blobs in `note_embeddings` table
- In-memory cache: all embeddings loaded on startup; cache updated atomically on Save
- Hybrid retriever: combines FTS5 keyword search with cosine-similarity vector search, fuses results via reciprocal rank fusion
- Backfill: startup goroutine embeds unembedded pages; manual trigger via web UI for re-embedding after model upgrades
- Integration: both Supernote and Boox workers embed OCR'd text as part of the processing pipeline (best-effort, failures logged not propagated)
- Config: embed_enabled, ollama_url, ollama_embed_model in settings table (defaults: http://localhost:11434, nomic-embed-text:v1.5)

### Chat Subsystem (vLLM + RAG)
- RAG-powered chat: user question triggers hybrid search, top results injected as context into vLLM prompt
- SSE streaming: handler proxies vLLM OpenAI-compatible streaming response to browser via Server-Sent Events
- Session persistence: chat sessions and messages stored in SQLite (chat_sessions, chat_messages tables in notedb)
- Config: chat_enabled, chat_api_url, chat_model in settings table (defaults: http://localhost:8000, Qwen/Qwen3-8B)

### MCP Server (`/mcp`)
- Built into `cmd/ultrabridge`; mounted at `/mcp` behind the main auth/OAuth flow
- Tools: `search_notes`, `get_note_pages`, `get_note_image`, `list_text_boxes`, `edit_text_box`, and task CRUD/housekeeping tools
- Tool handlers proxy through UltraBridge JSON APIs using an internal bearer token
- Tool results include MCP `structuredContent` plus a short text fallback; `search_notes` supports source, location, folder, device_model, created/modified date, sort, mode, and limit filters, with deprecated `device` and `date_from`/`date_to` aliases still accepted
- Result deep-links use `boox_external_base_url` when configured, falling back to the local listener URL

### MCP Token Management (internal/mcpauth)
- Bearer tokens for MCP clients stored as SHA-256 hashes in `mcp_tokens` table (shared notedb)
- Raw token shown once at creation, never stored; only hash persisted
- Schema migrated at ultrabridge startup via `mcpauth.Migrate` (idempotent)
- Web UI settings card for create/revoke (internal/web)
