# internal/service

Last verified: 2026-05-26

## Purpose

Decouples HTTP handlers in `internal/web` from the concrete stores
and pipelines that back them. The web layer depends only on
service interfaces; concrete adapters live in their own packages
(`taskdb`, `notedb`, `booxpipeline`, `processor`, `search`).
New device kinds, alternate storage backends, or
test doubles plug in by satisfying these interfaces.

## Contracts

Five public service interfaces, all defined in `interfaces.go`,
all implemented by unexported structs and constructed via
`New*Service` factories:

- **`TaskService`** — task CRUD + bulk + completion. Calls
  `SyncNotifier.NotifyChange()` after every mutation so device
  pipelines can push STARTSYNC.
- **`NoteService`** — file listing (Supernote tree, Boox catalog),
  content, page rendering, pipeline start/stop, bulk import. Nil-
  safe: `HasSupernoteSource()` / `HasBooxSource()` let callers
  render empty-state placeholders instead of panicking when a
  source isn't configured.
- **`SearchService`** — FTS5+vector hybrid search, vLLM-streamed
  chat (returns `<-chan ChatResponse` for SSE), embedding
  backfill. `HasEmbeddingPipeline()` gates the chat tab.
  `Search(ctx, query, folder, sources)` runs through the `rag`
  retriever (not the bare FTS index) and accepts a source-type
  facet (`supernote|boox|forestnote|digest`); each `SearchResult`
  carries its `SourceType`.
- **`DigestService`** (`digest.go`) — read surface over
  `digeststore` for the web Digests tab: `ListDigests(group, tag,
  page, perPage)` + `ListGroups`. All-users reads (single-user
  instance). Constructed only in SPC server mode; the web handler
  holds it via `SetDigestService` (nil ⇒ tab + nav entry hide).
- **`ConfigService`** — config get/save and sources CRUD. (The
  sync-status delegate was removed with the legacy SPC client
  2026-05-25; device sync now lives entirely in `internal/spcserver`.)

## Dependencies

- **Uses**: `taskdb` (TaskStore), `booxpipeline` (BooxStore,
  BooxImporter, BooxProcessor), `processor` (Supernote pipeline),
  `notestore`, `search`, `chat`, `rag`, `appconfig`.
- **Used by**: `cmd/ultrabridge` (wires services at startup,
  passes them to `web.NewHandler`), `internal/web` (handlers).
- **Boundary**: services must NOT import `internal/web`, MUST NOT
  reach into device-specific code beyond the adapter interfaces
  declared here. Web handlers MUST go through service interfaces,
  not the underlying stores.

## Key Decisions

- **`interface{}` returns for cross-domain values** (sources,
  history, versions, content): keeps the service interfaces from
  pulling in every concrete domain type. Web handlers type-assert
  at the call site.
- **`TaskPatch` uses pointer fields + separate `ClearDueAt` bool**:
  a `*time.Time` can't distinguish "leave unchanged" from "clear
  to null". `Title` is intentionally non-clearable — CalDAV VTODOs
  require a `SUMMARY` and empty titles round-trip badly to the
  device. `Detail` clears on `""`.
- **No domain logic in services**: services orchestrate stores +
  pipelines and translate types between layers. Business rules
  live in the underlying packages (e.g. CalDAV soft-delete is in
  `taskdb`, OCR scheduling is in `processor`).

## Invariants

- Every successful `TaskService` mutation calls
  `SyncNotifier.NotifyChange()` exactly once. Bulk operations
  notify once at the end, not per-item.
- `NoteService.ListFiles` is the legacy unified entry point; new
  code should use `ListSupernoteFiles` / `ListBooxNotes` directly.
- `RetryFailed` currently iterates Boox jobs only — Supernote-side
  retry is an open gap (follow-up #17).

## Key Files

- `interfaces.go` — public types + service interfaces. Read this first.
- `task.go` — `taskService`, `TaskStore`, `SyncNotifier`.
- `note.go` — `noteService` + the four store/pipeline interfaces it depends on.
- `search.go` — `searchService` (FTS5+vector retriever, chat stream).
- `config.go` — `configService` + `SyncStatusProvider` delegate.

## Gotchas

- The note service walks two completely different storage layers
  (Supernote via filesystem + `notedb` jobs; Boox via the
  `BooxStore` SQLite catalog). `ListFiles` papers over this and is
  fragile when paths could plausibly belong to either; prefer the
  source-specific `List*` methods.
- `interface{}` returns mean type errors land at runtime in the
  web handler, not at compile time in the service. Add a unit test
  whenever you change a concrete return shape.
