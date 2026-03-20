# Content Hash Deduplication -- Test Requirements

**Design plan:** `docs/design-plans/2026-03-20-content-hash-dedup.md`
**Implementation phases:** `phase_01.md`, `phase_02.md`
**Generated:** 2026-03-20

---

## Automated Tests

Every row below maps one acceptance criterion to a concrete test. Test type
reflects the implementation plan's testing strategy: "unit" tests exercise a
single method in isolation with an in-memory SQLite DB; "integration" tests
wire multiple components together (pipeline + notestore + processor) against
the same in-memory DB.

### content-hash-dedup.AC1: Move/rename detection skips re-processing

| AC | Criterion | Type | File | Function | Phase | Notes |
|----|-----------|------|------|----------|-------|-------|
| content-hash-dedup.AC1.1 | File moved to new path, unchanged content -- job transferred, not re-enqueued | integration | `internal/pipeline/pipeline_test.go` | `TestPipeline_MoveDetection_JobTransferred` | 2, Task 4 | Writes identical content at pathB after pathA is done; asserts pathB job is `done` (transferred) and pathA job is gone. |
| content-hash-dedup.AC1.2 | File renamed (same directory, different name) -- same behavior as move | integration | `internal/pipeline/pipeline_test.go` | `TestPipeline_MoveDetection_JobTransferred` | 2, Task 4 | Rename is structurally identical to move (SHA-256 is content-only, no filename component). The existing `TestPipeline_MoveDetection_JobTransferred` covers this because pathA and pathB differ only in filename within the same directory. A separate rename-specific test adds no value. |
| content-hash-dedup.AC1.3 | New path's `notes.sha256` set after transfer | integration | `internal/pipeline/pipeline_test.go` | `TestPipeline_MoveDetection_JobTransferred` | 2, Task 4 | Same test asserts `SELECT sha256 FROM notes WHERE path=pathB` equals the computed hash. |
| content-hash-dedup.AC1.4 | File moved AND content changed -- hash mismatch, enqueued normally | integration | `internal/pipeline/pipeline_test.go` | `TestPipeline_MoveDetection_ContentChanged` | 2, Task 4 | Writes different content at pathB; asserts pathB job is `pending` (new enqueue, not transferred). |

### content-hash-dedup.AC2: Mtime-only changes don't trigger re-processing

| AC | Criterion | Type | File | Function | Phase | Notes |
|----|-----------|------|------|----------|-------|-------|
| content-hash-dedup.AC2.1 | File `touch`ed (mtime changed, content unchanged) with existing done job -- not re-enqueued | integration | `internal/pipeline/pipeline_test.go` | (existing reconciler test) | 2, Task 4 | The done-status guard fires before hash computation. Phase 2 implementation notes state this is covered implicitly by the existing reconciler test that calls `reconcile()` twice on the same file and asserts no duplicate enqueue. No new test needed -- the guard path is unchanged. See "Rationalization" section below. |

### content-hash-dedup.AC3: Worker stores hash on completion

| AC | Criterion | Type | File | Function | Phase | Notes |
|----|-----------|------|------|----------|-------|-------|
| content-hash-dedup.AC3.1 | Job completes with OCR applied -- sha256 is hash of post-injection file | unit | `internal/processor/worker_test.go` | `TestWorker_StoresHash_WithOCR` | 1, Task 4 | Uses `mockOCRServer` to enable OCR; compares stored sha256 to `ComputeSHA256(path)` after processing (post-injection). |
| content-hash-dedup.AC3.2 | Job completes without OCR -- sha256 is hash of original file | unit | `internal/processor/worker_test.go` | `TestWorker_StoresHash_NoOCR` | 1, Task 4 | OCR disabled; verifies sha256 is set and matches `ComputeSHA256(path)`. |
| content-hash-dedup.AC3.3 | Job fails -- sha256 not written | unit | `internal/processor/worker_test.go` | `TestWorker_NoHashOnFailure` | 1, Task 4 | Seeds a job for a nonexistent file path; asserts `notes.sha256` is NULL/empty after processing. |

