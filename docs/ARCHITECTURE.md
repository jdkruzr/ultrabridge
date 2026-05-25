# UltraBridge Architecture

How the pieces fit together. For a high-level feature overview, see
the project [README](../README.md). For subsystem-level deep dives
see the `CLAUDE.md` file under each `internal/*` package.

## System diagram

```
   ┌──────────────────┐                      ┌──────────────────┐
   │ Supernote device │                      │ Boox device      │
   └────────┬─────────┘                      └────────┬─────────┘
            │ SPC protocol                            │ WebDAV
            │ (Engine.IO socket + REST)               │ PUT .note
            ▼ :8089                                   ▼ :8443
┌─────────────────────────────────────────────────────────────────────┐
│ UltraBridge — one process, two listeners                            │
│                                                                     │
│  :8089  SPC server (internal/spcserver)   :8443  main app           │
│   ├─ task sync       → taskdb (SQLite)     ├─ Web UI                 │
│   ├─ file up/download→ SPC file root       ├─ CalDAV ← taskdb        │
│   ├─ digests         → digeststore         ├─ Boox WebDAV server     │
│   └─ STARTSYNC push (to-do socket event)   ├─ JSON API + MCP server  │
│                                            └─ Chat (vLLM SSE proxy)  │
│                                                                     │
│  Shared note pipelines + services                                   │
│   Supernote .note ─┐                                                │
│   Boox .note ──────┴─▶ render → OCR → FTS5 index + embedding cache   │
│                                         (RRF hybrid retriever)       │
│                                         → search, RAG chat, MCP      │
└─────────────────────────────────────────────────────────────────────┘
            │                                        │
            ▼                                        ▼
   CalDAV clients (DAVx5, …)              AI agents (MCP / Claude)
```

There's no external SPC stack and no MariaDB — UltraBridge is the SPC
server, and both listeners run in the same process. Behind a reverse
proxy the two ports need separate hostnames; see the README's
"Reverse Proxy & Device Hostnames".

### Key points

- **UltraBridge is the SPC server.** It implements the Supernote
  Private Cloud protocol (`internal/spcserver`) on its own listener
  (`:8089` by default). The Supernote connects to UltraBridge directly;
  tasks, files, and digests all sync over SPC. There's no external
  `supernote-service` and no MariaDB — the legacy SPC *client* was
  removed in 2026-05. UltraBridge wins on task conflicts.
- **Two listeners, two hostnames.** The SPC server (`:8089`) and the
  main app (`:8443` — web UI, CalDAV, Boox WebDAV, MCP) are separate
  ports. Behind a reverse proxy each needs its own hostname; see the
  README's "Reverse Proxy & Device Hostnames".
- **CalDAV is SQLite-backed.** The CalDAV subsystem reads and writes
  `internal/taskdb` (SQLite). A Supernote completion arrives over SPC,
  lands in that same taskdb, and surfaces to CalDAV clients — and a
  CalDAV completion flows back to the device the same way.
- **Boox uses WebDAV, not SPC.** Boox devices push `.note` files into
  UltraBridge's embedded WebDAV server on the main listener; no SPC
  involvement.
- **Unified search.** Both pipelines write into the same `note_content`
  FTS5 table and the same embedding store, so search and RAG chat
  cross device boundaries transparently even though the two Files
  tabs are per-source.

## Supernote notes pipeline flow

```
Supernote uploads a .note over SPC → lands in the SPC file root
         │
         ▼
   upload handler enqueues an OCR job
   (an fsnotify watcher + 15-min reconciler also sweep the file root)
         │
         ▼
   Worker picks up job
         │
         ├─ backup original (if backup path configured)
         ├─ extract existing MyScript RECOGNTEXT → index as "myScript"
         ├─ if OCR enabled:
         │    render page → JPEG → vision API → inject RECOGNTEXT
         │    index as "api"
         │    if embedding enabled: text → Ollama → vector stored
         └─ job marked done
                  │
                  ▼
           FTS5 search index + vector cache
```

## Boox notes pipeline flow

```
Boox device syncs via WebDAV
         │
         ▼
   WebDAV PUT /webdav/onyx/{model}/{type}/{folder}/{name}.note
         │
         ├─ version-on-overwrite (old file → .versions/)
         ├─ parent directories auto-created
         └─ upload callback → enqueue job
                  │
                  ▼
   Boox processor picks up job (5s poll)
         │
         ├─ parse ZIP (protobuf metadata, shapes, point files)
         ├─ extract title, device model, page count
         ├─ render each page → JPEG cache
         ├─ if OCR enabled: vision API → text
         ├─ index page text → FTS5
         ├─ if embedding enabled: text → Ollama → vector stored
         └─ job marked done
                  │
                  ▼
   Unified FTS5 search index + vector cache (shared with Supernote)
```

## Task mutation flow (CalDAV + MCP)

```
Web UI form / MCP tool call / CalDAV client PUT
         │
         ▼
   TaskService (Create / Update / Complete / Delete / PurgeCompleted)
         │
         ├─ write to internal/taskdb (SQLite)
         ├─ emit audit log line (op, auth_method, auth_label, task_id)
         └─ Notify() → SPC STARTSYNC push (server mode only)
                  │
                  ▼
         UltraBridge emits the `to-do` socket event to the device
                  │
                  ▼
         Device pulls /api/file/schedule/task/all over SPC and sees
         the change (UltraBridge wins on conflict).

A Supernote-side change flows the same way in reverse: the device
PUTs /api/file/schedule/task/list → taskdb → CalDAV clients and the
web UI see it on their next read.
```

## Service layer

Post-decoupling, the web Handler depends on four service interfaces
rather than individual stores:

- `TaskService` — task CRUD, bulk operations, partial updates, sync
  notification.
- `NoteService` — file listings (Supernote directory-tree and Boox
  flat catalog), per-file fetch, page content/rendering, processor
  controls for both pipelines, bulk delete / import / migrate.
- `SearchService` — FTS + hybrid search, chat sessions with
  RAG-augmented streaming responses.
- `ConfigService` — runtime config, sources, MCP tokens, sync status.

Each service is nil-safe: if Boox isn't configured, `HasBooxSource()`
returns false and the corresponding UI surfaces render empty-state
placeholders rather than crashing.
