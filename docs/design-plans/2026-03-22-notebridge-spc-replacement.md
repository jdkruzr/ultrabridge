# NoteBridge: Supernote Private Cloud Replacement

> **SUPERSEDED (2026-05-15) by [`2026-05-15-ub-as-spc-refactor.md`](2026-05-15-ub-as-spc-refactor.md).**
> That plan revives this vision as a phased refactor of UltraBridge itself (rather than a separate "NoteBridge" service), grounded in the now-decompiled SPC source. Read it instead. This doc is kept for historical context only.

## Summary

NoteBridge is a Go service that replaces Supernote Private Cloud (SPC) — the manufacturer's self-hosted sync server — with a leaner, self-contained alternative. Where SPC requires four Docker containers (application server, MariaDB, Redis, and a note library), NoteBridge runs as a single container backed entirely by SQLite. The tablet points at NoteBridge instead of SPC with only a hostname change; all device-facing API endpoints, error codes, and port numbers are protocol-compatible with the existing tablet firmware.

The design draws from three sources: the device sync protocol and data model are ported from opennotecloud (a Go SPC reimplementation); architectural patterns — JWT-signed single-use download URLs, soft delete, and page-level change detection — are adapted from allenporter/supernote; and the OCR pipeline (job queue, RECOGNTEXT injection, FTS5 full-text search, CalDAV task sync, and web UI) transfers directly from UltraBridge, the existing Go sidecar that currently runs alongside SPC. The central reliability improvement is the elimination of the CONFLICT file problem: because NoteBridge owns the file store outright, post-OCR file updates are recorded as server-side changes and the tablet simply downloads the updated version on its next sync, with no competing authority to generate conflicts.

## Definition of Done
- Supernote tablet can authenticate, sync files, tasks, and digests with NoteBridge instead of SPC
- RECOGNTEXT injection works end-to-end: tablet syncs .note file, NoteBridge OCRs and injects, tablet receives injected version on next sync — with no CONFLICT files
- CalDAV task sync works against NoteBridge's own SQLite database (no MariaDB dependency)
- Full-text search works on injected RECOGNTEXT content
- Socket.IO connection supports real-time sync notifications between server and tablet
- Web UI provides file browsing, job management, and search (carried from UltraBridge)
- Deployment via install.sh + rebuild.sh + Docker (single container, no SPC dependencies)
- Migration script transfers files, tasks, and digests from existing SPC installation

## Acceptance Criteria

### notebridge-spc-replacement.AC1: Device Authentication
- **AC1.1 Success:** Tablet completes challenge-response flow (random code → SHA256(MD5+code) → JWT token)
- **AC1.2 Success:** JWT token accepted by auth middleware on subsequent requests
- **AC1.3 Failure:** Wrong password hash returns error with code E0019
- **AC1.4 Failure:** Expired/invalid JWT returns error with code E0712
- **AC1.5 Failure:** Account locked after 6 failures in 12 hours, returns E0045
- **AC1.6 Edge:** Expired challenge code (>5min) rejected

### notebridge-spc-replacement.AC2: File Sync
- **AC2.1 Success:** Tablet acquires sync lock, lists files, uploads new file, sees it in next list
- **AC2.2 Success:** Download via signed URL returns correct file content with Range header support
- **AC2.3 Success:** Chunked upload merges parts into final file on last chunk
- **AC2.4 Success:** File delete moves to recycle (soft delete), no longer appears in list
- **AC2.5 Success:** Move/rename updates file path, autorenames on collision
- **AC2.6 Success:** Copy creates independent duplicate with new Snowflake ID
- **AC2.7 Failure:** Sync lock rejects second device with E0078
- **AC2.8 Failure:** Expired signed URL rejected (upload >15min, download >24hr)
- **AC2.9 Failure:** Reused nonce on signed URL rejected (single-use enforcement)
- **AC2.10 Edge:** Sync lock expires after 10min, allowing retry. Lock refreshed on upload finish.

