# UB Go Server — ForestNote Sync Implementation Plan

**Status:** ready to implement (deliverable #2 of the approved design).
**Date:** 2026-05-26
**Spec (the contract):** `docs/sync/forestnote-sync-protocol.md` + `docs/sync/vectors/`.
**Design (Part C):** `docs/design-plans/2026-05-25-forestnote-ub-sync.md`.
**Companion:** ForestNote Kotlin client plan (Part D) — separate session.

> Task-by-task plan for the **server** side. Two phases: **Phase 1 = the sync wire**
> (ingest + merge + relay; a device can sync and converge), independently shippable and
> testable; **Phase 2 = the pipeline bridge** (synced strokes → render → OCR → index → embed,
> the search/RAG payoff). Each phase is context-compactable: it declares its entry/exit
> state and a cold-start context list, per the repo's phase-isolation convention.

---

## Grounding (verified against the tree 2026-05-26 — not assumed)

These are the real signatures/patterns this plan builds on; cited so a cold start can trust
them without re-deriving:

- `source.Source` = `Type() string; Name() string; Start(ctx) error; Stop()` — pure
  lifecycle, **no filesystem assumption** (`internal/source/source.go:14`). A virtual source
  is legal.
- `source.SharedDeps{ Indexer, Embedder, EmbedModel, EmbedStore, OCRClient, OCRMaxFileMB,
  Logger }` (`source.go:31`) — already bundles every dep the bridge needs.
- `processor.Indexer.IndexPage(ctx, path string, pageIdx int, source, bodyText, titleText,
  keywords string) error` (`internal/processor/processor.go:20`) — **`path` is an opaque
  string**, so `forestnote://{notebook}/{page}` keys cleanly into FTS5.
- `rag.Embedder.Embed(ctx, text) ([]float32, error)`; `rag.EmbedStore.Save(ctx, notePath
  string, page int, embedding, model) error` (`internal/rag/embedder.go:17,23`) — same
  opaque path key.
- `processor.OCRClient.Recognize(ctx, jpegData []byte, prompt string) (string, error)`
  (`internal/processor/ocrclient.go:62`).
- Migration gating model: `if cfg.SPCMode == "server" { if err := digeststore.Migrate(ctx,
  noteDB); err != nil { log + disable } else { store = digeststore.New(noteDB) } }`
  (`cmd/ultrabridge/main.go:309,326`). `digeststore.Migrate(ctx, *sql.DB) error` +
  `New(*sql.DB) *Store` (`internal/digeststore/{schema,store}.go`).
- Source registry: `registry.Register("boox", func(db, row, deps) (Source, error){...})`,
  then `registry.Create(noteDB, row, deps)` + `s.Start(ctx)` over `ListEnabledSources`
  (`main.go:217,262`). A virtual source registers identically.
- Boox source adapter (`internal/source/boox/source.go`) is the model: parse `config_json`
  in `NewSource`, build the pipeline in `Start`, expose it via an accessor; `Stop` tears down.
- Route wiring: `mux.Handle("/path", authMW.Wrap(handler))` (`main.go:467,598`). `authMW`
  is `auth.NewDynamic(...)` with bearer (`mcpauth`) + Basic fallback already wired
  (`main.go:396,411`). **Plain authenticated REST — not the SPC Socket.IO machinery.**
- `booxrender.RenderPage(*booxnote.Page)` is bound to Boox's `TinyPoint` (big-endian, ARGB,
  `Thickness`) — **cannot** consume ForestNote's LE 5-int points + `pen_width_min/max`.
  `forestrender` is a real new package, not a reuse.
- notedb: SQLite WAL, `MaxOpenConns=1` (single writer). New sync tables live here.

---

## Naming discipline

