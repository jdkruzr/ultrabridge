# Note Database

Last verified: 2026-05-26

## Purpose
Opens and migrates the SQLite database used by the Supernote pipeline, Boox pipeline, RAG embeddings, and chat subsystem.
Centralizes schema ownership so all packages share one DB connection.

## Contracts
- **Exposes**: `Open(ctx, path) (*sql.DB, error)` -- opens/creates SQLite DB, applies migrations, returns pool.
- **Guarantees**: WAL mode and foreign keys enabled. Schema is idempotent (safe to call on existing DB). MaxOpenConns=1 (SQLite single-writer).
- **Expects**: Writable filesystem path. Context for cancellation.

## Dependencies
- **Uses**: `modernc.org/sqlite` (pure-Go, no CGO)
- **Used by**: `cmd/ultrabridge` (startup), indirectly by `notestore`, `processor`, `search`, `booxpipeline`, `rag`, `chat`, `mcpauth`
- **Boundary**: Only owns schema DDL. No CRUD logic -- that lives in domain packages.

## Key Decisions
- Pure-Go SQLite (`modernc.org/sqlite`): avoids CGO dependency for cross-compilation
- Single `*sql.DB` shared across all pipeline packages via dependency injection

## Invariants
- `notes.path` is PRIMARY KEY (absolute filesystem path)
- `jobs.note_path` has FK to `notes.path` -- notes row must exist before job insert
- `boox_notes.path` is PRIMARY KEY (absolute filesystem path)
- `boox_jobs.note_path` has FK to `boox_notes.path` -- boox_notes row must exist before job insert
- `note_fts` is a contentless FTS5 table synced via triggers on `note_content`
- `note_content` has UNIQUE(note_path, page) -- one content row per page per note
- `note_content` is shared by both Supernote and Boox pipelines (unified search)
- `note_embeddings` has UNIQUE(note_path, page, chunk) -- long pages embed as multiple chunk vectors (chunk 0..N); pre-chunking rows migrate to chunk 0 via an in-place table rebuild (see schema.go)
- `chat_messages.session_id` has FK to `chat_sessions.id`

## Schema Tables

### Supernote Pipeline
- `notes` -- file metadata (path, type, size, mtime, backup state)
- `jobs` -- processing queue (status, attempts, timestamps, requeue_after)

### Boox Pipeline
- `boox_notes` -- file metadata (path PK, note_id, title, device_model, note_type, folder, page_count, file_hash, version, timestamps)
- `boox_jobs` -- processing queue (status, attempts, timestamps, requeue_after, ocr_source, api_model; FK to boox_notes.path)

### Shared (both pipelines)
- `note_content` -- extracted text per page (title, body, keywords, source)
- `note_fts` -- FTS5 virtual table for full-text search

### RAG Pipeline
- `note_embeddings` -- float32 vector embeddings per page (note_path, page, embedding BLOB, model, created_at)

### Chat Subsystem
- `chat_sessions` -- conversation sessions (id, title, created_at, updated_at; millisecond timestamps)
- `chat_messages` -- messages within sessions (id, session_id FK, role, content, created_at; millisecond timestamps)

### MCP Auth
- `mcp_tokens` -- bearer token hashes for MCP clients (token_hash TEXT PK, label, created_at, last_used; migrated by `mcpauth.Migrate`, not by notedb.Open)
