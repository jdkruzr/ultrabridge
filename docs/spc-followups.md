# UB-as-SPC — consolidated follow-ups

Last updated: 2026-05-25

Single index of everything still owed on the UB-as-SPC refactor. Phases 0–4 are
complete + hardware-validated (the device fully syncs tasks and creates / downloads
/ renames / moves / copies / deletes files against UB, on UB's own dedicated
`UB_SPC_FILE_ROOT`). This is the list of what remains. Detail lives in the linked
docs/memory — this file is just the map.

Source docs: `docs/design-plans/2026-05-15-ub-as-spc-refactor.md`,
`docs/spc-protocol.md`, `docs/future-work/spc-no-analogue-features.md`,
`docs/future-work/multi-collection-task-lists.md`,
`docs/PRIVATE_CLOUD_REFERENCE.md`.

## 1. Phase 5 — remaining phased work (verification + gated teardown)

Coexistence principle: **do not tear down the legacy SPC integration until the full
stack is built and soaked.** Real-SPC flip-back stays a working escape hatch.

- **5a — Verification sweep.** Run the device against UB ≥1 week; confirm no
  device-hit endpoint 404s/5xxs (capture/NPM logs). Gate for 5b/5e.
- **5b — Catalog cutover** (deferred from Phase 4). Delete `internal/processor/catalog.go`
  + `catalog_test.go`; remove `WorkerConfig.CatalogUpdater` + the `AfterInject()` call;
  worker updates `notedb.notes.md5`/`size` directly; main.go stops passing MariaDB to
  the processor. Gated on soak. → design plan Phase 5b.
- **5e — Big cleanup PR.** Delete `internal/sync/`, `internal/tasksync/supernote/`,
  `internal/db/`, MariaDB integration tests; remove `UB_SN_*` config keys; drop
  `supernote-service` + `mariadb` from `docker-compose.yml`. Gated on full soak.
- **5c / 5d (conditional, likely skip).** Note render (`note→pdf/png`) and
  `DIGEST-SYN` events — the device doesn't hit these in the 0b capture; 5d folds into
  the Digest build if/when it happens.

## 2. Deferred first-class features (real builds — now unblocked)

- **Digests** ("summary" in the SPC API). First-class Supernote feature (user-curated
  excerpts + handwritten `.mark` annotations); **NOT** superseded by RAG. Split into
  D1 (protocol round-trip), D2 (UB-native surfacing), D3 (proactive push). Plan:
  `~/.claude/plans/okay-so-we-have-sunny-flame.md`.
  - **D1 — protocol round-trip: DONE + hardware-validated 2026-05-25.** Full
    `F_SummaryController` over `internal/digeststore` via `handlers/summary.go` (item/
    group/tag CRUD + queries + `.mark` over the OSS path). Additive (nil-DigestStore →
    old stubs). Validated on the Nomad both directions (push/pull/.mark byte-exact/
    delete/update); wire findings + fixes in `spc-protocol.md §8` and memory
    `project_spc_phaseD_digests`. **New follow-up surfaced:** digest delete is
    device-authoritative — a server-only soft-delete does NOT propagate (device
    re-asserts via `update/summary`, which UB no-ops → benign re-push divergence). A
    future UB/web-initiated digest delete (D2) needs a **tombstone** the device honors.
  - **D2 — UB-native surfacing: DONE 2026-05-26.** Digests index into the shared
    `note_content`/`note_fts`/`note_embeddings` at `digest://<uid>` (not parallel tables) via
    `internal/digestindex`; `service.DigestService` + `internal/web` `/digests` tab.
    **Tombstone: built 2026-05-26 (web-path verified; hardware-pending).** A web-initiated
    delete (`DELETE /digests/{id}` → `DigestService.DeleteDigest`) soft-deletes, de-indexes,
    and pushes a `DELETE_DIGEST` over the `digest` socket event via
    `notify.NotifyDigestDelete` so the device honors the delete (wire shape in
    `spc-protocol.md §8`). Items-only for v1 (group-delete tombstone deferred — wire shape
    not yet captured). `/api/v1/digests` MCP surface deferred (browse + search cover it).
  - **D3 — proactive `DIGEST-SYN` push (capture-gated, not built).** `notify.NotifyDigest`
    over the `digest` socket event; only if a capture shows the device needs it (it polls
    `query/summary/hash` every sync, so round-trip works without it).
  → `docs/future-work/spc-no-analogue-features.md`, `PRIVATE_CLOUD_REFERENCE.md §6`,
  memory `project_spc_phaseD_digests`, `project_spc_no_analogue_features`.