### notebridge-spc-replacement.AC3: Socket.IO
- **AC3.1 Success:** Tablet establishes Socket.IO connection with JWT, receives handshake
- **AC3.2 Success:** Ping/pong keepalive maintains connection
- **AC3.3 Success:** ServerMessage pushed to connected tablets when files change
- **AC3.4 Failure:** Invalid JWT on Socket.IO connect returns error frame

### notebridge-spc-replacement.AC4: OCR Pipeline + RECOGNTEXT Injection
- **AC4.1 Success:** .note file uploaded via sync → OCR runs → RECOGNTEXT injected → syncdb updated with new MD5
- **AC4.2 Success:** Next tablet sync sees updated MD5 → downloads injected version (no CONFLICT)
- **AC4.3 Success:** RTR notes (FILE_RECOGN_TYPE=1) are OCR'd and indexed but NOT modified
- **AC4.4 Success:** Re-processing: user edits note → uploads new version → hash mismatch detected → re-queued with 30s delay
- **AC4.5 Success:** FTS5 search returns OCR'd content from injected RECOGNTEXT
- **AC4.6 Edge:** Backup created before any file modification

### notebridge-spc-replacement.AC5: Tasks
- **AC5.1 Success:** Task created on tablet syncs to NoteBridge, persists in schedule_tasks
- **AC5.2 Success:** Task list (group) CRUD works: create, update, delete, list
- **AC5.3 Success:** Batch task update atomically updates multiple tasks
- **AC5.4 Success:** nextSyncToken pagination returns only tasks modified since last sync
- **AC5.5 Success:** Recurrence field preserved through sync round-trip

### notebridge-spc-replacement.AC6: Digests
- **AC6.1 Success:** Summary created on tablet syncs to NoteBridge
- **AC6.2 Success:** Summary groups (collections) CRUD works
- **AC6.3 Success:** Summary file upload/download works via signed URLs

### notebridge-spc-replacement.AC7: CalDAV
- **AC7.1 Success:** Tasks synced from tablet appear as VTODOs via CalDAV
- **AC7.2 Success:** VTODO created via CalDAV client syncs to tablet on next sync
- **AC7.3 Success:** Task completion status round-trips: tablet ↔ CalDAV

### notebridge-spc-replacement.AC8: Web UI
- **AC8.1 Success:** File browser shows files from blob storage
- **AC8.2 Success:** Job status shows pending/in-progress/done counts from processor
- **AC8.3 Success:** Search returns FTS5 results with snippets
- **AC8.4 Success:** Task list view shows tasks from syncdb

### notebridge-spc-replacement.AC9: Deployment
- **AC9.1 Success:** install.sh creates directories, prompts for credentials, starts container
- **AC9.2 Success:** rebuild.sh rebuilds and restarts with health check
- **AC9.3 Success:** Single container, no external dependencies (no MariaDB, Redis, etc.)

### notebridge-spc-replacement.AC10: Migration
- **AC10.1 Success:** migrate.sh copies files from SPC and creates correct syncdb entries
- **AC10.2 Success:** Tasks exported from SPC MariaDB appear in NoteBridge
- **AC10.3 Success:** Tablet pointed at NoteBridge after migration syncs without errors

## Glossary