Packages are `sync*` / generic (not `forestnote*`) so a future Boox/Supernote sync client
reuses the store + wire. Only the **source adapter** (`internal/source/forestnote`) and the
**renderer** (`internal/forestrender`) are ForestNote-specific. (Go note: avoid the package
name `sync` — it shadows stdlib. Use `syncstore`, `syncsvc`, `synchttp`.)

---

# Phase 1 — the sync wire

**Entry state:** `main` after the spec/vectors land. No sync code exists.
**Exit state:** `POST /sync/v1` ingests ops, merges by the spec's LWW rule, relays to other
devices, returns `accepted_through`/`rejected`/`ops`/`cursor`/`has_more`. Gated on a
`SyncEnabled` setting (default off). Two devices converge in an integration test. **No
rendering/OCR yet** — the mirror is populated but nothing reads strokes for search.
**Cold-start context:** this plan; the spec (§3–§7); the Grounding block above.

## T1 — `internal/syncstore` (mirror + changelog + cursors + pure merge)

The heart. Pure merge in `reconcile.go` (FCIS — side-effect-free); the DB shell calls it.

**Files**
- `internal/syncstore/schema.go` — `Migrate(ctx, *sql.DB) error`, idempotent `CREATE TABLE
  IF NOT EXISTS`, mirroring digeststore style.
- `internal/syncstore/op.go` — the `Op` type + JSON (de)serialization; ULID validation;
  `points` stays a base64 string at this layer (decoded only in forestrender).
- `internal/syncstore/reconcile.go` — pure: `Key(op) (wall_ts, op_seq, site_id)`;
  `Less(a,b)`; `Merge(ops []Op) map[TablePK]Op` (the materializer); `normalize(op)` (drop
  unknown cols per §3.2, reject missing known cols).
- `internal/syncstore/store.go` — `Store{db}`; `ApplyBatch(ctx, siteID, []Op)
  (ApplyResult, error)`; `OpsSince(ctx, cursor, excludeSite, limit) ([]Op, newCursor, hasMore,
  error)`; cursor get/set.
- Tests: `reconcile_test.go` (+ the **vector runner**, below), `store_test.go`.

**Schema (in notedb)**
```sql
CREATE TABLE IF NOT EXISTS sync_seq (id INTEGER PRIMARY KEY CHECK (id=1), last_seq INTEGER NOT NULL);
INSERT OR IGNORE INTO sync_seq(id,last_seq) VALUES (1,0);

CREATE TABLE IF NOT EXISTS sync_ops (
  seq        INTEGER PRIMARY KEY,        -- global monotonic, = sync_seq bump
  site_id    TEXT    NOT NULL,
  op_seq     INTEGER NOT NULL,
  table_name TEXT    NOT NULL,
  pk         TEXT    NOT NULL,
  wall_ts    INTEGER NOT NULL,
  payload    TEXT    NOT NULL,           -- the full Op as canonical JSON (relay verbatim)
  applied_at INTEGER NOT NULL,
  UNIQUE(site_id, op_seq)                -- dedup key (§7.3)
);
CREATE INDEX IF NOT EXISTS idx_sync_ops_seq ON sync_ops(seq);

CREATE TABLE IF NOT EXISTS sync_cursors (
  site_id       TEXT PRIMARY KEY,
  last_pull_seq INTEGER NOT NULL DEFAULT 0,
  updated_at    INTEGER NOT NULL
);

-- materialized mirror; every row carries its winning op's provenance triple
CREATE TABLE IF NOT EXISTS fn_notebook (
  id TEXT PRIMARY KEY, name TEXT, sort_order INTEGER, created_at INTEGER, deleted_at INTEGER,
  lww_wall_ts INTEGER NOT NULL, lww_op_seq INTEGER NOT NULL, lww_site_id TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS fn_page (
  id TEXT PRIMARY KEY, notebook_id TEXT, sort_order INTEGER, created_at INTEGER, deleted_at INTEGER,
  lww_wall_ts INTEGER NOT NULL, lww_op_seq INTEGER NOT NULL, lww_site_id TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS fn_stroke (
  id TEXT PRIMARY KEY, page_id TEXT, color INTEGER, pen_width_min INTEGER, pen_width_max INTEGER,
  points BLOB, z INTEGER, created_at INTEGER, deleted_at INTEGER,
  lww_wall_ts INTEGER NOT NULL, lww_op_seq INTEGER NOT NULL, lww_site_id TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS idx_fn_page_nb ON fn_page(notebook_id);
CREATE INDEX IF NOT EXISTS idx_fn_stroke_pg ON fn_stroke(page_id, z);
```
> `points` stored as a BLOB (decoded from base64 once on apply) so forestrender reads bytes
> directly; the relayed `payload` keeps the base64 form for byte-exact relay.

