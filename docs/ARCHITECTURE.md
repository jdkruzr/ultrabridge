# UltraBridge Architecture

How the pieces fit together. For a high-level feature overview, see
the project [README](../README.md). For subsystem-level deep dives
see the `CLAUDE.md` file under each `internal/*` package.

## System Diagram

```text
Supernote device       Boox device       ForestNote        reMarkable
SPC protocol           WebDAV PUT        /sync/v1          rM protocol
(Engine.IO + REST)     .note files       JSON sync         sync/search/HWR
       |                   |                 |                  |
       v :8089             v :8443           v :8443            v :8443

+---------------------------------------------------------------------+
| UltraBridge - one process, two listeners                            |
|                                                                     |
| :8089 SPC server (internal/spcserver)     :8443 main app            |
| - task sync -> taskdb (SQLite)            - Web UI                  |
| - file up/download -> SPC file root       - CalDAV <- taskdb        |
| - digests -> digeststore                  - Boox WebDAV server      |
| - STARTSYNC push                          - ForestNote /sync/v1     |
|                                           - reMarkable protocol     |
|                                           - JSON API + MCP server   |
|                                           - Chat (vLLM SSE proxy)   |
|                                                                     |
| Shared note pipelines + services                                    |
| - Supernote .note                                                   |
| - Boox .note              -> render -> OCR -> FTS5 + embeddings     |
| - ForestNote pages                         RRF hybrid retriever     |
| - reMarkable docs                         -> search, chat, MCP      |
+---------------------------------------------------------------------+
       |                                             |
       v                                             v
CalDAV clients (DAVx5, ...)                 AI agents (MCP / Claude)
```

There is no external SPC stack and no MariaDB. UltraBridge is the SPC
server, and both listeners run in the same process. Behind a reverse
proxy the two ports need separate hostnames; see the Supernote user
docs.

## Key Points

- **UltraBridge is the SPC server.** It implements the Supernote
  Private Cloud protocol (`internal/spcserver`) on its own listener
  (`:8089` by default). The Supernote connects to UltraBridge directly;
  tasks, files, and digests all sync over SPC. There is no external
  `supernote-service` and no MariaDB. The legacy SPC client was removed
  in 2026-05. UltraBridge wins on task conflicts.
- **Two listeners, two hostnames.** The SPC server (`:8089`) and the
  main app (`:8443` for web UI, CalDAV, Boox WebDAV, MCP, ForestNote,
  and reMarkable) are separate ports. Behind a reverse proxy each needs
  its own hostname.
- **CalDAV is SQLite-backed.** The CalDAV subsystem reads and writes
  `internal/taskdb`. A Supernote completion arrives over SPC, lands in
  that same taskdb, and surfaces to CalDAV clients. A CalDAV completion
  flows back to the device the same way.
- **Boox uses WebDAV, not SPC.** Boox devices push `.note` files into
  UltraBridge's embedded WebDAV server on the main listener.
- **ForestNote uses `/sync/v1`.** ForestNote devices sync through a
  JSON sync endpoint on the main listener. The source owns mirror state,
  relay-log compaction, device pruning, page rendering, and client OCR
  handoff into the shared index.
- **reMarkable uses a protocol surface on the main listener.** The
  source owns pairing, tokens, blob/document storage, render/OCR queues,
  tablet search compatibility, and optional MyScript HWR proxying.
- **Unified search.** Source pipelines write into the same
  `note_content` FTS5 table and embedding store, so search and RAG chat
  cross device boundaries transparently even though Files tabs are
  per-source.

## Supernote Notes Pipeline Flow

```text
Supernote uploads a .note over SPC -> lands in the SPC file root
         |
         v
   upload handler enqueues an OCR job
   (an fsnotify watcher + 15-min reconciler also sweep the file root)
         |
         v
   Worker picks up job
         |
         +- backup original (if backup path configured)
         +- extract existing MyScript RECOGNTEXT -> index as "myScript"
         +- if OCR enabled:
         |    render page -> JPEG -> vision API -> inject RECOGNTEXT
         |    index as "api"
         |    if embedding enabled: text -> Ollama -> vector stored
         `- job marked done
                  |
                  v
           FTS5 search index + vector cache
```

## Boox Notes Pipeline Flow

```text
Boox device syncs via WebDAV
         |
         v
   WebDAV PUT /webdav/onyx/{model}/{type}/{folder}/{name}.note
         |
         +- version-on-overwrite (old file -> .versions/)
         +- parent directories auto-created
         `- upload callback -> enqueue job
                  |
                  v
   Boox processor picks up job (5s poll)
         |
         +- parse ZIP (protobuf metadata, shapes, point files)
         +- extract title, device model, page count
         +- render each page -> JPEG cache
         +- if OCR enabled: vision API -> text
         +- index page text -> FTS5
         +- if embedding enabled: text -> Ollama -> vector stored
         `- job marked done
                  |
                  v
   Unified FTS5 search index + vector cache
```

## ForestNote Sync Flow

```text
ForestNote device POSTs /sync/v1
         |
         v
   sync service validates/authors ops
         |
         +- syncstore mirror updates notebook/page/text/task state
         +- relay log records accepted ops for other devices
         +- compaction prunes acknowledged relay history
         `- page text feeds note_content + embedding backfill
                  |
                  v
       ForestNote Files tab, Search, RAG chat, MCP, and task provenance
```

## reMarkable Source Flow

```text
reMarkable device sync/search/HWR routes on :8443
         |
         v
   source-specific protocol handlers
         |
         +- register devices and tokens
         +- store document metadata and blob payloads
         +- render supported notebook pages
         +- enqueue OCR for searchable text
         +- serve tablet search compatibility payloads
         `- optionally proxy native HWR to MyScript
                  |
                  v
       reMarkable Files tab, Search, RAG chat, and tablet sync/search
```

## Task Mutation Flow

```text
Web UI form / MCP tool call / CalDAV client PUT
         |
         v
   TaskService (Create / Update / Complete / Delete / PurgeCompleted)
         |
         +- write to internal/taskdb (SQLite)
         +- emit audit log line (op, auth_method, auth_label, task_id)
         `- Notify() -> SPC STARTSYNC push (server mode only)
                  |
                  v
         UltraBridge emits the `to-do` socket event to the device
                  |
                  v
         Device pulls /api/file/schedule/task/all over SPC and sees
         the change (UltraBridge wins on conflict).

A Supernote-side change flows the same way in reverse: the device
PUTs /api/file/schedule/task/list -> taskdb -> CalDAV clients and the
web UI see it on their next read.
```

## Service Layer

Post-decoupling, the web Handler depends on service interfaces rather
than individual stores:

- `TaskService` - task CRUD, bulk operations, partial updates, and sync
  notification.
- `NoteService` - file listings and detail views for Supernote, Boox,
  ForestNote, and reMarkable; per-file/page fetch; rendering; OCR
  controls; bulk actions; and source-specific maintenance.
- `SearchService` - FTS + hybrid search and chat sessions with
  RAG-augmented streaming responses.
- `ConfigService` - runtime config, sources, MCP tokens, sync status,
  and source/device settings.

Services are nil-safe: if a source is not configured, its UI surfaces
render empty-state placeholders rather than crashing.