- **SPC (Supernote Private Cloud)**: The manufacturer's official self-hosted sync server for Supernote tablets. NoteBridge replaces it entirely.
- **UltraBridge**: The existing Go sidecar service that currently runs alongside SPC to add OCR, search, and CalDAV sync. NoteBridge absorbs its pipeline code.
- **opennotecloud**: An open-source Go reimplementation of SPC whose device sync protocol and data model NoteBridge ports.
- **allenporter/supernote**: An open-source Python Supernote server whose architectural patterns (signed URLs, soft delete, event bus) NoteBridge adapts.
- **go-sn**: The Go library used for parsing `.note` files, decoding strokes, and injecting RECOGNTEXT.
- **RECOGNTEXT**: A structured text field embedded inside a `.note` file that stores OCR results. NoteBridge injects this field after running vision-API OCR so the tablet can search handwritten content.
- **JIIX**: The JSON Ink Interchange format (v3 "Raw Content" variant) used as the wire format inside RECOGNTEXT.
- **FILE_RECOGN_TYPE**: A flag inside `.note` files. `0` = Standard (NoteBridge may inject RECOGNTEXT); `1` = RTR/real-time recognition (file is indexed but never modified).
- **RTR (Real-Time Recognition)**: A Supernote note mode where the device itself performs handwriting recognition. NoteBridge reads RTR output for indexing but does not overwrite the file.
- **CONFLICT file**: A duplicate file the Supernote tablet creates when it detects that both the server and the device have independently modified the same file. NoteBridge eliminates this by being the sole file authority.
- **Snowflake ID**: A 64-bit time-ordered identifier (epoch: 2020) used as the primary key for files and other records, matching SPC's own ID scheme.
- **Sync lock**: A per-user, per-device mutex stored in SQLite that prevents two devices from syncing simultaneously. TTL: 10 minutes.
- **Challenge-response auth**: The Supernote device login protocol. The server issues a random code; the tablet hashes it with the user's MD5 password via SHA-256 and returns the result. A JWT is issued on success.
- **Signed URL**: A time-limited, single-use URL (JWT + nonce) used for file upload and download, avoiding the need to pass the session token in every file transfer request.
- **Nonce**: A single-use random value stored in `url_nonces` and consumed on first use, preventing replay of signed URLs.
- **Socket.IO / Engine.IO**: The WebSocket-based protocol the Supernote tablet uses for real-time communication with the sync server (keepalive ping/pong and `ServerMessage` push for cross-device notifications).
- **ServerMessage**: A Socket.IO event pushed from NoteBridge to connected tablets to signal that files have changed and a new sync should begin.
- **FTS5**: SQLite's built-in full-text search extension. NoteBridge uses it to index injected RECOGNTEXT for searching handwritten note content.
- **BlobStore**: NoteBridge's internal abstraction over file storage (interface: Put, Get, Delete, Exists). Initial implementation writes to the local filesystem.
- **EventBus**: An in-process publish/subscribe mechanism (goroutine-dispatched) that decouples the sync layer from the OCR pipeline.
- **syncdb**: The single SQLite database that holds all NoteBridge state: auth, file catalog, tasks, digests, jobs, and FTS index.
- **Soft delete / recycle bin**: File deletes move the record to a `recycle_files` table rather than destroying data.
- **CalDAV / VTODO**: CalDAV is a standard protocol for calendar and task sync (RFC 4791). VTODO is the iCalendar component type for tasks.
- **emersion/go-webdav**: The Go library that provides the CalDAV/WebDAV server framework.
- **fsnotify**: A Go library for filesystem event notifications. Used as a backup file-detection path alongside the primary event bus.
- **Reconciler**: A periodic full-directory scan (every 15 minutes) that catches any files the event bus or fsnotify watcher may have missed.
- **nextSyncToken**: A cursor the tablet sends to retrieve only tasks modified since its last sync, enabling efficient incremental task synchronization.
- **Digest / Summary**: Supernote's name for highlight collections — groups of clipped or annotated content synced separately from `.note` files.
- **migrate.sh**: A one-time script that copies files, tasks, and digests from an existing SPC installation into NoteBridge.

## Architecture

NoteBridge is a new Go project (separate repo from UltraBridge) that replaces Supernote Private Cloud entirely. The tablet points directly at NoteBridge for all sync operations. SPC's four containers (supernote-service, mariadb, redis, notelib) are eliminated.

**Approach: "Best of both"** — opennotecloud's device sync protocol (Go, ~3K lines, full Socket.IO) combined with allenporter/supernote's architectural patterns (event bus, JWT signed URLs with single-use nonces, soft delete/recycle bin, page-level change detection). UltraBridge's proven pipeline (~70% of code) transfers directly.

### Package Structure