**`ApplyBatch` algorithm (one transaction), per op:**
1. **Validate** (`reconcile.normalize` + ULID/op_seq checks). Fail → append to
   `result.Rejected{site_id, op_seq, reason}`, continue. (Permanent; §7.2.)
2. **Dedup** — `(site_id,op_seq)` in `sync_ops`? → skip, mark settled (counts toward
   `accepted_through`).
3. **Merge** — load mirror row's `lww_*`; incoming wins iff `Less(stored, incoming)` (or no
   row). On win, upsert `cols` + `lww_*`; collect changed page pk (for Phase 2).
4. **Assign seq** — `UPDATE sync_seq SET last_seq=last_seq+1 RETURNING last_seq`.
5. **Append** — insert into `sync_ops` (verbatim payload).

Returns `ApplyResult{ SettledOpSeqs map[siteID][]int64 (applied∪deduped),
Rejected []RejectedOp, ChangedPages []TablePK }`. Render/OCR are **out** of this txn.

**ACs**
- Pure `Merge` matches the spec's total order; the **vector runner passes all 12 vectors**.
- `ApplyBatch` is idempotent: replaying a batch changes no mirror row and adds no `sync_ops`.
- `OpsSince` excludes the requesting site, orders by `seq`, caps at `limit`, reports
  `hasMore` correctly.

### T1a — conformance vector runner (the ironclad tie-in)

`internal/syncstore/vectors_test.go` loads every `docs/sync/vectors/*.vector.json`, feeds
`ops` through `reconcile.Merge`, and asserts the materialized winners equal `expected_state`
(set-keyed by pk; compare `cols` + provenance triple). This is the same suite the Kotlin
client runs — neither side is the source of truth. Failing vector = release blocker.
> Path: tests resolve the vectors dir relative to the repo root (walk up from the test
> file, or a `//go:embed`-friendly copy). Keep the JSON the single source — do not fork it.

## T2 — `internal/syncsvc` (service: envelope, rejected[], accepted_through, relay)

Decouples the handler from the store (mirrors the `internal/service` boundary rule).

**Files** `internal/syncsvc/service.go` (`SyncService.Sync(ctx, req) (resp, error)`),
`service_test.go`.

**`Sync` logic**
1. `protocol_version == 1` else `ErrUnsupportedVersion` (→ 409). `schema_hash ==`
   the v1 constant else `ErrSchemaMismatch` (→ 409). Envelope malformed → `ErrBadRequest`
   (→ 400). (HTTP mapping in T3.)
2. `store.ApplyBatch`.
3. **`accepted_through`** = contiguous high-water over this site's *settled* op_seqs
   (applied ∪ deduped ∪ rejected). Compute: collect settled set for `req.site_id`, walk
   `1,2,3…` until the first gap; report the last contiguous value. (Spec §4.1 — the
   contiguous-not-max rule, the bit that prevents both silent loss and poison-loop.)
4. `resp.rejected` = `ApplyResult.Rejected`.
5. `store.OpsSince(req.cursor, req.site_id, batchLimit)` → `resp.ops`, `resp.cursor`,
   `resp.has_more`.
