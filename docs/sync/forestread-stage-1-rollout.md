# ForestRead: coordinated Stage 1 rollout contract

Status: **specification only; no runtime, schema, dependency or deployment change** (2026-09-07).
Stage 1 snapshot above; 2026-09-08 implementation progress is in the
[Stage 2A headless asset slice](../../../ForestNote/docs/test-plans/forestread-stage-2/README.md).
The new host adapter/harness is not registered in the production router and advertises no capability.
Companions: [FN domain contract](../../../ForestNote/docs/design-plans/2026-09-07-forestread-stage-1.md),
[Rhizome assets-v1](../../../rhizome/spec/assets-v1.md), and
[pending acceptance definitions](../../../ForestNote/docs/test-plans/forestread-stage-1/README.md).
Sibling-checkout links are intentional. These documents govern the proposed addition, not the
historical pre-Rhizome protocol text or the currently released service.

## 1. Actual integration seams

UB currently uses `github.com/jdkruzr/rhizome/server-go` via the local
`third_party/rhizome-server-go` replacement. Changing the sibling Rhizome repository alone does
NOT update UB. FN separately consumes published Kotlin artifacts; Stage 1 changes neither.

UB's `internal/synchttp` owns the production HTTP wrapper and currently defaults to a 64 MiB
request cap. `internal/syncstore` owns SQL relay/mirror storage, provenance and compaction;
`internal/source/forestnote` joins them to source processing. Adding a route only to the generic
Rhizome example server will not enable the feature in UB. New bounded-rows limits are opt-in and
must not silently replace the legacy request contract for older clients.

## 2. Ownership and durable state

The new server asset adapter stores descriptors, 256 KiB chunk BLOBs and durable transfer state
in UB's existing notedb, separate from taskdb and the base64 row relay. This matches the single
DB backup model and avoids introducing an untracked file tree as the only asset copy. Table names
are host-specific; asset identity and transactional verification semantics follow assets-v1.
Caches used by future book management/search may be recreated; original bytes may not.

Register authenticated capability and asset routes through UB's existing sync authentication,
not the unauthenticated static-file path. Reuse account scope and enforce size, digest, index and
state rules before writing. Do not resolve metadata-supplied paths or fetch book-provided URLs.
Network activity and complete-asset hashing must not hold the notedb writer transaction.

The upgraded FN mirror retains raw domain rows and their LWW provenance, then derives effective
annotation/ink state using the same rules as Kotlin. Do not make the server arbitrate conflicts.
Missing parent/session/asset/anchor rows remain pending; arrival order must not delete children.
Mirror updates and relay durability preserve existing atomicity, dedup, sequence and ACK rules.

Use per-producer recognition rows with input fingerprints. Search only projects current results;
do not overwrite client recognition with server text or turn the combined search body into a new
recognition author. Reference source/target indexes permit later backlink queries, but the link
picker, graph/backlinks UI, user-facing anchor routes, book-management UI and web reader are not
implemented by this stage.

## 3. Compatibility matrix and activation order

| Client / server | Metadata sync | Assets / reader behavior |
|---|---|---|
| Existing FN / existing UB | Unchanged | No assets capability. |
| Existing FN / upgraded UB | Legacy accepted hash remains supported; existing notes continue. | Client ignores unknown reader tables normally; its row cursor is NOT a content-download ACK. |
| Reader FN / upgraded UB | New hash accepted and both capabilities verified. | Local-first import plus resumable upload/download. |
| Reader FN / old/rolled-back UB | Explicit unsupported-capability/schema status; keep queues and local library intact. | Local reading/editing works. Do not upload a filtered outbox under the old hash. |
| Old FN upgraded after passing reader ops | One-shot schema cursor reconciliation + provenance-aware join. | Recover current reader rows then discover/download referenced assets. |

Implementation/deployment order (all AFTER Stage 1):

1. Implement generic asset storage/transport + bounded rows in Rhizome Kotlin/Go and execute the
   pending generic fixtures. Keep legacy conformance green. Publish a version only after evidence.
2. Update UB's vendored dependency deliberately, add host SQL/HTTP adapters and FN mirrors, then
   run registry parity and metadata/asset crash tests against disposable databases.