```
internal/
  sync/              ← NEW: device sync protocol server
    server.go        ← HTTP router for /api/* device endpoints
    auth.go          ← challenge-response login, JWT, signed URLs
    files.go         ← file list, upload/download/delete/move/copy
    folders.go       ← folder CRUD
    socketio.go      ← Socket.IO handler (ping/pong, ServerMessage push)
    tasks.go         ← schedule task/group CRUD (device API)
    digests.go       ← summary/digest CRUD (device API)
    snowflake.go     ← Snowflake ID generator (2020 epoch)
    middleware.go     ← auth middleware, logging, recovery
    errors.go        ← SPC-compatible error codes (E0018, E0078, etc.)
  syncdb/            ← NEW: sync database (SQLite)
    schema.go        ← all tables (auth, files, tasks, digests, etc.)
    store.go         ← query methods for sync operations
  blob/              ← NEW: file storage abstraction
    storage.go       ← BlobStore interface: Put, Get, Delete, Exists
    local.go         ← local filesystem implementation
  events/            ← NEW: in-process event bus
    bus.go           ← publish/subscribe, fire-and-forget via goroutines
    types.go         ← FileUploadedEvent, FileModifiedEvent, FileDeletedEvent
  pipeline/          ← FROM UB: file detection + OCR enqueue
    pipeline.go      ← event listener (primary), fsnotify + reconciler (backup)
    watcher.go       ← fsnotify with 2s debounce (unchanged)
    reconciler.go    ← 15min full scan (unchanged)
  processor/         ← FROM UB: OCR job queue
    processor.go     ← SQLite job queue, worker loop, watchdog (unchanged)
    worker.go        ← injection pipeline, post-injection updates syncdb
    ocrclient.go     ← Anthropic/OpenAI vision API (unchanged)
  notestore/         ← FROM UB: note file tracking (unchanged)
  search/            ← FROM UB: FTS5 indexing (unchanged)
  caldav/            ← FROM UB: CalDAV backend (unchanged interface)
  taskstore/         ← FROM UB: task CRUD (rewritten against syncdb)
  web/               ← FROM UB: web UI handlers (unchanged)
  logging/           ← FROM UB: SSE log streaming (unchanged)
  config/            ← FROM UB: env var loading (adapted, no SPC vars)
```

**External dependency:** go-sn library imported via `go.mod` (unchanged from UltraBridge).

**Removed from UltraBridge:**
- `internal/processor/catalog.go` — MariaDB catalog sync
- `internal/sync/notifier.go` — Engine.IO STARTSYNC to SPC
- `internal/db/db.go` — MariaDB connection pool
- All MariaDB-related env vars and dependencies

### Component Interaction

```
Tablet ──HTTP──→ sync/server.go ──→ syncdb (SQLite)
       ──WS───→ sync/socketio.go     ↑
                     │                │
                     ↓                │
              blob/local.go ←── processor/worker.go
                     │                ↑
                     ↓                │
              events/bus.go ──→ pipeline/pipeline.go
                                      │
                                      ↓
                               processor/processor.go
                                      │
                                      ↓
                               search/index.go (FTS5)

CalDAV client ──→ caldav/backend.go ──→ taskstore/store.go ──→ syncdb
Web browser   ──→ web/handler.go ──→ notestore, search, processor, syncdb
```

### Sync Protocol Flow

1. Tablet authenticates via challenge-response (MD5+SHA256) → receives JWT
2. Tablet calls `synchronous/start` → acquires per-user sync lock (10min TTL)
3. Tablet lists server files via `list_folder_v3` → compares MD5 hashes
4. Downloads (server newer): signed URL → `GET /api/oss/download` with Range support
5. Uploads (tablet newer): signed URL → `POST /api/oss/upload` (single or chunked) → `upload/finish`
6. `upload/finish` → registers in syncdb → publishes `FileUploadedEvent`
7. Pipeline listener → enqueues .note files for OCR
8. Deletes: soft delete to recycle table
9. `synchronous/end` → releases lock
10. Socket.IO: bidirectional keepalive + `ServerMessage` push for cross-device notifications

### Injection Policy

RECOGNTEXT injection applies only to Standard notes (FILE_RECOGN_TYPE=0). RTR notes (FILE_RECOGN_TYPE=1) are OCR'd and indexed for search but the file is never modified — the device's auto-convert clobbers injected RECOGNTEXT within ~40 seconds of opening an RTR note, and silently converting RTR→Standard removes the real-time recognition sidebar.

