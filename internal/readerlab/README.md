# Disposable reader HTTP integration (Stages 2D11–16)

This package is a **test harness**, reachable only through explicit `cmd/assetlab --reader`.
That command binds loopback and requires a disposable `--db`. It installs the candidate reader
tables alongside the existing writer/relay schema; it does not change production migrations,
routes, accepted hashes, compaction, authentication or deployment.

Public fixture credentials are `reader-a:readerlab` and `reader-b:readerlab`, bound respectively
to `0000000000000000000000000A` and `0000000000000000000000000B`. Credentials are checked before
the body is dispatched. The request site and every operation author must match that binding.
This tests enforcement at the host boundary, **not production device enrollment**. Account-wide
Basic credentials still cannot establish which device authored an operation.

D16 adds explicit `--reader --reader-assets --reader-enrollment`: persistent device credential
hashes replace that fixed A/B mapping. Fixture admin `assetlab:assetlab` can approve enrollment
and revoke bindings; device tokens cannot manage enrollment and Basic/MCP credentials cannot
access the enrolled reader routes. The Kotlin child keeps its random secret in a private test
sidecar, submits only its hash for approval and retries without reminting an identity. Four
mandatory process cases plus race-tested Go guards cover retry/crash/adoption/revocation.
The current combined suite is 40 portable cases, or 48 with eight corpus books. See
[D16 scope and remaining activation gates](../../../ForestNote/docs/design-plans/2026-09-12-forestread-enrollment-identity.md).
No production wiring or Android credential-vault/setup UI is added. Key rotation and historical
credential rollback remain open; a copied Bearer credential is not hardware identity.

Only the derived combined writer/reader hash and bounded `/sync/v1` are enabled. Capabilities
advertise bounded rows, not assets, in plain `--reader` mode. D14's explicit
`--reader --reader-assets` mode enables the same asset implementation and advertises
`assets-v1`, behind the existing reader-a/reader-b authentication. No second shared-account
password bypass is introduced. The default assetlab mode and existing tests are unchanged.

ForestNote's `shared-library-e2e.mjs` drives two independently killable JVM clients against
this combined mode. Fixture-only `--checkpoint METHOD:path` holds a successful response
after commit and emits a stdout event before the parent kills the server. `--reader-inspect`
checks disposable DB integrity, mirror/asset counts and annotation `n`'s actual projection,
then exits. Both flags require `--reader-assets`; neither is a production HTTP route.
Client-side crash gates live exclusively in ForestNote test sources. All three databases
survive restarts; queue scope is stable even when the fixture port changes.

D15 adds test-only `--reader-backup NEW_PATH` (SQLite `VACUUM INTO`, refuses an existing
destination), optional `--metadata-only` (strips assets from that newly created snapshot
only), and `--reader-inventory` (deterministic table hashes and verified original-book roots).
All require `--reader --reader-assets`. The running source is never stripped. Restore tests
copy the closed consistent snapshot into another new disposable file and download into a
fresh client. This is not the production backup API/policy and does not qualify recovery of
operations authored after a historical snapshot or replace migration/enrollment tests.

## Shared transaction, separate materialization

`syncstore.ExchangeReaderCandidate` prepares reader validation/canonical payloads off-writer,
checks author binding and operation identity, then uses the existing bounded exchange:

1. Reserve the writer and read the **pre-batch** acknowledgement.
2. Check identity collisions across reader and writer operations; retain reader receipts and
   append valid-shape reader operations to the existing `sync_ops` global relay sequence.
3. Apply ordinary writer rows using the existing merge path. Advance the same contiguous ACK
   walk, considering durable reader receipts when a previous shape rejection occupied a gap.
4. Update the shared HLC; build the bounded response, preserving reader int64/opaque JSON strings.
5. Commit receipts, writer mirrors, relay, HLC, ACK and cursor only after the encoded response fits.

An identical retry does not duplicate or re-author rows. A different payload under a retained
identity fails the whole candidate exchange. Malformed reader shape is durably quarantined,
reported as a rejection, and not relayed. Valid-shape pending rows are relayed with their original
provenance: each host still runs the dependency/ownership gate before projecting them. Durable
receipt ACK is **not** domain acceptance, finished processing, asset readiness or full backup.

Reader materialization is not run inside an HTTP transaction. `--reader --reader-project ID`
explicitly drains a fixture database with bounded pages/sweeps, prints the actual UB projection,
and exits without opening HTTP. Kotlin tests use this after stopping the server to compare its
computed ink hash, anchor and effective height with both client repositories. It is not a worker,
debug web endpoint, search index or production scheduling implementation.

D12 starts the [readerstore background worker](../readerstore/README.md) in `assetlab --reader`.
The handler emits only a nonblocking post-commit wake hint. The worker recovers pending rows
on startup, materializes bounded pages, idles on missing dependencies and journals mirror changes
atomically. No downstream search consumer is installed: jobs stay pending for the later consumer.
EOF/SIGINT/SIGTERM cancels and joins the worker before the shared DB closes. The projection CLI
remains a separate explicit fixture action, not necessary for background materialization.

## Verification and remaining gates

Run the FN Stage 2 runner; it builds the executable, requires all four actual Kotlin reader HTTP
tests, and runs these Go HTTP tests under `-race`. Coverage includes mixed sequence gaps,
durable rejected-reader gaps across restart, exact large integers, retries/collisions, credential
spoofing, 413 row/envelope rollback, injected receipt/relay/cursor failure, client response-commit
failure, offline drain after restart, and provenance-aware backfill without re-authoring.

D13 replaces the nil consumer with [readersearch](../readersearch/README.md): durable job handoff,
bounded/restartable annotation fan-out, fingerprint-matched recognition/title/quote FTS and an
independent index loop. The authenticated fixture route `/reader/search` returns typed IDs,
selectors and recognition alternatives. The fourth Kotlin HTTP test exercises recognition sync,
search, restart and stale-text invalidation. Both workers are joined before DB closure. There is
no production search route/UI or automatic server OCR/embedding.

Still separate: production enrollment/vault integration, production search federation/UI,
migration/rolling-upgrade/compaction and clone/reset/historical-restore recovery, and physical
devices. D14/D15 already qualify the combined coordinator and consistent fixture snapshots. Existing writer rejection
and cursor-reseed policies remain unchanged; this does not qualify arbitrary legacy rejection
gaps or pruning/compaction of candidate receipts. No production activation is implied.
