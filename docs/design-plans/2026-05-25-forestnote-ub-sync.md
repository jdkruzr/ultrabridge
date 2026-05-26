# ForestNote ↔ UltraBridge Sync — Design Plan

**Status:** approved design, not yet implemented
**Date:** 2026-05-25
**Decision source:** `docs/research/2026-05-25-sync-decision.md` (why roll-our-own)
**Companion:** ForestNote client repo (`~/ForestNote`) — the Kotlin device side, built in
a separate session, implements Part D against this spec.

> This is a **design plan** (protocol spec + server architecture), not a task-by-task
> implementation plan. It spins into two implementation plans: the UB Go server (Part C)
> and the ForestNote Kotlin client (Part D). The wire protocol (Part A) + conformance
> vectors (Part B) are the contract between them.

## Context

ForestNote is a Viwoods-native Kotlin e-note app with **no sync at all** today — local
SQLDelight only. UltraBridge will be its sync server. We evaluated PowerSync / cr-sqlite /
sqlite-sync / SQLSync and chose **roll-our-own** (see the decision dossier): the deciding
factor is licensing — a likely future of *paid hosted UltraBridge instances* trips the
"managed service" clauses in PowerSync (FSL) and sqlite-sync (Elastic 2.0); the
clean-license options fail on Android viability / maturity.

**The payoff of UB-as-the-server** (why this matters beyond backup): synced ForestNote
strokes flow into UB's existing **render → OCR → index → embed** pipeline, so ForestNote
notes become full-text searchable and RAG-chat-able alongside Supernote and Boox notes.
UB is the unified note hub.

### Decisions locked

- **Single-user per UB instance.** No multitenancy, no tenant key, no RLS. (The hosted
  business is *instance-per-user*, deployed later via Docker/ECS — an ops concern that
  does not touch this spec. License rationale still holds for the hosted offering.)
- **Dual-language ironclad spec.** Server stays **Go**; client is **Kotlin**. The wire
  protocol is a language-neutral *spec*; neither implementation is the source of truth.
  **Shared conformance test vectors** keep Go and Kotlin merging identically — first-class,
  not an afterthought.
- **Merge model: simple row-level LWW + per-device op counter.** No per-column Lamport
  vectors. Tiebreak `(wall_ts, op_seq, site_id)`. Versioned envelope leaves room to add
  per-column / block-level LWW later (e.g. for future text notes) without breaking stroke
  sync.
