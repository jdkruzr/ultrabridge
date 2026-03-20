# Human Test Plan: Content Hash Deduplication

**Implementation plan:** `docs/implementation-plans/2026-03-20-content-hash-dedup/`
**Generated:** 2026-03-20

---

## Prerequisites

- UltraBridge built and running against a configured notes directory (`UB_NOTES_PATH`)
- SQLite database initialized (`UB_DB_PATH`)
- Pipeline active (verify via web UI that the processor shows "running")
- All automated tests passing:
  ```
  go test -C /home/sysop/src/ultrabridge/.worktrees/content-hash-dedup ./internal/notestore/ ./internal/processor/ ./internal/pipeline/
  ```

---

## Phase 1: Hash Storage on Job Completion

| Step | Action | Expected |
|------|--------|----------|
| 1.1 | Place a new `.note` file in the notes directory (e.g., `cp testfile.note $UB_NOTES_PATH/hash-test.note`) | File appears in web UI Files tab within 15 minutes (or after watcher triggers). |
| 1.2 | Wait for processing to complete. Check the web UI Details modal or logs for `status=done`. | Job shows as "done" in the UI. |
| 1.3 | Query the SQLite database: `sqlite3 $UB_DB_PATH "SELECT sha256 FROM notes WHERE path LIKE '%hash-test.note'"` | A 64-character hex SHA-256 hash is present (not NULL or empty). |
| 1.4 | Compute the file hash independently: `sha256sum $UB_NOTES_PATH/hash-test.note` | The stored hash matches the sha256sum output (note: if OCR was applied, the file was modified, so hash reflects the post-injection state). |

---

## Phase 2: Move/Rename Detection

| Step | Action | Expected |
|------|--------|----------|
| 2.1 | With `hash-test.note` fully processed (done), move it: `mv $UB_NOTES_PATH/hash-test.note $UB_NOTES_PATH/renamed-test.note` | The filesystem watcher or next reconciler cycle detects the change. |
| 2.2 | Wait for the next reconciler cycle (up to 15 minutes) or observe watcher logs. | Logs contain: `"detected moved file, transferred job"` with `old=.../hash-test.note new=.../renamed-test.note`. |
| 2.3 | Check the web UI Files tab. | `renamed-test.note` appears with status "done". No new pending job was created. `hash-test.note` is gone from the list. |
| 2.4 | Query the database: `sqlite3 $UB_DB_PATH "SELECT sha256 FROM notes WHERE path LIKE '%renamed-test.note'"` | The sha256 value matches the original hash from step 1.3. |
| 2.5 | Query jobs for the old path: `sqlite3 $UB_DB_PATH "SELECT COUNT(*) FROM jobs WHERE note_path LIKE '%hash-test.note'"` | Returns 0 (old path job was transferred). |

---

## Phase 3: Move with Content Change

| Step | Action | Expected |
|------|--------|----------|
| 3.1 | Place a new `.note` file, wait for it to process to `done`. | Job is done, sha256 is stored. |
| 3.2 | Copy a *different* `.note` file to a new name in the same directory (simulating a move where content changed). | The file appears as a new discovery. |
| 3.3 | Wait for reconciler cycle. | Logs do NOT contain "detected moved file" for this file. Instead, a new `pending` job is created. |
| 3.4 | Check the web UI. | The new file shows as "pending" or "in_progress", not immediately "done". |

---

## Phase 4: Mtime-Only Touch (AC2.1)

| Step | Action | Expected |
|------|--------|----------|
| 4.1 | With a `.note` file fully processed (done), run: `touch $UB_NOTES_PATH/renamed-test.note` | mtime is updated but file content is unchanged. |
| 4.2 | Wait for the next reconciler cycle (up to 15 minutes). | No "enqueue" or "detected moved file" log messages for this file. |
| 4.3 | Check the web UI. | File still shows as "done" with no new job activity. No pending job appears. |
| 4.4 | Query the database: `sqlite3 $UB_DB_PATH "SELECT status FROM jobs WHERE note_path LIKE '%renamed-test.note'"` | Status remains "done". |

---

## End-to-End: Full Lifecycle

**Purpose:** Validate the complete content-hash deduplication lifecycle from initial processing through move detection, confirming no data loss or duplicate processing.

1. Start with a clean notes directory. Place three `.note` files: `alpha.note`, `beta.note`, `gamma.note`.
2. Wait for all three to process to `done`. Verify all three have sha256 values in the database.
3. Move `alpha.note` to a subdirectory: `mkdir $UB_NOTES_PATH/archive && mv $UB_NOTES_PATH/alpha.note $UB_NOTES_PATH/archive/alpha.note`
4. Rename `beta.note`: `mv $UB_NOTES_PATH/beta.note $UB_NOTES_PATH/beta-v2.note`
5. Leave `gamma.note` unchanged.
6. Wait for one reconciler cycle.
7. Verify:
   - `archive/alpha.note` shows "done" (transferred), no pending job.
   - `beta-v2.note` shows "done" (transferred), no pending job.
   - `gamma.note` shows "done" (unchanged, no new activity).
   - No jobs exist for the old paths `alpha.note` or `beta.note`.
   - Log messages confirm "detected moved file, transferred job" for both alpha and beta.

---

## Human Verification Required

| Criterion | Why Manual | Steps |
|-----------|------------|-------|
| AC1.2: Rename detection | SHA-256 is content-only (no path component), so rename is architecturally identical to move. Automated test covers this with different filenames, but manual verification on a live system confirms no path-dependent behavior in production filesystem events. | Phase 2, steps 2.1–2.5 above. |
| AC2.1: Mtime-only touch not re-enqueued | The done-status guard is pre-existing unchanged code. Automated tests cover it implicitly but cannot simulate production filesystem operations (sync tools, backup software) that update mtime without content changes. | Phase 4, steps 4.1–4.4 above. |

---

## Traceability

| Acceptance Criterion | Automated Test | Manual Step |
|----------------------|----------------|-------------|
| AC1.1 Move detection transfers job | `TestPipeline_MoveDetection_JobTransferred` | Phase 2: steps 2.1–2.5 |
| AC1.2 Rename detection | `TestPipeline_MoveDetection_JobTransferred` | Phase 2: steps 2.1–2.5 (rename variant) |
| AC1.3 New path sha256 set | `TestPipeline_MoveDetection_JobTransferred` | Phase 2: step 2.4 |
| AC1.4 Content changed, normal enqueue | `TestPipeline_MoveDetection_ContentChanged` | Phase 3: steps 3.1–3.4 |
| AC2.1 Mtime touch, done job skipped | `TestReconciler_NewAndUnchanged` (implicit) | Phase 4: steps 4.1–4.4 |
| AC3.1 Hash stored with OCR | `TestWorker_StoresHash_WithOCR` | Phase 1: steps 1.3–1.4 |
| AC3.2 Hash stored without OCR | `TestWorker_StoresHash_NoOCR` | Phase 1: steps 1.3–1.4 |
| AC3.3 No hash on failure | `TestWorker_NoHashOnFailure` | — (failure path, not practical to test manually) |
| AC4.1 Transfer retains fields | `TestTransferJob` | Phase 2: step 2.3 (done status preserved) |
| AC4.2 Old path job removed | `TestTransferJob` | Phase 2: step 2.5 |