6. Hand `ApplyResult.ChangedPages` to the bridge (Phase 2; in Phase 1 the hook is a no-op).

**ACs (service tests, not vectors — per the vectors README scope note)**
- push 3 ops where #2 is malformed → `rejected` names #2; `accepted_through == 3`
  (rejected counts as settled).
- push op_seq {1,3} (gap at 2) → `accepted_through == 1` (resend 3 later).
- second device pulls the first device's ops; first device does **not** receive its own.
- replay a batch → identical `accepted_through`, no duplicate relay.
- `has_more` paging across a `batchLimit`-sized boundary.

## T3 — `internal/synchttp` (thin HTTP handler)

**Files** `internal/synchttp/handler.go` (`New(svc) http.Handler`), `handler_test.go`.

Decode JSON → `svc.Sync` → encode JSON. Map service errors → status: `ErrBadRequest` 400,
`ErrSchemaMismatch`/`ErrUnsupportedVersion` 409, oversized body 413 (reserved; enforce a
max-bytes read), else 500. `401` is handled upstream by `authMW.Wrap` — the handler never
sees unauthenticated requests. Never touches the store directly.

**ACs:** round-trip decode/encode; each error maps to the right code; a body over the cap →
413.

## T4 (Phase 1 tail) — config + minimal wiring to make the wire live

- **`internal/appconfig`**: `SyncEnabled bool` (Stage-2 DB setting, key `sync_enabled`, no
  bootstrap env), `SyncBatchLimit int` (default 500). DB-backed via `notedb.GetSetting`,
  same pattern as the SPC settings.
- **`cmd/ultrabridge/main.go`**: gate like SPC server mode —
  ```go
  if cfg.SyncEnabled {
      if err := syncstore.Migrate(ctx, noteDB); err != nil {
          logger.Error("sync migration failed; device sync disabled", "err", err)
      } else {
          syncSvc := syncsvc.New(syncstore.New(noteDB), syncBatchLimit, bridge /*nil in P1*/, logger)
          mux.Handle("/sync/v1", authMW.Wrap(synchttp.New(syncSvc)))
      }
  }
  ```
- **Settings UI card** (`internal/web`): a "ForestNote Device Sync" card with an enable
  toggle + batch-limit field, mirroring the existing "UB-as-SPC Device Sync Server" card.
  (Auth uses the existing bearer/Basic creds — no new secret, single user.)

**Phase 1 exit ACs:** `go build/vet/test ./...` green; with `sync_enabled=true`, an
integration test posts ops from site A, pulls them as site B, mutates on B, and asserts both
converge through `/sync/v1`; schema mismatch → 409; unauth → 401.

---

# Phase 2 — the pipeline bridge (the search/RAG payoff)

**Entry state:** Phase 1 merged; mirror populates but nothing renders strokes.
**Exit state:** a synced page's strokes render → OCR → index → embed, so ForestNote notes
are full-text searchable and RAG-chat-able alongside Supernote/Boox. v1 = **no filesystem
writes** (path-keyed index/embed only); UI image surfacing is **v2, out of scope**.
**Cold-start context:** this plan; Phase 1 code; `booxrender` + `booxpipeline/worker.go` as
render/loop references; the Grounding block.

## T5 — `internal/forestrender` (NEW renderer)

**Files** `internal/forestrender/render.go`, `render_test.go`.