3. Derive the new application hash from the implemented Kotlin/Go registries. Assert exact parity
   in FN, Rhizome's FN examples/fixtures where applicable, UB vendor and UB native known-column
   declarations. Keep the supported legacy hash in the grace set. Do not insert a guessed hash.
4. Gate capability advertisement on successfully initialized storage, routes and mirror schema.
   Backup the existing notedb consistently, then deploy UB first. Verify an actual legacy client
   still synchronizes notes before activating any reader client.
5. Update FN's Kotlin dependency and migrate disposable copies of current `.forestnote` libraries.
   Preserve old notes, text, settings, IDs, pending ops, site identity and HLC/cursors. Migration
   failure must leave a recoverable original, never invoke delete-and-recreate recovery.
6. Install certificate-matched reader-capable FN builds in place after fixture/migration gates.
   Verify local-only use, capability handling, actual Mini↔UB↔Go byte/stroke round trips and normal
   writer sync. Do not uninstall or clear data to bypass signing mismatch.

Retire a legacy hash only in a separate explicit rollout decision after the fleet is known to be
upgraded; Stage 2 completion does not authorize dropping it. Reference tables may be prepared
with the reader schema, but no client should create new reference records until resolver/storage
tests pass and the later linking feature is intentionally enabled.

## 4. Recovery, ordering, and compaction

Keep pull-first joins and `backfillUntracked` for received rows. Schema upgrades re-pull once;
retries do not mint new identities, reset the cursor repeatedly, or re-author the downloaded
library. Existing ordinary-note concurrency behavior remains unchanged.

Shared book/annotation/reference lifecycle and editing-session states are semantic records with
no registry tombstone. UB's registry-derived tombstone map must exclude them. Test that generic
compaction can collapse superseded versions but retains the winning cancellation/delete state,
including after old clients have advanced their cursors. Do not compact alternative per-session
property contributions into one winning annotation value: a later cancellation may need them.

No automatic asset GC, no cascade from deleted books/anchors into references, and no reference
resolution that silently restores a book or guesses a replacement edition. The initial reader
milestone offers soft delete/restore; irreversible book purge is not exposed until a later safe
retirement protocol exists. Existing NOTEBOOK purge behavior is not changed by this reader work.

Backup consists of a consistent notedb snapshot including assets, relay, provenance, mirror and
transfer state. Verify restored bytes, not just table counts. A metadata-only restore is incomplete:
mark content unavailable and let a replica with a verified asset re-upload it. Do not use a stale
historical server-ready ACK as proof after server restore/rollback; re-query asset state.
Interrupted staged uploads remain resumable. A failed final root hash is invalid, never ready.
Disk-full/auth failures must not acknowledge uncommitted chunks or prune metadata queues.

Rollback: disable new capability advertisement before reverting handlers. Do not downgrade an
already-migrated database in place or install an old binary over a new schema as a recovery plan.
Use a matched pre-upgrade DB/software backup for full rollback, retain the failed/new DB, and
report reader sync unavailable. Devices retain local data and discover missing assets on recovery.

## 5. Acceptance and observability

Required UB acceptance IDs are in the FN pending case catalog (`UB-*` and `COMP-*`). The generic
transport catalog lives in Rhizome; do not fork a second copy under `third_party` during Stage 1.
Later adapters run both catalogs against the actual SQL/HTTP host, not only the toy relay.

Record structured transfer ID/hash, direction, verified bytes, state, retry/error code, metadata
cursor and ACK high-water separately. Never log full book payloads, handwriting or credentials.
Expose metadata-pending, asset-uploading, asset-downloading, verifying, ready and failure as
distinct host statuses. Avoid a global green "synced/backed up" while original bytes are absent.

Release gates: existing note conformance/parity; new Kotlin/Go/UB behavioral fixtures; migration
preservation and replay; interrupted transfer/restart; same-file dedup; conflicting sessions;
trash/late ink; anchor/reference pending/restore behavior; consistent backup/restore; bounded
transfer memory and writer responsiveness; physical cross-device acceptance. Fixture shape
validation alone satisfies NONE of these behavioral gates.