### content-hash-dedup.AC4: Job transfer integrity

| AC | Criterion | Type | File | Function | Phase | Notes |
|----|-----------|------|------|----------|-------|-------|
| content-hash-dedup.AC4.1 | Transferred job retains `ocr_source`, `api_model`, `status=done` | unit | `internal/notestore/store_test.go` | `TestTransferJob` | 2, Task 2 | Seeds a done job with `ocr_source='api'`, `api_model='test-model'`; after `TransferJob`, asserts all three fields match on the new path. |
| content-hash-dedup.AC4.2 | Old path's job record is gone after transfer | unit | `internal/notestore/store_test.go` | `TestTransferJob` | 2, Task 2 | Same test asserts `SELECT COUNT(*) FROM jobs WHERE note_path=oldPath` returns 0. |

### Supporting method tests (not directly AC-mapped but required infrastructure)

These tests verify the NoteStore methods that the AC tests depend on. They are
listed here for completeness -- each is exercised as part of the AC coverage
above but also runs independently.

| Test | Type | File | Function | Phase | Supports |
|------|------|------|----------|-------|----------|
| SetHash writes sha256 column | unit | `internal/notestore/store_test.go` | `TestSetHash` | 1, Task 2 | AC3.*, AC1.3 |
| LookupByHash returns match with done job | unit | `internal/notestore/store_test.go` | `TestLookupByHash_Found` | 1, Task 2 | AC1.1 |
| LookupByHash returns false for unknown hash | unit | `internal/notestore/store_test.go` | `TestLookupByHash_NotFound` | 1, Task 2 | AC1.4 |
| LookupByHash returns false when no job exists | unit | `internal/notestore/store_test.go` | `TestLookupByHash_NoJob` | 1, Task 2 | AC1.4 edge case |
| LookupByHash returns false for pending job | unit | `internal/notestore/store_test.go` | `TestLookupByHash_PendingJob` | 1, Task 2 | AC1.4 edge case |
| TransferJob errors when no job exists | unit | `internal/notestore/store_test.go` | `TestTransferJob_NoJob` | 2, Task 2 | Error path safety |

---

## Human Verification

### content-hash-dedup.AC1.2: File renamed (same directory, different name)

**Justification:** AC1.2 is architecturally identical to AC1.1. The SHA-256 hash
is computed over file content only -- the path/filename is not part of the hash
input. The automated test `TestPipeline_MoveDetection_JobTransferred` already
uses two different filenames in the same directory. However, the design plan
calls out AC1.2 separately to confirm there is no path-dependent behavior in
production. A quick manual smoke test on a running system provides additional
confidence.

**Verification approach:**
1. Start UltraBridge with a configured notes directory.
2. Place a `.note` file, wait for processing to complete (check web UI or logs for "done" status).
3. Rename the file within the same directory (e.g., `mv notes/a.note notes/b.note`).
4. Wait for the next reconciler cycle (up to 15 minutes) or trigger a manual scan.
5. Confirm in the logs: "detected moved file, transferred job" message with old and new paths.
6. Confirm in the web UI: `b.note` shows as processed (done), no new pending job was created.

### content-hash-dedup.AC2.1: Mtime-only change with existing done job

**Justification:** This criterion tests a non-regression: the existing done-status
guard must continue to fire before the new hash-computation code. The automated
test suite covers this implicitly (the done-status guard is unchanged, and
existing reconciler tests verify it). However, in production the mtime change
could come from filesystem-level operations (sync tools, backup software) that
are difficult to simulate in unit tests. Manual verification confirms the
end-to-end behavior.

