# Candidate reader jobs and search (Stage 2D13)

Explicit, disposable-notedb consumer of `readerstore.Change`. Only `cmd/assetlab --reader`
installs/runs it. Production source/router/schema hashes, ordinary note search, OCR, embeddings
and Android are unchanged. Annotation identity is not squeezed into a fabricated notebook page.
Reader results have typed book/annotation IDs and the original selector; they are not new public
URIs or proof that the selector has resolved against downloaded book bytes.

## Durable processing

`Install` creates versioned local-only jobs, documents and a SQLite FTS5 index. It bootstraps jobs
from the retained change journal once, including changes an earlier consumer acknowledged.
Subsequent installs preserve completed jobs and paged progress. Incompatible schemas fail without
dropping/recreating the library. This is fixture qualification, not a deployed migration gate.

`Schedule` copies a verified journal identity into an idempotent job before returning success to
the materializer's delivery loop. That loop may then advance its handoff checkpoint; it does not
mean indexing has finished. `Run` independently processes at most 32 annotation targets per step,
with a scheduling break between steps. One owner per library is required. Book title/lifecycle
changes fan out in stable ID pages; each document/FTS update and fan-out cursor commit together.
Restart resumes that cursor. Failed jobs retain progress/error state and a retry time; another
ready job can run while one annotation is over budget. Jobs/journal are retained, not compacted.

Position changes and the reserved reference graph explicitly produce no annotation search
targets. Link resolution/backlinks and notebook-content search federation remain later adapters.

## Current-state search, not another OCR producer

`readerstore.SearchSnapshot` reads the annotation contribution winners, book/title, recognition
alternatives and change watermark in one bounded read transaction. The existing reducer then
computes visibility, effective height, anchor and FNRI1 ink fingerprint after the transaction closes.
Only visible READY projections are indexed. Sources are book title, the effective highlighted
quote, and recognition alternatives with `status=ready` and an exact input-hash match. Alternatives
retain producer/engine/model/language, client-first and newest-version-first within the class.
No recognition, stroke, row provenance, outbox, HLC or relay data is authored by search.

Publication checks for newer relevant mirror changes after the snapshot; a raced result retries
rather than overwriting the index with obsolete text. Queries use the same relevance rules to hide
dirty cached results even before jobs are scheduled. Ink, erase claims, sessions, properties,
recognition, annotation lifecycle and book changes all invalidate affected results. Unrelated
books are not globally suppressed. Pending inbox rows are not yet materialized winners; search
is current relative to the local mirror, not a promise that all devices have finished syncing.

The source snapshot shares row/byte budgets with recognition and title loading. Output is capped
at 256 KiB per annotation without truncation; oversize fails visibly and leaves a retryable job.
FTS input is quoted as literal terms, parameterized, limited to 256 bytes/16 terms; results are
limited to 1–100 (fixture HTTP uses 25). Empty input returns no results. APIs return raw strings,
not trusted HTML; a future UI must escape them. FTS ranking is BM25. There is no full-book text
index, renderer, web reader, semantic embedding or AI/OCR request in this slice.

## Harness and evidence

The readerlab host now runs both the materializer and index loop and joins both at shutdown.
`GET /reader/search?q=...&book=...` is available only in candidate mode behind the existing
fixture-site authentication. It is not advertised or mounted by the production UB source.

Seven Go tests cover matching/alternative/stale/failed recognition; quote/title search; book
fan-out and restart; delete/restore/cancellation; enqueue/index/checkpoint rollback; snapshot
races; oversized jobs without starving another book; erase/height/anchor invalidation; unsupported
selectors; literal query bounds; reconstruction from an already-acked journal; and lifecycle.
Kotlin's actual repositories also send recognition over HTTP, query it, restart UB and verify
new handwriting invalidates the old text. Run the FN Stage 2 runner for required execution and
source/vector/binary/log hashes. Production federation, UI and operational qualification remain
separate from these passing regressions.
