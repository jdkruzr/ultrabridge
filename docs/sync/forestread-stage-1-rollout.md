# ForestRead: coordinated Stage 1 rollout contract

Status: **specification only; no runtime, schema, dependency or deployment change** (2026-09-07).
Stage 1 snapshot above; 2026-09-08 implementation progress is in the
[Stage 2A/B headless asset and bounded-row slices](../../../ForestNote/docs/test-plans/forestread-stage-2/README.md).
Stage 2A commit `581c1f1` was rebuilt on production without activating asset routes or schema.
Stage 2B is local: opt-in bounded HTTP/SQL exchanges and authenticated capabilities pass tests against
the disposable harness. Production advertises no new capability; reader registries/migrations and
asset/capability route activation remain pending. Existing requests without the opt-in header retain
the legacy path. The bounded path commits incoming relay/mirror/HLC/ACK/cursor state only after its
encoded response fits, and notifies downstream processing only after commit.
Companions: [FN domain contract](../../../ForestNote/docs/design-plans/2026-09-07-forestread-stage-1.md),
[Rhizome assets-v1](../../../rhizome/spec/assets-v1.md), and
[pending acceptance definitions](../../../ForestNote/docs/test-plans/forestread-stage-1/README.md).
Sibling-checkout links are intentional. These documents govern the proposed addition, not the
historical pre-Rhizome protocol text or the currently released service.

2026-09-09 Stage 2D8: local, inactive [`internal/readercontract`](../../internal/readercontract/README.md)
now matches Kotlin's typed reader registry, row validation, pure ownership/dependency rules,
composite IDs and FNRI1 fingerprints. The headless runner compares the actual production writer
descriptors too, and derives (but does not accept/activate) the candidate combined schema hash.
No production route, migration, capability, accepted hash, dependency vendor or deployment changes.
Full Go annotation projection and durable reader HTTP/mirror integration remain next-stage work.

2026-09-10 Stage 2D9 completes the pure Go projection reducer, with 68 complete Kotlin/Go
projection scenarios and 12 shuffled orders per scenario. It preserves canonical stroke bytes,
raw selectors and provenance, uses Kotlin UTF-16 ordering, and derives the same cancellation,
erase, height, visibility, status and recognition-hash results. It performs no DB/network writes.
The remaining integration boundary is durable reader ingress/mirrors and bounded snapshot
acquisition, then real reader HTTP interoperability. No production activation or deployment.

Authentication qualification: current shared Basic credentials authenticate an account, not a
particular claimed `site_id`. The new pure author guard requires a host-verified site binding;
it does not implement credential enrollment or derive trust from the request. Resolve device
binding before claiming client ownership enforcement; server-recognition producers need their
own authenticated binding. Until then the candidate domain guard rejects server producer rows.

2026-09-10 Stage 2D10 adds inactive [`internal/readerstore`](../../internal/readerstore/README.md):
explicit additive installation in disposable notedb, prepared receipt, durable pending/quarantine,
typed LWW mirrors and bounded consistent snapshots. Full ink validation happens off-writer;
mirror/status updates commit together and reduction/hash happens after snapshot transaction close.
`Prepared.CommitTx` is a host-transaction seam, not a second relay or acknowledgement path.
The existing production sync schema/hash/route/migration runner is unchanged. Mixed reader/writer
HTTP relay and ACK integration, credential binding and production migration remain future work.

2026-09-10 Stage 2D11 connects that storage to the existing bounded relay/ACK transaction in the
explicit [reader HTTP harness](../../internal/readerlab/README.md). `assetlab --reader` uses fixture
credentials bound to two sites and only the derived combined candidate hash; production remains
unchanged. Actual Kotlin repositories round-trip through UB across restarts and compare the UB
projection with both clients. Mixed reader/writer gaps, durable reader rejections, identity reuse,
response-size rollback and injected commit failures are covered. Worker/search scheduling,
production credential enrollment, row-plus-asset coordination and migration qualification remain
next-stage work. Metadata ACK still does not mean original bytes are backed up.

2026-09-10 Stage 2D12 adds a restart-safe background materializer and independent at-least-once
downstream delivery loop. The explicit candidate reader schema advances to version 2: mirror
changes and key-only journal entries commit together, with a contiguous consumer checkpoint.
Existing v1 mirror keys are queued once without re-authoring. Startup/polling recover lost wake
hints; unresolved dependencies idle and failed pages back off. The readerlab process starts and
joins the worker; no actual search consumer is installed, so downstream jobs remain pending.
Kotlin HTTP tests verify materialization while the host stays running. Production schema/routes,
accepted hashes, deployment and Android lifecycle integration remain unchanged.

2026-09-12 Stage 2D13 connects the journal to durable paged search jobs and a candidate annotation
FTS index. Only current fingerprint-matched recognition alternatives, effective highlighted quotes
and book titles are indexed; stale/deleted/cancelled/unsupported projections are hidden. Snapshot
publication is guarded against newer mirror changes, with the same invalidation rules at query
time. Kotlin exercises actual sync → search → restart → ink-change invalidation over fixture HTTP.
No producer rows are authored by search. Production search federation/UI, migrations/authentication
and combined reader-row/asset qualification remain separate. See the
[current plan review](../../../ForestNote/docs/design-plans/2026-09-12-forestread-progress-review.md).

## 1. Actual integration seams

2026-09-12 D16 adds inactive `internal/syncidentity` plus explicit loopback
`assetlab --reader --reader-assets --reader-enrollment`. Dedicated credential hashes bind to
existing author sites; account-admin enrollment retries are idempotent, legacy adoption explicit,
and revoked bindings stay reserved without deleting cursor/content/provenance. Device credentials
protect candidate rows, capabilities, assets and search, but cannot manage enrollment. The Kotlin
fixture saves its private secret before submitting a hash and tests lost replies and restart.
Production enrollment/Android vault/setup, rotation/recovery and historical credential rollback
remain unimplemented. No production routes, migration runner or accepted schema hashes changed.
See [D16 protocol and activation gates](../../../ForestNote/docs/design-plans/2026-09-12-forestread-enrollment-identity.md).

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
