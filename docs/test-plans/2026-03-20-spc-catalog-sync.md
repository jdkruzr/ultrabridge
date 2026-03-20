# Human Test Plan: SPC Catalog Sync

Generated: 2026-03-20
Implementation plan: `docs/implementation-plans/2026-03-20-spc-catalog-sync/`
Automated coverage: 14/14 acceptance criteria

## Prerequisites

- UltraBridge built and deployed to a Supernote Private Cloud environment with MariaDB access
- `go test -C /path/to/spc-catalog-sync ./internal/processor/ -v` passing (all 11 SPC catalog tests green)
- At least one `.note` file on the device with existing handwriting content
- Access to the MariaDB `supernote` database to query `f_user_file`, `f_file_action`, and `f_capacity` tables
- UltraBridge configured with `UB_OCR_ENABLED=true` and a working OCR endpoint

---

## Phase 1: End-to-End Catalog Sync After OCR Injection

Purpose: Confirm that a real `.note` file processed through the full pipeline (scan, OCR, inject) results in correct SPC catalog updates visible to the Supernote device.

| Step | Action | Expected |
|------|--------|----------|
| 1 | Query MariaDB: `SELECT id, inner_name, size, md5, update_time FROM f_user_file WHERE inner_name = '<filename>'` and record the current values for size, md5, and update_time. | Row exists with pre-injection values. |
| 2 | Query MariaDB: `SELECT used_capacity FROM f_capacity WHERE user_id = <user_id>`. Record the current value. | Returns current used capacity. |
| 3 | Query MariaDB: `SELECT COUNT(*) FROM f_file_action WHERE inner_name = '<filename>'`. Record the count. | Returns current action count. |
| 4 | Trigger processing: either restart UltraBridge so the reconciler picks up the file, or use the web UI processor controls at `/processor` to enqueue the file. Wait for the job to complete (status = "done" in the web UI). | Job completes as "done" in the processor status page. No errors in the log stream. |
| 5 | Re-run the query from Step 1. Compare `size` to the actual file size on disk (`ls -l /path/to/<filename>`). | `f_user_file.size` matches the on-disk file byte count exactly. |
| 6 | Compute the file's MD5: `md5sum /path/to/<filename>`. Compare to the `md5` value from Step 5. | `f_user_file.md5` matches the hex digest from `md5sum`. |
| 7 | Compare `update_time` from Step 5 to the value from Step 1. | `update_time` has increased and is within a few seconds of the job completion time. |
| 8 | Re-run the query from Step 3. | Count has increased by exactly 1. |
| 9 | Query the new row: `SELECT action, file_id, user_id, md5, size, inner_name, create_time, update_time FROM f_file_action WHERE inner_name = '<filename>' ORDER BY create_time DESC LIMIT 1`. | `action = 'A'`, `file_id` matches the `f_user_file.id`, `md5` and `size` match updated values, `create_time = update_time`. |
| 10 | Re-run the query from Step 2. Compute expected: old_capacity + (new_size - old_size). | `used_capacity` matches the expected value. |

---

## Phase 2: Device Visibility Verification

Purpose: Confirm the Supernote device sees the updated file metadata after catalog sync.

| Step | Action | Expected |
|------|--------|----------|
| 1 | On the Supernote device, open the SPC app and navigate to the folder containing the processed `.note` file. | File appears in the list. |
| 2 | Check the file size displayed by the device (if visible in file details). | Size matches the post-injection file size. |
| 3 | Force a device sync (pull from cloud). Open the `.note` file on the device. | File opens without corruption. RECOGNTEXT content (OCR results) is present if the device supports viewing it. |
| 4 | Check the storage usage indicator on the device or SPC web interface. | Used storage reflects the delta from the injection (increased if file grew, decreased if file shrank). |

---

## Phase 3: Best-Effort Failure Resilience

Purpose: Confirm that MariaDB connectivity issues do not cause job failures.

| Step | Action | Expected |
|------|--------|----------|
| 1 | Stop MariaDB temporarily (`systemctl stop mariadb`). | MariaDB is unavailable. |
| 2 | Enqueue a `.note` file for processing via the web UI or by placing it in the watched directory. Wait for the job to complete. | Job completes as "done". The UltraBridge log shows WARN-level messages about SPC catalog update failures but no ERROR or job failure. |
| 3 | Start MariaDB again (`systemctl start mariadb`). | MariaDB is available. |
| 4 | Verify the processed file's OCR content is intact (check SQLite `notes` table for `sha256` populated, check search index for the file's content). | OCR injection and indexing succeeded despite catalog sync failure. |
| 5 | Process the same file again (or a new file) with MariaDB running. | Catalog updates succeed — `f_user_file`, `f_file_action`, and `f_capacity` are all updated correctly. |

---

## End-to-End: Full Pipeline From File Drop to Device Sync

Purpose: Validate the complete flow from file creation through OCR to device visibility, including catalog sync.

1. Create a new handwritten `.note` file on the Supernote device and sync it to SPC.
2. Verify UltraBridge's fsnotify watcher or reconciler detects the new file (check the web UI at `/files`).
3. Verify a job is enqueued (check `/processor` status page).
4. Wait for the job to complete as "done".
5. Query `f_user_file` in MariaDB — size and md5 should reflect the post-injection file.
6. Query `f_file_action` — a new row with `action='A'` should exist.
7. Query `f_capacity` — `used_capacity` should reflect the size delta.
8. On the device, force a sync and reopen the file — it should open without corruption.
9. Search for the handwritten content via UltraBridge's search UI at `/search` — the OCR'd text should appear in results.

---

## Traceability

| Acceptance Criterion | Automated Test | Manual Step |
|----------------------|----------------|-------------|
| AC1.1 size update | `TestAfterInject_UpdatesUserFile` | Phase 1, Step 5 |
| AC1.2 md5 update | `TestAfterInject_UpdatesUserFile` | Phase 1, Step 6 |
| AC1.3 update_time | `TestAfterInject_UpdatesUserFile` | Phase 1, Step 7 |
| AC1.4 missing row | `TestAfterInject_MissingUserFile` | — |
| AC2.1 file_action insert | `TestAfterInject_InsertsFileAction` | Phase 1, Steps 8–9 |
| AC2.2 unique id, timestamps | `TestAfterInject_InsertsFileAction` | Phase 1, Step 9 |
| AC3.1 capacity delta | `TestAfterInject_AdjustsCapacity` | Phase 1, Step 10 |
| AC3.2 zero delta | `TestAfterInject_ZeroDeltaCapacity` | — |
| AC4.1 SELECT failure | `TestAfterInject_SelectFails` | Phase 3, Step 2 |
| AC4.2 UPDATE failure | `TestAfterInject_UpdateFails_ContinuesToInsertAndCapacity` | Phase 3, Step 2 |
| AC4.3 INSERT failure | `TestAfterInject_InsertFails_ContinuesToCapacity` | Phase 3, Step 2 |
| AC5.1 AfterInject on success | `TestWorker_CatalogUpdaterCalledOnSuccess` | Phase 1, Steps 4–10 |
| AC5.2 AfterInject not on failure | `TestWorker_CatalogUpdaterNotCalledOnFailure` | Phase 3, Step 2 |
| AC6.1 nil CatalogUpdater | `TestWorker_NilCatalogUpdater` | — |
| main.go wiring | (compilation) | End-to-End scenario |
| Device visibility | — | Phase 2 |