**Verification approach:**
1. Start UltraBridge, let a `.note` file process to completion.
2. Run `touch /path/to/file.note` to update mtime without changing content.
3. Wait for the next reconciler cycle.
4. Confirm in the logs: no "enqueue" or "detected moved file" messages for the file. The done-status guard should silently skip it.
5. Confirm in the web UI: the file still shows as "done" with no new job activity.

---

## Rationalization Against Implementation Decisions

### Hash computation gated behind mtime check
The design states that SHA-256 is only computed for files whose mtime differs
from the DB value. This means `ComputeSHA256` is never called for truly
unchanged files. The automated tests verify this indirectly:
`TestPipeline_MoveDetection_JobTransferred` only triggers hash computation
because pathB is a new file (no prior mtime in the DB). The AC2.1 case (mtime
unchanged, done job exists) never reaches hash computation because the
done-status guard returns first.

### Best-effort hash check in enqueue
The implementation plan makes hash-based detection best-effort: any failure in
`ComputeSHA256`, `LookupByHash`, or `TransferJob` falls through to normal
enqueue. This means move detection can silently degrade to re-processing. The
automated tests verify the happy path (transfer succeeds) and the content-changed
path (no match found, normal enqueue). The degraded path (e.g., `TransferJob`
fails) is not explicitly tested because the fallback is the pre-existing
behavior (normal enqueue), which is already covered by the existing test suite.

### pruneOrphans race condition
Phase 2 documents a known limitation: if `Scan()` calls `pruneOrphans` before
`enqueue()` processes the new path, the old path's job may already be deleted,
causing `LookupByHash` to return `found=false`. In this case, the file is
re-processed normally. This race is inherent to the reconciler's batch model
and is not tested -- it is documented as an accepted limitation. Watcher-detected
moves (real-time CREATE events) are not affected because pruning does not run
in the watcher path.

### Worker hash storage is non-critical
Phase 1 specifies that hash computation/storage failures in `processJob` are
logged but do not fail the job. `TestWorker_NoHashOnFailure` verifies that
failed jobs do not store a hash, but there is no test for the case where
`ComputeSHA256` succeeds but the `UPDATE notes SET sha256=?` call fails.
This is acceptable because (a) the failure path only logs a warning, and
(b) the consequence is that a future move of this file will not be detected
and will be re-processed -- a safe degradation to pre-existing behavior.

### AC1.2 collapsed into AC1.1 test
The design plan lists AC1.1 (move) and AC1.2 (rename) as separate criteria.
The implementation test `TestPipeline_MoveDetection_JobTransferred` covers
both because the code path is identical -- SHA-256 is content-only. The test
uses files in the same directory with different names, which satisfies both
the "move" and "rename" interpretations. No separate test for AC1.2 is needed.

---

## Coverage Summary

| AC | Automated | Human | Status |
|----|-----------|-------|--------|
| content-hash-dedup.AC1.1 | `TestPipeline_MoveDetection_JobTransferred` | -- | Covered |
| content-hash-dedup.AC1.2 | `TestPipeline_MoveDetection_JobTransferred` | Smoke test on running system | Covered (automated + optional manual) |
| content-hash-dedup.AC1.3 | `TestPipeline_MoveDetection_JobTransferred` | -- | Covered |
| content-hash-dedup.AC1.4 | `TestPipeline_MoveDetection_ContentChanged` | -- | Covered |
| content-hash-dedup.AC2.1 | Existing reconciler test (implicit) | `touch` test on running system | Covered (implicit + manual) |
| content-hash-dedup.AC3.1 | `TestWorker_StoresHash_WithOCR` | -- | Covered |
| content-hash-dedup.AC3.2 | `TestWorker_StoresHash_NoOCR` | -- | Covered |
| content-hash-dedup.AC3.3 | `TestWorker_NoHashOnFailure` | -- | Covered |
| content-hash-dedup.AC4.1 | `TestTransferJob` | -- | Covered |
| content-hash-dedup.AC4.2 | `TestTransferJob` | -- | Covered |