- **Multi-collection / multiple task lists.** Today all task lists collapse to one
  synthesized group; group CRUD is accepted-but-no-op. `GroupProvider` seam is ready.
  → `docs/future-work/multi-collection-task-lists.md`, memory `project_ub_multicollection_future`.

## 3. Minor gaps / known limitations (smaller, in scope)

- **Search-index lifecycle on mutation.** OCR-kick fires only on upload finish
  (`pipeline.Enqueue`). Soft-deleted notes stay searchable; moved/copied/restored
  files aren't re-indexed. Decide one consistent policy across delete/move/copy/restore.
- **Manual task sort/reorder not persisted** — `schedule/sort` endpoints are no-ops;
  tasks order by `lastModified`.
- **Chunked upload** (`/api/oss/upload/part`) — deferred (>50MB); build only if a
  device capture shows chunking. The `oss` signer already supports the fileSize term.
- **Capacity + recycle accounting** — decide whether `.recycle` counts toward quota
  (real SPC counts it; `E0333`). Check `capacity.Meter`'s dot-dir handling.

## 4. Explicitly out of scope for THIS project (acknowledged future goals)

- **Supernote Partner app support.** The entire parallel web/Partner controller
  surface (`F_FileController`, `F_FileV2Controller`, `F_FileUploadController`,
  `F_FileLocalWebController` incl. recycle-browse/restore + file search, `F_ShareController`)
  + its own login flow (`loginMethod=1` phone) + sharing semantics. A separate project.
  Point Partner at UB today → 404s.
- **Other no-analogue user-triggered features** (note export, sharing).
  Currently 404'd on weak "not seen in 0b" evidence — the 0b soak is a *passive* trace
  and under-weights user-initiated endpoints. **Re-verify each with a deliberate
  on-device action before assuming the user won't hit it.** → `spc-no-analogue-features.md`.
  (Dictionary/Reference and file "label" search were reclassified to accept-loss on
  2026-05-25 — they're backend plumbing, not user features; see that doc.)

## 5. Housekeeping

- `docker-compose.yml` stays uncommitted (carries secrets); the
  `UB_SPC_FILE_ROOT=/mnt/supernote/ub_sn_files` change rides along uncommitted.
  Both plaintext secrets can now be removed from the compose env so it can be committed
  (migration is a separate manual step, intentionally not done in the Settings-UI build):
  - `UB_SPC_DEVICE_PASSWORD` (and the other `UB_SPC_*` fields) are now editable in the web
    **Settings → UB-as-SPC Device Sync Server** card and persisted in the `settings` table.
  - `UB_MCP_API_PASS` can be replaced by `UB_MCP_API_TOKEN`, a DB-backed MCP bearer token
    created in **Settings → MCP Tokens**. `ub-mcp` sends it as `Authorization: Bearer` and
    UB's auth middleware already validates it (`mcpauth.ValidateToken`); Basic Auth via
    `UB_MCP_API_USER`/`UB_MCP_API_PASS` remains as a fallback when no token is set.
- **Stop running UB as root (future builds).** The `ultrabridge` image has no `USER`
  directive so the container runs as `uid=0` — a holdover from the real SPC running as
  root. Consequence: everything UB materializes under `UB_SPC_FILE_ROOT`
  (`NOTE/DOCUMENT/EXPORT/…` buckets, `.staging/`, `.recycle/`, uploaded `.note`s) is
  `root:root` on the host, so the operator can't manage/back-up those files without
  `sudo`, and the container holds more privilege than it needs. Fix (deferred so it
  doesn't perturb live device validation): either add `user: "1000:1000"` to the compose
  service or bake a non-root `USER` into the Dockerfile, then one-time
  `chown -R 1000:1000 /mnt/supernote/ub_sn_files` (and confirm `/data` + the OCR
  pipeline still read/write under the new uid). Owner preference: get UB off root.

## Process reminders earned this session (carry into Phase 5 + Digests)

- **Capture the wire BEFORE coding any device-hit endpoint** — decompiled
  `@ApiModelProperty` annotations lie. All three Phase 4 bugs were wrong annotations
  (`path`, `to_path`); the cleartext `:8089` tcpdump tap caught each. → `spc-protocol.md §8`.
- **UB-as-SPC uses its OWN file tree**, never the real SPC's data dir. → memory
  `project_spc_dedicated_file_tree`.
- **Verify the device actually hits an endpoint** before building it (many recycle/
  search routes are Partner/web-only). → memory `project_spc_phase5_watchlist`.
