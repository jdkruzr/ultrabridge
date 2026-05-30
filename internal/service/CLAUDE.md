# internal/service

Last verified: 2026-05-30 (TaskService write surface for URL/Priority/Categories/Comment + ForestNote provenance + hard-purge; PurgeCompleted now returns (int64, error); PurgeDeleted now returns (purged, skipped, error); ForestNoteReprocessor.Status + EmbeddingJobStatus.ForestNote; SearchService.Search gained explicit `limit` arg with service-side default/ceiling clamp)

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

- **`TaskService`** — task CRUD + bulk + completion + hard-purge. Calls
  `SyncNotifier.NotifyChange()` after every mutation so device
  pipelines can push STARTSYNC. **`Create` takes a `TaskCreate` struct**
  (not `(title, dueAt)`) so future write fields don't churn call sites;
  inputs cover `Title` (required), `DueAt`, `Detail`, `URL`, `Priority`,
  `Categories`, `Comment`. **`TaskPatch`** carries `URL`/`Priority`/
  `Categories`/`Comment` plus `Clear{URL,Priority,Comment}` sentinels —
  Categories is wholesale (nil = unchanged, non-nil incl. empty slice =
  replace). **`ListIncludingDeleted`** surfaces soft-tombstoned rows
  alongside live ones (the `Deleted bool` on each `Task` distinguishes
  them); the default `List` still hides them. **`PurgeCompleted(ctx)
  (int64, error)`** soft-deletes every completed row and returns the count
  affected (was just `error` pre-UB-3; the count is what backs the
  "Soft-deleted N completed task(s)." MCP/REST surface). **`PurgeDeleted(ctx,
  olderThanDays) (purged, skipped int64, err error)`** is the irreversible
  end of the pipeline — rejects `olderThanDays <= 0`, does NOT notify (rows
  were already tombstoned for sync); delegates to
  `TaskStore.HardDeleteOlderThan`. `skipped` counts soft-deleted rows still
  inside the safety window so callers can tell "0 purged because nothing
  was eligible" from "0 purged because the gate broke."
- **`NoteService`** — file listing (Supernote tree, Boox catalog,
  ForestNote folder tree), content, page rendering, pipeline
  start/stop, bulk import. Nil-safe: `HasSupernoteSource()` /
  `HasBooxSource()` / `HasForestNoteSource()` let callers render
  empty-state placeholders instead of panicking when a source isn't
  configured. ForestNote (a synced, filesystem-less source) is wired
  via `SetForestNoteReader(ForestNoteReader)` — a narrow interface
  over the `syncstore` mirror (`*syncstore.Store` satisfies it);
  `ListForestNoteTree` / `ListForestNotePages` derive the inventory
  live from the `fn_*` tables, and `RenderPage` renders a
  `forestnote://{nb}/{page}` path on the fly via `forestrender` (no
  disk cache).
- **`SearchService`** — FTS5+vector hybrid search, vLLM-streamed
  chat (returns `<-chan ChatResponse` for SSE), embedding
  backfill. `HasEmbeddingPipeline()` gates the chat tab.
  `Search(ctx, query, folder, sources, limit)` runs through the `rag`
  retriever (not the bare FTS index) and accepts a source-type
  facet (`supernote|boox|forestnote|digest`); each `SearchResult`
  carries its `SourceType`. The `limit` arg caps result count;
  `limit <= 0` means "use service default" (20) and anything over
  the ceiling (100) is clamped down server-side. Callers that don't
  care should pass `0` rather than try to pick a default.
- **`DigestService`** (`digest.go`) — read + delete surface over
  `digeststore` for the web Digests tab: `ListDigests(group, tag,
  page, perPage)` + `ListGroups`, plus `DeleteDigest(id)`. All-users
  reads (single-user instance). Constructed only in SPC server mode;
  the web handler holds it via `SetDigestService` (nil ⇒ tab + nav
  entry hide). `DeleteDigest` soft-deletes, de-indexes via an
  injected `DigestDeindexer` (`*digestindex.Bridge`), and pushes a
  `DELETE_DIGEST` tombstone via a `DigestTombstoneNotifier` wired
  through `SetTombstoneNotifier` (D2; `*notify.SocketNotifier`
  satisfies it structurally so `service` never imports `spcserver`).
  Soft-delete is authoritative (its error propagates); de-index +
  push are best-effort.
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
- **`TaskPatch` uses pointer fields + separate `Clear*` bools**:
  a `*time.Time` / `*string` can't distinguish "leave unchanged" from
  "clear to null". `Title` is intentionally non-clearable — CalDAV
  VTODOs require a `SUMMARY` and empty titles round-trip badly to the
  device. `Detail` and `Comment` clear on `""`. URL/Priority/Comment
  each get a `Clear*` flag (Clear wins over the value pointer when both
  are set); `Categories` is wholesale (`*[]string`: nil = unchanged,
  non-nil incl. `[]` = replace).
- **Two storage targets for write fields**: `URL` lands in
  `tasks.links` (structured column), `Priority` in `tasks.importance`
  (structured column). `Categories` and `Comment` have no structured
  column — they ride in the `ical_blob` via
  `caldav.BuildBlobWithMetadata` (Create) or
  `caldav.MergeBlobMetadataPatch` (Update). The merge layer preserves
  every other blob property (X-FORESTNOTE-*, PRIORITY, VALARM, etc.)
  so writes through the REST/MCP surface don't trample CalDAV-PUT
  history.
- **`TaskForestNote` is read-only on the service surface**: provenance
  comes in via the CalDAV PUT path (the `X-FORESTNOTE-*` extractor in
  `caldav.VTODOToTask`), never via the REST/MCP write API. The block
  is nil when no structured column is populated, so non-FN tasks drop
  the field entirely via `omitempty`. `NativeURL` is parsed from the
  blob on read and only attached when the structured columns confirm
  FN origin (a blob-only NativeURL with no column-side provenance
  would conjure a misleading block).
- **No domain logic in services**: services orchestrate stores +
  pipelines and translate types between layers. Business rules
  live in the underlying packages (e.g. CalDAV soft-delete is in
  `taskdb`, OCR scheduling is in `processor`).
- **Processor-status surface is per-source-additive**: `EmbeddingJobStatus`
  carries an optional `Boox *booxpipeline.QueueStatus` and an optional
  `ForestNote *ForestNoteQueueStatus` block (both `omitempty`). The FN
  block mirrors the syncbridge `Status` shape (Pending / InFlight /
  Processed / Dropped / Capacity) but is redeclared in `service` so the
  web JSON contract doesn't leak the `syncbridge` package name. Attached
  only when `Capacity > 0` (or any counter has moved), so deployments
  with no FN source emit no `forestnote` key at all. Counters are
  monotonic-since-process-start; the caller diffs across polls for a
  rate. `ForestNoteReprocessor.Status()` is the read path and must be
  nil-safe on both the source and the underlying bridge.

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
