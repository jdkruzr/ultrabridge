# Candidate ForestRead notedb storage (Stages 2D10–12)

Explicit, inactive adapter for the existing notedb. No production package, router or migration
runner constructs it. Tests use disposable on-disk SQLite databases alongside the existing writer
and asset schemas. This is not a new relay, acknowledgement engine, clock or asset store.

## API and transaction ownership

1. `Install(ctx, db)` atomically adds the 15 typed `fn_reader_*` / `fn_content_*` mirrors,
   non-null original LWW provenance, a version marker and the durable `reader_store_incoming`
   inbox. It validates existing column types/nullability/PKs and the unique receipt-identity
   index. Incompatibility/future versions fail without deleting/recreating the library or changing
   its `user_version`. No FK cascades or reader lifecycle tombstone compaction is introduced.
2. `Prepare(verifiedSite, rawOps)` does lossless decode/full ink validation off-writer, with
   500-operation, 8 MiB row and 16 MiB input/canonical batch limits. It privately owns payloads.
   Valid outer JSON is canonicalized without changing selector strings or int64 values; invalid
   domain rows retain their original payload for quarantine. Unusable envelope/author identities
   reject the batch. `verifiedSite` means an externally verified credential binding, NOT the
   request's own `site_id`. Current production account Basic auth cannot supply that binding.
3. `Prepared.CommitTx` inserts durable pending/quarantined receipts inside a **host-owned writer
   transaction**. Acquire the writer before the hook and roll back the whole transaction on error.
   Identical retries are idempotent; different content under `(site_id, op_seq)` fails rather than
   overwriting the first receipt. The convenience `Store.Stage` owns that transaction for headless
   tests. Neither API emits ACKs or touches existing `sync_ops` / clocks / cursors. D11 HTTP
   integration now combines receipt with the existing relay/ACK transaction in the explicit
   [reader HTTP harness](../readerlab/README.md). `RelayEntries` exposes immutable prepared
   payload/identity/rejection metadata, not mutable decoded ink or a second relay log.
4. `Drain(after, limit)` handles one keyset page (1–128 ops, at most 16 MiB queued payloads), with
   validation before acquiring the writer. Dependency lookup is cached and bounded; the prepared
   domain gate then runs on-writer. Rhizome's existing `syncstore.Less` chooses winners. Original
   `(op_ts, op_seq, site_id)` survives exactly. Mirror changes and inbox status commit together.
   `Next=0` ends a sweep; start a fresh sweep after dependencies arrive or prior progress justifies
   another pass. There is no internal busy retry loop. D12 also appends each changed table/ID
   to `reader_store_changes` in that same transaction. The returned `Changed` is only a hint;
   the journal, not the callback, is the durable downstream handoff.
5. `Snapshot(id, limits)` reads annotation/book/lifecycles and all relevant contribution winners
   in one read transaction. Byte-length probes precede blob fetches. Default limits are 4096 rows
   and 16 MiB; all loaded metadata/provenance counts. Oversize returns `ErrBudget` and no partial
   snapshot. A missing annotation returns nil. `Projection` closes that transaction before calling
   the pure reducer; no writer is held while hashing/reducing ink.

Pending inbox entries are durable but are not row winners. A successful snapshot/READY projection
does not prove that all devices' edits have arrived, that the book bytes are ready, or that a
backup is complete. Book metadata presence and verified asset readiness remain distinct.
Quarantined records remain inspectable in the inbox; no retention purge is implemented here.

## Background worker and downstream handoff (D12)

`NewWorker(store, consumer, options)` constructs an inactive owner; the host explicitly runs
`Run(ctx)`, cancels it on shutdown and waits for it before closing the database. One owner per
library is a host requirement. Concurrent `Run` on that owner is rejected. Materialization and
downstream delivery have separate serial loops, so a slow consumer cannot stall incoming-row
materialization. Consumer callbacks must honor cancellation; `OnError` must be quick/thread-safe
(it can be called from either loop). No renderer, hash, network call or consumer runs in a writer
transaction. Defaults: 32 rows/page, 10 ms yield between pages, 1 s idle poll and error backoff.

Startup scans durable pending receipts even without a notification. A coalesced `Wake()` after
successful HTTP commit reduces delay; periodic `MAX(seq)` probes recover missed hints. A sweep
repeats only after progress or new receipts; unresolved dependencies then idle without repeatedly
decoding ink or rewriting reasons. Every page yields, and wake storms cannot bypass error backoff.
Errors leave the page pending and are reported; corrupt/over-budget pages are not silently skipped.
This is not a per-row dead-letter recovery policy for manually corrupted databases.

Schema version 2 adds a key-only change journal and contiguous consumer checkpoint. Its explicit
v1 upgrade queues existing mirror keys once using SQL (no ink loading/restamping); startup does
not repeatedly backfill. The migration is atomic but can hold the writer while copying many keys;
production migration timing/backup qualification remains separate. Neither `user_version` nor
sync clocks, row versions or relay cursors are changed by this worker schema.

`Changes(limit)` detaches a bounded page before a consumer runs. A `Change` is an invalidation of
current state, not a historical row snapshot. The downstream consumer must re-read current state
and durably/idempotently schedule any affected annotation/book/reference work, including paged
fan-out for book/session/lifecycle changes, before returning success. `CompleteChange(seq)` then
advances exactly the next checkpoint; stale completion cannot discard newer jobs. Delivery is
**at-least-once**, including a crash after downstream success but before checkpoint commit.
One ordered consumer checkpoint is provided, not independent subscriptions for several systems.

With a nil consumer, materialization still runs and downstream jobs remain pending. D12 used this
setup; D13 readerlab now supplies [readersearch.Schedule](../readersearch/README.md), which durably
hands off jobs before acknowledging delivery. Its separate job cursor tracks indexing completion,
not OCR/rendering. Journal entries are
retained even after completion to avoid sequence reuse; retention/compaction is future work.
Production consumers must plug into this handoff rather than transient callbacks.

## Verification and remaining boundaries

Run `node /home/jtd/ForestNote/docs/test-plans/forestread-stage-2/run.mjs --import-dir /home/jtd/Downloads`.
It regenerates actual Kotlin row fixtures, requires all 15 tables to survive SQLite preparation,
receipt, domain drain and typed reload in four shuffled orders, and runs the package under `-race`.
The other tests cover additive/idempotent install, failed migration rollback, exact int64 storage,
restart/retry, missing dependencies, terminal-before-open, delete/restore, identity reuse,
host-transaction rollback, injected mirror/status failure, quarantine, byte/row limits, unchanged
caller buffers, concurrent receipt/drain, invalid Unicode retention and incorrect identity indexes.

Standalone `go test -race ./internal/readerstore` skips the cross-language fixture test unless
`FORESTREAD_CONTRACT_VECTORS` names freshly generated Kotlin vectors; the full runner requires
that test to execute. D11 additionally exercises mixed reader/writer HTTP relay and ACK and
actual Kotlin reader repositories through the disposable harness. Authentication enrollment,
production search federation/UI, migrations/activation and device qualification remain separate
gates. No production accepted hash or deployed service changes in these slices.

D12 adds race-tested startup/missed-hint recovery, idle dependency handling, atomic mirror/job
failure, v1 upgrade, restart after downstream success/checkpoint failure, stale/out-of-order
completion, cancellation/join, slow-consumer independence and bounded retry under wake storms.
The Kotlin HTTP harness verifies that UB materializes received ink and queues its change while
the server is still running, without invoking the explicit projection/drain command.