### CONFLICT Bug Resolution

The CONFLICT file problem is solved structurally. NoteBridge owns file storage — there is no second system to conflict with. Post-injection flow:

1. Worker finishes RECOGNTEXT injection (Standard notes only) → writes file to blob storage
2. Worker updates `syncdb.files` row with new MD5, size, update_time
3. Worker stores SHA-256 in `notestore.notes` for re-processing detection
4. Socket.IO pushes notification to connected tablets
5. Next tablet sync sees updated MD5 → downloads injected version

No conflict possible because NoteBridge is the authority. The tablet never sees a "local modification" — it sees a server-side update and downloads it.

### Data Model (SQLite)

Single SQLite database with these table groups:

**Auth & Users** (from opennotecloud, with rate limiting):
- `users` — id, email, password_hash (MD5 hex), username, error_count, last_error_at, locked_until
- `equipment` — id, equipment_no, user_id, name, status, total_capacity
- `auth_tokens` — key, token, user_id, equipment_no, expires_at (30-day JWT)
- `login_challenges` — account, timestamp, random_code (5-min TTL)
- `sync_locks` — user_id, equipment_no, expires_at (10-min TTL)
- `server_settings` — key/value (jwt_secret generated on first run)
- `url_nonces` — nonce, expires_at (single-use signed URL tracking)

**File Catalog** (from opennotecloud, with soft delete):
- `files` — id (Snowflake), user_id, directory_id, file_name, inner_name, storage_key, md5, size, is_folder, is_active, created_at, updated_at
- `recycle_files` — same columns + deleted_at, original_directory_id
- `chunk_uploads` — upload_id, part_number, total_chunks, chunk_md5, path

**Tasks** (from opennotecloud, CalDAV-compatible):
- `schedule_groups` — task_list_id, user_id, title, last_modified, create_time
- `schedule_tasks` — task_id, user_id, task_list_id, title, detail, status, importance, due_time, completed_time, recurrence, is_reminder_on, links, sort columns

**Digests** (from opennotecloud):
- `summaries` — id, user_id, unique_identifier, name, description, file_id, parent_unique_identifier, content, tags, md5_hash, is_summary_group, creation_time, last_modified_time

**Notes Pipeline** (from UltraBridge, unchanged):
- `notes` — path, rel_path, file_type, size_bytes, mtime, sha256, backup_path
- `jobs` — id, note_path, status, skip_reason, ocr_source, attempts, requeue_after
- `note_content` — note_path, page, title_text, body_text, keywords, source
- `note_fts` — FTS5 virtual table with triggers

### Blob Storage Layout

```
/data/notebridge/
  notebridge.db          ← single SQLite database
  storage/
    {user_email}/
      Supernote/
        Note/             ← .note files (mirrors device folder structure)
        Document/
        EXPORT/
  chunks/
    {upload_id}/
      part_00001          ← temp chunked upload parts
  backups/
    {note_filename}.bak   ← pre-injection backups
  cache/
    {file_id}/
      {page_id}.jpg       ← rendered page images
```

Blob store key = relative path under `storage/{email}/Supernote/`. Files are human-browsable on disk. Pipeline's `notes.path` = absolute blob path.

### Contracts

**BlobStore interface:**
```go
type BlobStore interface {
    Put(ctx context.Context, key string, r io.Reader) (size int64, md5 string, err error)
    Get(ctx context.Context, key string) (io.ReadCloser, int64, error)
    Delete(ctx context.Context, key string) error
    Exists(ctx context.Context, key string) bool
}
```

**EventBus interface:**
```go
type EventBus interface {
    Publish(ctx context.Context, event Event)
    Subscribe(eventType string, handler func(Event))
}

type Event struct {
    Type   string // "file.uploaded", "file.modified", "file.deleted"
    FileID int64
    UserID int64
    Path   string
}
```