- **Narrow wire, broad server.** The cross-vendor reconciliation (ForestNote + Boox +
  Supernote inside one user's UB) is **Go-side internal logic**, never on the dual-language
  wire.
- **Server store: SQLite** (in the existing `notedb`), behind a store interface. Postgres
  is "maybe never" for one user.
- **Canonical encodings:** ULID = 26-char uppercase Crockford base32; timestamps = int64 ms
  UTC; stroke `points` = little-endian int32 array, 5 ints/point `[x,y,pressure,tsHi,tsLo]`,
  base64 in JSON; transport = JSON over HTTPS behind UB's existing auth.

### Scope

- **In:** the wire protocol spec; conformance vectors; the full UB Go server (ingest,
  merge, relay, and the pipeline bridge that makes synced notes searchable); a frozen
  *client-requirements appendix* for the ForestNote session.
- **v1 server payoff = sync + search + RAG.** Indexing/embedding are path-keyed and need
  **no filesystem writes**, so they work fully virtual.
- **Out / deferred:** UI image surfacing of ForestNote pages in the Files tab + the
  `get_note_image` MCP tool (filesystem-coupled — needs a cache root + a `NoteService`
  branch; **v2**). Sync buckets / partial replication (single user → one bucket = all;
  envelope leaves room). Per-column / block-level LWW. Postgres / multitenancy / RLS.
  ForestNote's own non-sync feature work.

---

## Part A — The wire protocol (the dual-language ironclad core)

### A.1 Identity & encodings

- **`site_id`** — per-install device id, a ULID minted once per ForestNote installation,
  persisted locally. UB **never authors ForestNote ops** (it's a relay + mirror), so all
  ops originate on devices; conflict is only device↔device.
- **ULID** — canonical 26-char uppercase Crockford base32 (ForestNote's `Ulid.kt` already
  emits this; Go side must match exactly).
- **Timestamps** — int64 ms since Unix epoch, UTC.
- **`op_seq`** — per-device monotonic int64, starts at 1, +1 per authored op, never reused,
  persists across restarts.
- **`points` BLOB** — little-endian int32 array, 5 ints/point, base64-encoded in JSON.

### A.2 The op model — one op type

**Every op is a full-row UPSERT** carrying the row's complete current column set, including
a `deleted_at` field (`null` = live). There is **no separate delete op**: deletion sets
`deleted_at`, restoration clears it — both are ordinary upserts resolved by the same LWW
rule. This makes ForestNote's recycle-bin **restore** converge correctly, and stroke-erase /
notebook-delete / notebook-restore all obey one rule.

```
Op = {
  table:    "notebook" | "page" | "stroke",
  pk:       <ULID>,
  site_id:  <ULID>,
  op_seq:   <int64>,
  wall_ts:  <int64 ms UTC>,
  cols:     { ...full row state, incl deleted_at }
}
```

Per-table `cols`:
- **notebook**: `name, sort_order, created_at, deleted_at`
- **page**: `notebook_id, sort_order, created_at, deleted_at`
- **stroke**: `page_id, color, pen_width_min, pen_width_max, points(base64), z, created_at, deleted_at`

A device authors one op per local mutation: create / rename / reorder / delete / restore /
erase all emit a full-row upsert with the new state and the next `op_seq`.

### A.3 The sync round-trip — single call batches send+receive

`POST /sync/v1` (behind UB auth; bearer token preferred, Basic fallback):

```
Request  { protocol_version:1, schema_hash:"<hex>", site_id, cursor:<int64>, ops:[Op,...] }
Response { protocol_version:1, accepted_through:<int64>, ops:[Op,...], cursor:<int64>, has_more:bool }
```

- `cursor` (req) = last global seq this device has applied. `ops` (req) = pending local ops
  since last successful push, in `op_seq` order.
- Server applies incoming ops, then returns ops with global seq > `cursor`, **excluding this
  `site_id`** (the device already has its own), in seq order, capped at a batch limit.
- `accepted_through` = max `op_seq` from this device durably accepted → device stops
  resending those. `cursor` (resp) = new high-water. `has_more` = server capped → device
  re-calls immediately.

**Client loop:** gather local ops → POST → on 200 ack through `accepted_through`, apply
returned ops locally (idempotent), advance cursor, repeat while `has_more`. On error: retry
with backoff (ops are idempotent, so resends are safe).

### A.4 Conflict resolution (deterministic — both languages MUST agree)

Define a **total order** on ops touching the same `(table, pk)`: compare
`(wall_ts, op_seq, site_id)` lexicographically; greater wins. The row's materialized state =
the `cols` of the winning op.

**Convergence property (assert + test):** because the winner is selected by a *total order*
independent of arrival order, every replica that has seen the same set of ops for a pk
computes the same state — regardless of delivery order or duplication. This is what makes
the system correct under at-least-once, out-of-order delivery. The conformance suite must
include shuffled/duplicated op orderings that all reduce to one expected state.

- **stroke insert-union** falls out for free: distinct ULIDs ⇒ distinct pks ⇒ all survive;
  same-pk collisions are impossible across devices.
- **delete / restore**: `deleted_at` is just an LWW column; latest writer decides
  live-vs-deleted.

> **Row-level vs column-level trade-off:** with row-level LWW, two devices concurrently
> editing *different fields of the same notebook* (e.g. rename on A, reorder on B) resolve to
> one device's whole row — the other field-edit is lost. Acceptable given the benign model
> (nobody co-edits a notebook's metadata in real time). Per-column LWW is a `protocol_version:2`
> addition if a future need (text notes) justifies it.

### A.5 Schema-hash validation & the versioned envelope

- **`schema_hash`** — hash over the canonicalized table/column definitions. Server rejects
  (`409`) payloads whose hash it doesn't recognize, so a client on an unknown schema can't
  corrupt the mirror. Borrowed from sqlite-sync.
- **`protocol_version`** — unknown future fields are ignored (forward-compat). Per-column
  LWW, block-level LWW, and sync buckets are reserved `protocol_version: 2` additions; v1
  implements one global cursor and row-level LWW.

### A.6 Auth & errors

Reuse UB's existing `auth` middleware (bearer via `mcpauth`, Basic fallback). `401`
unauth, `409` schema-hash mismatch, `400` malformed op (per-op skip+log inside an otherwise
valid batch, mirroring the pipeline's "skip bad shape" tolerance), `200` success.

---

## Part B — Conformance test vectors (the ironclad mechanism)

A language-neutral suite checked into a neutral path (proposed: `docs/sync/vectors/` in UB,
mirrored to the client repo). Each vector is JSON:

```
{ name, ops:[Op,...], expected_state:{ notebook:[...], page:[...], stroke:[...] } }
```

Both the Go server and the Kotlin client run the suite in CI: feed `ops` through the merge
function, assert the materialized state equals `expected_state`. Cases must cover:
LWW tiebreaks on each axis (wall_ts, then op_seq, then site_id); delete-then-restore and
restore-then-delete; stroke union; **shuffled and duplicated op orderings converging to one
state** (the convergence property); clock-skew (lower wall_ts but higher op_seq); unknown
columns ignored. The vectors are the contract between the two sessions.

---

## Part C — UltraBridge Go server design

(Grounded by codebase investigation; file paths are real as of 2026-05-25.)

### C.1 New packages

- **`internal/syncstore`** — the mirror + ops changelog + cursors. Protocol-generic (named
  `sync*`, not `forestnote*`, so a future Boox/Supernote sync client reuses it). Holds
  `Migrate`, `Store{ApplyBatch, OpsSince, cursor CRUD}`, pure merge functions in
  `reconcile.go` (FCIS — side-effect-free LWW; the txn shell calls them).
- **`internal/syncsvc`** — service layer decoupling handler from store (mirrors
  `internal/service`'s boundary rule). `SyncService.Sync(ctx, req) (resp, error)`; owns the
  pipeline **bridge**.
- **`internal/synchttp`** — thin `http.Handler`: decode JSON → `SyncService.Sync` → encode
  JSON. Never touches the store directly.
- **`internal/source/forestnote`** — a `source.Source` adapter (virtual, **no filesystem
  watch**); `Start()` launches the bridge worker. (`source.Source` is a pure lifecycle
  interface — Type/Name/Start/Stop — with no filesystem assumption; the fs coupling lives
  only in the supernote/boox adapters.)
- **`internal/forestrender`** — NEW renderer. `booxrender` is hard-bound to Boox's
  big-endian 16-byte `TinyPoint` + ARGB/thickness model and **cannot** consume ForestNote's
  LE 5-int points + `color`/`pen_width_min/max`. Fork the `fogleman/gg` pressure-width
  approach from `booxrender/render.go:55-89`, decode LE points, map `pressure` →
  `pen_width_min..max`.

### C.2 SQLite schema (in `notedb`; WAL + `MaxOpenConns=1`; feature-gated `Migrate` like `digeststore`)

```sql
sync_seq(id=1 PK, last_seq INTEGER)                       -- global monotonic, bumped in apply txn
sync_ops(seq PK, site_id, op_seq, table_name, pk, wall_ts,
         payload TEXT, applied_at, UNIQUE(site_id,op_seq)) -- changelog + DEDUP key; client pulls from here
sync_cursors(site_id PK, last_pull_seq, last_recv_op, updated_at)
fn_notebook(id PK, name, sort_order, created_at, deleted_at,
            lww_wall_ts, lww_op_seq, lww_site_id)          -- materialized mirror + winning-op bookkeeping
fn_page(id PK, notebook_id, sort_order, created_at, deleted_at, lww_*)
fn_stroke(id PK, page_id, color, pen_width_min, pen_width_max,
          points BLOB, z, created_at, deleted_at, lww_*)
-- indexes: fn_page(notebook_id), fn_stroke(page_id, z), sync_ops(seq)
```

Every mirror row carries `lww_(wall_ts,op_seq,site_id)` of its current winner (incl. for
`deleted_at`, since deletion is just a column). Strokes get the same LWW columns — uniform
rule, no special-casing.

### C.3 Apply algorithm — `Store.ApplyBatch(ctx, []Op)` in one transaction

Per op: **validate** (known table, parseable payload, `op_seq>0`) → **dedup**
(`(site_id,op_seq)` already in `sync_ops` ⇒ skip) → **merge** (pure `resolveUpsert`: incoming
wins iff `(wall_ts,op_seq,site_id)` > stored `lww_*`; if so write `cols`+`lww_*`) →
**assign global seq** (`UPDATE sync_seq SET last_seq=last_seq+1 RETURNING last_seq`) →
**append** to `sync_ops` → record changed page pks. After commit, hand changed page pks to
the bridge. Then `OpsSince(cursor, excludeSite, limit)` →
`SELECT * FROM sync_ops WHERE seq>? AND site_id<>? ORDER BY seq LIMIT N`; response carries
`new_cursor` + `has_more`. Keep render/OCR **out** of the apply txn (single-writer
contention) — the bridge runs async.

### C.4 Pipeline bridge — stroke → render → OCR → index → embed

`internal/syncsvc/bridge.go` (owned by the forestnote source). For each changed live page:
read `fn_stroke WHERE page_id=? AND deleted_at IS NULL ORDER BY z` → `forestrender.RenderPage`
→ JPEG → OCR → index → embed. **Reuses existing interfaces unchanged** (all already bundled
in `source.SharedDeps`):
- `processor.Indexer.IndexPage(...)` with opaque `path = "forestnote://{notebook}/{page}"`
  (FTS5 keys on a string, not a real file — `internal/search/index.go`).
- `processor.OCRClient.Recognize(...)` (same vision API as Boox).
- `rag.Embedder` + `rag.EmbedStore.Save(...)` (same opaque path).
- Loop modeled on `booxpipeline/worker.go:118-171`; OCR/embed failures best-effort/logged,
  never failing the sync HTTP response.

### C.5 Wiring, config, migration

- **Route:** `mux.Handle("/sync/v1", authMW.Wrap(synchttp.New(syncSvc)))` in
  `cmd/ultrabridge/main.go` (near existing route wiring). Plain authenticated REST — **not**
  the SPC Engine.IO/Socket.IO machinery.
- **Source registration:** `registry.Register("forestnote", ...)` next to supernote/boox.
- **Config (`internal/appconfig`):** `SyncEnabled bool` (Stage-2 DB setting, no bootstrap
  env), optional `SyncBatchLimit int` (default ~500). No secret (auth token does it), no
  device-account (single user). DB-backed + a Settings UI card mirroring the existing
  "UB-as-SPC Device Sync Server" card.
- **Migration:** `syncstore.Migrate(ctx, noteDB)` gated on `cfg.SyncEnabled`, best-effort,
  same style as the SPC server-mode migrations — failure logs + disables sync, never stops UB.

---

## Part D — ForestNote client requirements (frozen appendix for the other session)

The other session implements these against the spec above; the wire contract + conformance
vectors are the handoff. **No Kotlin is written in this plan.**

1. **Oplog table (new SQLDelight migration).** Every mutation routed through the
   executor-confined `NotebookStore` appends a row to a local `outbox`/oplog **in the same
   transaction** (`NotebookStore` being the single writer is the ideal chokepoint — no
   triggers needed). Track `acked` high-water (`op_seq`) and the server `cursor`.
2. **`deleted_at` soft-delete (Area E).** ForestNote currently **hard-deletes**. Adopt
   `deleted_at` tombstone columns on notebook/page (and stroke for erase) so deletes/restores
   are syncable upserts, not row removals. This is the near-term schema overlap flagged in
   the dossier — coordinate so the column names/semantics match the spec.
3. **`site_id` + `op_seq`** persisted locally (ULID install id; monotonic counter).
4. **Sync client.** ForestNote has **no coroutines yet** — sync is where adopting
   kotlinx-coroutines + `Flow`/`StateFlow` pays off: network I/O, retry/backoff, a periodic
   sync loop, connectivity observation, and a `StateFlow<SyncStatus>` for the UI. HTTP client
   posts `/sync/v1`, applies returned ops idempotently via `NotebookStore`.
5. **Settings:** the "Sync server URL" field (already specced in the client's design handoff)
   + a bearer token.
6. **Run the conformance vectors** in the client's test suite.

---

## Open sub-decisions (resolve at implementation, flagged here)

1. **Render canvas size.** ForestNote's schema has **no page width/height**; strokes carry
   absolute device-pixel coords. *Recommendation:* v1 renders from the **stroke bounding box
   + margin** (sufficient for OCR/search, the v1 payoff). Add client-supplied page dimensions
   in the wire when v2 UI surfacing lands and faithful page geometry matters.
2. **v1 UI surfacing = out.** Search + RAG need no disk writes; the Files-tab /
   `get_note_image` path is filesystem-coupled (`internal/service/note.go:449`) and a
   type-switch in `main.go` — defer to v2 to keep v1 tight.
3. **Tombstone GC.** Single-user, few devices → retain `sync_ops` + tombstones indefinitely
   for v1; revisit GC-by-age only if the changelog grows large.

---

## Verification

- **Unit (Go):** `reconcile.go` LWW/convergence against the shared vectors;
  `ApplyBatch` dedup + idempotency; `OpsSince` cursor paging + self-exclusion.
- **Conformance:** identical vector suite green in both Go and Kotlin CI.
- **Integration (Go):** POST `/sync/v1` round-trips — push ops, pull them back from a second
  `site_id`, assert convergence; schema-hash mismatch → 409; auth → 401.
- **Pipeline:** sync a page with strokes → assert a `forestnote://…` row appears in the FTS5
  index and (if embeddings enabled) `note_embeddings`; search returns it.
- **Build/vet:** `go build/vet/test -C /home/sysop/src/ultrabridge ./...`.

## Deliverables / suggested execution split

1. **Spec doc + conformance vectors** first (the contract) — neutral, both repos consume it.
2. **UB Go server implementation plan** (Part C) — `syncstore` → `syncsvc`/bridge →
   `forestrender` → `synchttp`+wiring+config.
3. **ForestNote client implementation plan** (Part D) — handed to the other session.