`RenderPage(strokes []Stroke) (image.Image, error)`. Decode each stroke's LE int32 points
(5/point `[x,y,pressure,tsHi,tsLo]`); draw with `fogleman/gg` segment-by-segment with
pressure→width modulation (port the approach from `booxrender/render.go:54-95`, but read
`pen_width_min/max` + ForestNote pressure scale, not Boox `Thickness`/EMR-4095). Canvas:
**v1 renders from the stroke bounding box + margin** (ForestNote has no page width/height yet
— open sub-decision #1 in the design; sufficient for OCR/search). White background; skip
strokes with <2 points (mirror booxrender's tolerance).

**ACs:** a 2-point stroke renders a visible segment; empty page → blank canvas, no error;
golden-ish pixel sanity (non-white pixel count > 0 for a known stroke).

## T6 — `internal/source/forestnote` (virtual source) + the bridge

**Files** `internal/source/forestnote/source.go`, `internal/syncsvc/bridge.go`,
`bridge_test.go`.

- **Source adapter** — implements `source.Source`. `Type()="forestnote"`; `NewSource(db,
  row, deps)` parses (minimal) `config_json`; **`Start()` launches the bridge worker** (a
  goroutine draining a changed-page queue); `Stop()` cancels it. **No filesystem watch** —
  this source is fed by `/sync/v1`, not a directory.
- **Bridge** (`syncsvc/bridge.go`, driven by the source) — for each changed **live** page
  (`fn_page.deleted_at IS NULL`): read `fn_stroke WHERE page_id=? AND deleted_at IS NULL
  ORDER BY z` → `forestrender.RenderPage` → JPEG → `OCRClient.Recognize` →
  `Indexer.IndexPage(ctx, "forestnote://{notebook}/{page}", 0, "forestnote", text, "", "")`
  → `Embedder.Embed` + `EmbedStore.Save(ctx, path, 0, vec, model)`. Loop modeled on
  `booxpipeline/worker.go:114-203`. OCR/embed failures are **best-effort, logged, never**
  fail the sync HTTP response (decoupled from the apply txn — §7 isolation).
- **Connect it:** `syncSvc` enqueues `ApplyResult.ChangedPages` to the bridge after commit;
  register `registry.Register("forestnote", ...)` next to supernote/boox in `main.go`.

**ACs (integration):** sync a notebook+page+two strokes → a `forestnote://…` row appears in
the FTS5 index; search returns it; with embeddings enabled, a `note_embeddings` row exists.
A delete op (stroke erased / page deleted) → that content drops out of subsequent renders;
deleting a page removes/skips its index entry.

## T7 — full verification

- **Unit:** `reconcile` vectors (T1a); `ApplyBatch` dedup/idempotency; `OpsSince` paging +
  self-exclusion; `forestrender` basics.
- **Service/HTTP:** `accepted_through` contiguity + `rejected` (T2); error→status (T3).
- **Integration:** two-site convergence over `/sync/v1`; bridge → FTS5/embeddings.
- **Build/vet:** `go build -C /home/sysop/src/ultrabridge ./... && go vet ./... && go test ./...`.

---

## Risks / watch-outs

- **Vector path resolution in CI** — the Go runner must find `docs/sync/vectors/`; resolve
  relative to repo root, don't fork the JSON.
- **Single-writer contention** — notedb is `MaxOpenConns=1`. Keep `ApplyBatch` txns short;
  the bridge (render/OCR) runs *after* commit, off the sync path, or it will serialize
  writes and stall `/sync/v1`.
- **`accepted_through` is the subtle correctness point** — get the contiguous-over-
  (settled) computation right (T2); it's the §7 fix. Cover the gap and the poison-op cases.
- **Open wire types (spec §9)** — `pen_width`/`color`/`points` int widths must be confirmed
  against ForestNote's real schema before the client ships; a change is a one-line edit to
  spec §3.1 + the canonical string + `schema_hash`, and a re-hash here.
- **Don't reach for SPC machinery** — this is plain authenticated REST; no Engine.IO/
  Socket.IO, no device-account, no OSS signing.

## Suggested commit/PR sequencing

Phase 1 as one PR (syncstore → syncsvc → synchttp → config/wiring → Settings card), Phase 2
as a second (forestrender → forestnote source + bridge → wiring). Each PR ends green on
`build/vet/test`. The conformance-vector test (T1a) lands with Phase 1 and is the gate that
keeps the Go and Kotlin merges identical.