**SyncDB store interface** (key methods):
```go
type SyncStore interface {
    // Auth
    CreateChallenge(ctx context.Context, account string) (code string, timestamp int64, err error)
    VerifyLogin(ctx context.Context, account, passwordHash string, timestamp int64) (*User, error)
    CreateToken(ctx context.Context, userID int64, equipmentNo string) (string, error)
    ValidateToken(ctx context.Context, token string) (userID int64, equipmentNo string, err error)

    // Sync lock
    AcquireLock(ctx context.Context, userID int64, equipmentNo string) error
    ReleaseLock(ctx context.Context, userID int64, equipmentNo string) error
    RefreshLock(ctx context.Context, userID int64) error

    // Files
    ListFolder(ctx context.Context, userID int64, directoryID int64) ([]FileEntry, error)
    CreateFile(ctx context.Context, f *FileEntry) error
    UpdateFile(ctx context.Context, id int64, md5 string, size int64) error
    SoftDelete(ctx context.Context, id int64) error
    GetFile(ctx context.Context, id int64) (*FileEntry, error)

    // Signed URLs
    StoreNonce(ctx context.Context, nonce string, expiresAt time.Time) error
    ConsumeNonce(ctx context.Context, nonce string) (bool, error)
}
```

### Deployment

**Docker Compose** (standalone, single container):
```yaml
services:
  notebridge:
    build: .
    ports:
      - "8443:8443"    # web UI + CalDAV
      - "19071:19071"  # device sync API + Socket.IO
    volumes:
      - /data/notebridge/notebridge.db:/data/notebridge.db
      - /data/notebridge/storage:/data/storage
      - /data/notebridge/backups:/data/backups
      - /data/notebridge/cache:/data/cache
```

Port 19071 matches SPC's backend port — tablet needs only hostname change, not port.

**install.sh**: Create directories, prompt for user email + password, generate jwt_secret, build and start container, health check.

**rebuild.sh**: `docker compose up -d --build --force-recreate notebridge` + health check (same pattern as UltraBridge).

**migrate.sh** (one-time SPC → NoteBridge):
1. Copy files from SPC storage → NoteBridge blob storage
2. Scan directory tree, create `files` rows with Snowflake IDs, compute MD5s
3. Export tasks from SPC MariaDB `t_schedule_task` → `schedule_tasks`
4. Export digests from SPC MariaDB `t_summary` → `summaries`
5. Copy user credentials from `u_user`

## Existing Patterns

**From UltraBridge (direct transfer):**
- Job queue with SQLite (processor pattern): single worker, atomic claim, watchdog reclaim, requeue with delay
- Pipeline detection strategies: fsnotify + reconciler + event-based (Engine.IO → event bus)
- Web handler with nil-safe dependency injection
- CalDAV via emersion/go-webdav Backend interface
- Configuration via `UB_`-prefixed env vars (becomes `NB_`-prefixed)
- install.sh / rebuild.sh deployment model

**From opennotecloud (ported to Go, same language):**
- Device sync protocol: all HTTP endpoints, request/response formats, error codes
- Challenge-response auth with MD5+SHA256 hashing
- Snowflake ID generator (2020 epoch, mutex-protected)
- Socket.IO frame handling (ping/pong, event framing)
- Sync lock with TTL and refresh

**New patterns (from allenporter, adapted to Go):**
- **Event bus**: In-process pub/sub with fire-and-forget goroutines. Replaces UltraBridge's fsnotify-as-primary with event-driven-as-primary for synced files. Simpler than allenporter's `asyncio.create_task()` — Go goroutines are natural fit.
- **JWT signed URLs with single-use nonces**: More secure than opennotecloud's reusable HMAC signatures. Nonces tracked in SQLite `url_nonces` table with TTL.
- **Soft delete / recycle bin**: File deletes move to `recycle_files` table. Prevents accidental data loss. opennotecloud hard-deletes.

## Implementation Phases

<!-- START_PHASE_1 -->
### Phase 1: Project Skeleton + Auth
**Goal:** New Go repo with build/deploy infrastructure and working device authentication.

