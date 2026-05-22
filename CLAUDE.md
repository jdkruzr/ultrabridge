# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Last verified: 2026-04-28

Platform-neutral note management and task synchronization service supporting Supernote (via Supernote Private Cloud) and Onyx Boox devices. Six subsystems:
1. **CalDAV task sync** -- CalDAV VTODO over local SQLite task store
2. **Device sync** -- bidirectional task sync with Supernote via SPC REST API (adapter-based)
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
- `cmd/ub-mcp/` -- MCP server binary: exposes search_notes, get_note_pages, get_note_image tools via stdio or HTTP SSE (see domain CLAUDE.md)

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
- `internal/tasksync/` -- adapter-agnostic sync engine: reconciliation, sync map, conflict resolution (see domain CLAUDE.md)
- `internal/tasksync/supernote/` -- Supernote SPC REST adapter: JWT auth, field mapping, migration (see domain CLAUDE.md)

### Note Processing & Pipelines
- `internal/processor/` -- background OCR job queue: backup, extract, render, OCR, inject, SPC catalog sync (see domain CLAUDE.md)
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
- `internal/web/` -- HTML UI: setup wizard, settings, task list, Files tab, Search tab, Chat tab, processor C&C, sync status, Boox render/versions, JSON API, config/sources API, MCP token management, SSE log stream (see domain CLAUDE.md)
- `internal/mcpauth/` -- MCP bearer token store: SHA-256 hashed tokens in SQLite, CRUD + validation (see domain CLAUDE.md)

### SPC Server (UB-as-SPC refactor, Phase 0+)
- `internal/spcserver/` -- Device-facing Supernote Private Cloud protocol reimplementation; currently stub-only (see domain CLAUDE.md). Spec source: `docs/spc-protocol.md` and `/home/sysop/spc-rev/cfr-decrypted/`. Design plan: `docs/design-plans/2026-05-15-ub-as-spc-refactor.md` (Phase 0 complete; Phase 1 = skeleton + auth + tasks).

### Infrastructure
- `internal/sync/` -- Engine.IO v3 notifier: STARTSYNC push + inbound events (see domain CLAUDE.md)
- `internal/auth/` -- Basic Auth middleware (bcrypt)
- `internal/db/` -- MariaDB pool + single-user discovery
- `internal/logging/` -- structured slog, file rotation, syslog, WebSocket broadcast
- `tests/` -- integration tests (require real DB)

## Build & Test

Use `-C` flag to target the repo root without `cd`:

```bash
go build -C /home/jtd/ultrabridge ./cmd/ultrabridge/
go build -C /home/jtd/ultrabridge ./cmd/ub-mcp/
go test -C /home/jtd/ultrabridge ./...
go vet -C /home/jtd/ultrabridge ./...
```

Run a single package's tests:
```bash
go test -C /home/jtd/ultrabridge ./internal/taskstore/
```

Integration tests (require running MariaDB with Supernote schema):
```bash
TEST_DBENV_PATH=/mnt/supernote/.dbenv go test -C /home/jtd/ultrabridge -tags integration ./tests/ -v
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
- `github.com/modelcontextprotocol/go-sdk/mcp` -- MCP server (stdio + HTTP SSE transport)

## Subcommands

- `ultrabridge hash-password "pw"` -- generate bcrypt hash for UB_PASSWORD_HASH
- `ultrabridge seed-user <username> <password>` -- pre-provision credentials in settings DB (headless/Docker setup, skips setup wizard)
- `ub-mcp` -- MCP server for AI agents (stdio by default, `--http :8081` for HTTP SSE)

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
- Config: all env vars prefixed `UB_`, DB creds from shared `.dbenv` file (legacy MariaDB support)
- Auth: single-user Basic Auth, password stored as bcrypt hash
- Sources: device-agnostic pipelines, platform-specific adapters for Supernote and Boox

### CalDAV Subsystem (SQLite)
- Local SQLite task store (internal/taskdb) replaces direct MariaDB access for CalDAV
- DB timestamps: millisecond UTC unix timestamps, 0 = unset
- IDs: MD5(title + timestamp) for task IDs (matches Supernote device convention)
- Supernote quirk: `completed_time` holds creation time; `last_modified` holds actual completion time
- Soft deletes only: `is_deleted = 'Y'`, never hard delete
- iCal blob: VTODO round-trip fidelity via `ical_blob` column; DB fields overlaid on read

### Device Sync (tasksync)
- Adapter-based: sync engine is device-agnostic, adapters implement DeviceAdapter interface
- UB-wins conflict resolution: local task store is authoritative
- Sync map: per-task local-to-remote ID mapping in SQLite tables (sync_state, task_sync_map)
- Supernote adapter: SPC REST API with JWT challenge-response auth
- Config: UB_SN_SYNC_ENABLED, UB_SN_SYNC_INTERVAL, UB_SN_API_URL, UB_SN_PASSWORD
- Task DB path: UB_TASK_DB_PATH (SQLite file for local task store)

### Notes Pipeline (SQLite + MariaDB catalog sync)
- Three databases: SQLite for tasks (taskdb), SQLite for notes pipeline (notedb), MariaDB for SPC catalog sync
- After OCR injection, processor updates SPC MariaDB catalog (f_user_file, f_file_action, f_capacity) so the device sees correct file size/md5 -- best-effort, failures logged not propagated
- SQLite in WAL mode, MaxOpenConns=1 (single-writer)
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

### MCP Server (cmd/ub-mcp)
- Separate binary that calls UltraBridge JSON API endpoints via HTTP
- Auth chain: DB-backed bearer token (SHA-256 validated against notedb) -> static bearer token (UB_MCP_AUTH_TOKEN) -> Basic Auth fallback
- Tools: `search_notes` (hybrid search), `get_note_pages` (page content), `get_note_image` (JPEG rendering)
- Transport: stdio (default) or HTTP SSE (`--http :8081`)
- Config: UB_MCP_API_URL (default http://localhost:8443), UB_MCP_API_USER, UB_MCP_API_PASS, UB_DB_PATH (shared notedb for token validation)

### MCP Token Management (internal/mcpauth)
- Bearer tokens for MCP clients stored as SHA-256 hashes in `mcp_tokens` table (shared notedb)
- Raw token shown once at creation, never stored; only hash persisted
- Schema migrated at ultrabridge startup via `mcpauth.Migrate` (idempotent)
- Web UI settings card for create/revoke (internal/web)