**Components:**
- Go module init, Dockerfile (multi-stage, CGO_ENABLED=0), docker-compose.yml
- install.sh, rebuild.sh (adapted from UltraBridge)
- `internal/syncdb/schema.go` — SQLite schema creation (all tables)
- `internal/sync/snowflake.go` — Snowflake ID generator
- `internal/sync/auth.go` — challenge-response endpoints, JWT generation/validation, signed URL generation with nonces
- `internal/sync/middleware.go` — auth middleware, logging, recovery
- `internal/sync/errors.go` — SPC-compatible error codes
- `internal/sync/server.go` — HTTP router, route registration
- `internal/config/config.go` — env var loading (NB_ prefix)
- `cmd/notebridge/main.go` — entrypoint, wiring

**Dependencies:** None (first phase)

**Done when:** Container builds and starts. Tablet can authenticate (challenge-response → JWT). Auth middleware rejects invalid tokens. install.sh creates initial user.
<!-- END_PHASE_1 -->

<!-- START_PHASE_2 -->
### Phase 2: Blob Storage + File Sync
**Goal:** Tablet can upload, download, list, delete, move, and copy files.

**Components:**
- `internal/blob/storage.go` — BlobStore interface
- `internal/blob/local.go` — local filesystem implementation (Put, Get, Delete, Exists)
- `internal/sync/files.go` — sync start/end, list folder (v2+v3), upload apply/finish, download, delete (soft), query, space usage
- `internal/sync/folders.go` — create folder, directory tree operations
- `internal/syncdb/store.go` — file CRUD queries, sync lock acquire/release/refresh, nonce store/consume
- Signed URL generation and verification (JWT with nonce)
- Chunked upload support (temp chunk storage, implicit merge on final part)
- `internal/sync/files.go` — move, copy (with autorename, circular move detection)

**Dependencies:** Phase 1 (auth, schema, server)

**Done when:** Tablet can complete a full sync cycle: auth → start → list → upload/download → end. Files persist in blob storage. Signed URLs work with single-use nonces. Soft delete moves files to recycle table.
<!-- END_PHASE_2 -->

<!-- START_PHASE_3 -->
### Phase 3: Socket.IO + Event Bus
**Goal:** Real-time bidirectional communication and event-driven processing triggers.

**Components:**
- `internal/sync/socketio.go` — WebSocket upgrade at `/socket.io/`, Engine.IO handshake, ping/pong, ratta_ping, ClientMessage handling, ServerMessage push
- `internal/sync/notify.go` — client registry (track connected devices per user)
- `internal/events/bus.go` — EventBus implementation (Subscribe, Publish with goroutine dispatch)
- `internal/events/types.go` — FileUploadedEvent, FileModifiedEvent, FileDeletedEvent
- Wire `upload/finish` → publish FileUploadedEvent
- Wire Socket.IO → push ServerMessage to other connected devices on file changes

**Dependencies:** Phase 2 (file operations that trigger events)

**Done when:** Tablet maintains Socket.IO connection with keepalive. File uploads trigger events. Connected devices receive notifications when files change.
<!-- END_PHASE_3 -->

<!-- START_PHASE_4 -->
### Phase 4: OCR Pipeline Integration
**Goal:** .note files uploaded via sync are automatically OCR'd with RECOGNTEXT injection, and the tablet receives the injected version on next sync.

**Components:**
- `internal/notestore/` — copied from UltraBridge (note tracking, SHA-256, ComputeSHA256)
- `internal/search/` — copied from UltraBridge (FTS5 indexing)
- `internal/processor/` — copied from UltraBridge (job queue, worker, watchdog, OCR client), minus catalog.go
- `internal/processor/worker.go` — modified post-injection: update syncdb.files (MD5, size) instead of MariaDB, publish FileModifiedEvent
- `internal/pipeline/pipeline.go` — event listener as primary detection (FileUploadedEvent → enqueue .note files), fsnotify + reconciler as backup
- `internal/pipeline/watcher.go`, `reconciler.go` — copied from UltraBridge
- Wire: upload finish → event → pipeline → processor → injection → syncdb update → Socket.IO notify

**Dependencies:** Phase 3 (event bus, Socket.IO for notifications)

**Done when:** Tablet uploads a .note file. NoteBridge detects it, runs OCR, injects RECOGNTEXT, updates syncdb. Tablet's next sync downloads the injected version. No CONFLICT files. FTS5 search returns OCR'd content.
<!-- END_PHASE_4 -->

<!-- START_PHASE_5 -->
### Phase 5: Tasks + Digests
**Goal:** Device task lists, individual tasks, and digest/summary data sync with NoteBridge.

**Components:**
- `internal/sync/tasks.go` — schedule group CRUD (create, update, delete, list), schedule task CRUD (create, batch update, delete, list with nextSyncToken pagination)
- `internal/sync/digests.go` — summary group CRUD, summary item CRUD, summary file upload/download
- `internal/syncdb/store.go` — task and digest query methods

**Dependencies:** Phase 2 (file operations for digest file references)

**Done when:** Tasks created on tablet sync to NoteBridge and back. Digest highlights sync. Pagination with nextSyncToken works for incremental sync.
<!-- END_PHASE_5 -->

<!-- START_PHASE_6 -->
### Phase 6: CalDAV + Task Store
**Goal:** CalDAV clients can access tasks synced from the tablet.

**Components:**
- `internal/taskstore/store.go` — rewritten task CRUD against syncdb (instead of MariaDB t_schedule_task)
- `internal/caldav/` — copied from UltraBridge (CalDAV backend, VTODO conversion), wired to new taskstore
- Sync notification: CalDAV writes → update syncdb → push to tablet via Socket.IO

**Dependencies:** Phase 5 (tasks in syncdb)

**Done when:** Tasks created on tablet appear in CalDAV client (e.g., macOS Reminders). Tasks created via CalDAV sync to tablet on next sync cycle.
<!-- END_PHASE_6 -->

<!-- START_PHASE_7 -->
### Phase 7: Web UI + Polish
**Goal:** Web interface for file browsing, job management, and search. Production-ready error handling.

**Components:**
- `internal/web/` — copied from UltraBridge (handlers, templates, SSE logs), adapted for syncdb/blob storage
- `internal/logging/` — copied from UltraBridge
- Rate limiting on auth endpoints (per-IP + per-account, from allenporter's pattern)
- Edge cases: autorename collisions, circular move detection, sync lock expiry cleanup
- Health endpoint at `/health`

**Dependencies:** Phase 4 (search/processor for web UI), Phase 6 (tasks for task list view)

**Done when:** Web UI shows files from blob storage, job status from processor, search results from FTS5, task list from syncdb. Rate limiting prevents brute-force auth attempts.
<!-- END_PHASE_7 -->

<!-- START_PHASE_8 -->
### Phase 8: Migration Script
**Goal:** One-time migration from SPC to NoteBridge.

**Components:**
- `migrate.sh` (or `cmd/migrate/main.go`) — reads SPC MariaDB + filesystem, writes to NoteBridge syncdb + blob storage
- File migration: copy from SPC storage path, create `files` rows with Snowflake IDs, compute MD5s
- Task migration: export `t_schedule_task` → `schedule_tasks`
- Digest migration: export `t_summary` → `summaries`
- User migration: copy credentials from `u_user`

**Dependencies:** All previous phases (NoteBridge must be fully functional)

**Done when:** Running migrate.sh against a live SPC installation produces a NoteBridge database + blob store. Tablet pointed at NoteBridge syncs successfully with all existing files, tasks, and digests.
<!-- END_PHASE_8 -->

## Additional Considerations

**Transition period:** UltraBridge + SPC stays running during NoteBridge development and testing. NoteBridge runs on a separate IP/port. User tests tablet sync against NoteBridge. When confident, decommission SPC + UB.

**Future extensibility (v2+):**
- Embedding generation and semantic search (SQLite stores embeddings as blobs, cosine similarity in Go — works at single-user scale)
- MCP server for AI agent access to notebook content
- Multi-device backend abstraction (Boox, reMarkable) — the BlobStore + SyncStore interfaces naturally support this
- Web file management API (separate from device API)
- Recycle bin management UI
