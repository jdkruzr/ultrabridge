# Content Hash Deduplication — Phase 2: Hash-Based Skip and Job Transfer in pipeline.enqueue

**Goal:** At enqueue time, detect moved/renamed files by SHA-256 hash and transfer the existing completed job record to the new path rather than re-processing.

**Architecture:** Two changes. (1) Add `TransferJob` to the NoteStore interface and implement it as a single `UPDATE jobs SET note_path=? WHERE note_path=?`. (2) Extend `pipeline.enqueue()` — between the existing done-status guard and `proc.Enqueue()` — to compute SHA-256 of the file, call `LookupByHash` (from Phase 1), and if a match is found at a different path with a done job, call `TransferJob` + `SetHash` and return without enqueuing. This is best-effort: any failure falls through to normal enqueue.

**Tech Stack:** Go standard library. No new dependencies.

**Scope:** Phase 2 of 2. Requires Phase 1 (`SetHash`, `LookupByHash` on NoteStore).

**Codebase verified:** 2026-03-20

---

## Acceptance Criteria Coverage

This phase implements and tests:

### content-hash-dedup.AC1: Move/rename detection skips re-processing
- **content-hash-dedup.AC1.1 Success:** File moved to a new path with unchanged content → job record transferred to new path, file not re-enqueued
- **content-hash-dedup.AC1.2 Success:** File renamed (same directory, different name) → same behavior as move
- **content-hash-dedup.AC1.3 Success:** New path's `notes.sha256` is set to the file's hash after transfer
- **content-hash-dedup.AC1.4 Failure:** File moved AND content changed → hash mismatch, file enqueued for processing normally

### content-hash-dedup.AC2: Mtime-only changes don't trigger re-processing
- **content-hash-dedup.AC2.1 Success:** File `touch`ed (mtime changed, content unchanged) with existing done job → not re-enqueued (done-status guard fires before hash check; no regression)

### content-hash-dedup.AC4: Job transfer integrity
- **content-hash-dedup.AC4.1 Success:** Transferred job retains `ocr_source`, `api_model`, `status=done` from original
- **content-hash-dedup.AC4.2 Success:** Old path's job record is gone after transfer (UPDATE moved it, not copied)

---

## Implementation Context

From codebase verification:

- **`pipeline.enqueue()`**: `internal/pipeline/pipeline.go:91–110`. Guards in order: file type check → `UpsertFile` → done-status guard → `proc.Enqueue`. Hash check inserts after the done-status guard.
- **`Pipeline.store`**: Type `notestore.NoteStore`. Hash check calls `p.store.LookupByHash`, `p.store.TransferJob`, `p.store.SetHash`.
- **Known limitation — pruneOrphans race**: `Scan()` calls `pruneOrphans` before returning changed paths. If both the old path (now gone) and new path appear in the same reconcile pass, the old path's job is deleted by pruneOrphans before `enqueue()` can call `LookupByHash`. In this case, move detection silently falls through to normal re-enqueue. **Move detection is guaranteed for watcher-detected moves** (real-time CREATE events, where pruning hasn't run). Reconciler-detected moves (file moved while device offline) may re-process if both paths surface in the same 15-minute scan.
- **`notestore` import**: Already present in `pipeline.go:7`. No new import needed.
- **`jobs.note_path` UNIQUE constraint**: `internal/notedb/schema.go:26`. `UPDATE jobs SET note_path=new WHERE note_path=old` works as a single atomic statement; FK constraint satisfied because `UpsertFile` already created the notes row for the new path.
- **`mockNoteStore` in web tests**: `internal/web/handler_test.go`. Must receive a stub for `TransferJob` or compilation fails.
- **Integration test DB access**: `openTestComponents` doesn't return the `*sql.DB`. Add `openTestComponentsWithDB` helper (returns the shared `*sql.DB`) so integration tests can set job status via raw SQL without accessing unexported fields of other packages.
- **`debounceDelay` constant**: `internal/pipeline/watcher.go:14` — `2 * time.Second`. Not needed for reconcile-based tests (synchronous).

---

<!-- START_SUBCOMPONENT_A (tasks 1-2) -->

<!-- START_TASK_1 -->
## Task 1: Add `TransferJob` to NoteStore interface and implement it; add mock stub in web tests

**Verifies:** content-hash-dedup.AC4.1, content-hash-dedup.AC4.2

**Files:**
- Modify: `internal/notestore/store.go`
- Modify: `internal/web/handler_test.go`

**Implementation:**

In `internal/notestore/store.go`, add to the `NoteStore` interface (after `LookupByHash` from Phase 1):

```go
// TransferJob moves the job record for oldPath to newPath by updating the FK.
// Used when move detection identifies that newPath contains the same content
// as an already-processed oldPath. newPath must already exist in the notes table.
// Returns an error if no job exists for oldPath or the FK constraint is violated.
TransferJob(ctx context.Context, oldPath, newPath string) error
```

Add the implementation to `internal/notestore/store.go`:

```go
// TransferJob moves the job record for oldPath to newPath.
// The notes row for newPath must already exist (caller's responsibility).
func (s *Store) TransferJob(ctx context.Context, oldPath, newPath string) error {
	result, err := s.db.ExecContext(ctx,
		"UPDATE jobs SET note_path=? WHERE note_path=?", newPath, oldPath)
	if err != nil {
		return fmt.Errorf("transfer job %s → %s: %w", oldPath, newPath, err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("no job found for path %s", oldPath)
	}
	return nil
}
```

Ensure `"fmt"` is in the imports of `store.go` (add if not present).

In `internal/web/handler_test.go`, add the stub method to `mockNoteStore`:

```go
func (m *mockNoteStore) TransferJob(_ context.Context, _, _ string) error { return nil }
```

**Verification:**
```
go build -C /home/sysop/src/ultrabridge/.worktrees/content-hash-dedup ./...
```
Expected: Builds without errors.

**Commit:** `feat: add TransferJob to NoteStore interface`
<!-- END_TASK_1 -->

<!-- START_TASK_2 -->
## Task 2: Tests for `TransferJob`

**Verifies:** content-hash-dedup.AC4.1, content-hash-dedup.AC4.2

**Files:**
- Modify: `internal/notestore/store_test.go`

**Implementation:**

Add the following tests to `internal/notestore/store_test.go`. Use `openTestStore(t)` and direct SQL for seeding.

```go
// TestTransferJob verifies that TransferJob moves the job record from oldPath to newPath,
// verifying AC4.1 (job retains fields) and AC4.2 (old path job is gone).
func TestTransferJob(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().Unix()

	// Seed two notes rows.
	for _, path := range []string{"/old.note", "/new.note"} {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO notes (path, rel_path, file_type, size_bytes, mtime, created_at, updated_at)
			VALUES (?, ?, 'note', 0, ?, ?, ?)`, path, filepath.Base(path), now, now, now)
		if err != nil {
			t.Fatalf("seed notes %s: %v", path, err)
		}
	}

	// Seed a done job for /old.note with ocr_source and api_model set.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO jobs (note_path, status, ocr_source, api_model, queued_at, finished_at)
		VALUES (?, 'done', 'api', 'test-model', ?, ?)`, "/old.note", now, now)
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}

	// Transfer job to /new.note.
	if err := s.TransferJob(ctx, "/old.note", "/new.note"); err != nil {
		t.Fatalf("TransferJob: %v", err)
	}

	// AC4.2: old path has no job.
	var count int
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE note_path=?", "/old.note").Scan(&count)
	if count != 0 {
		t.Errorf("old path still has %d job(s), want 0", count)
	}

	// AC4.1: new path has the job with original fields intact.
	var status, ocrSource, apiModel string
	s.db.QueryRowContext(ctx,
		"SELECT status, COALESCE(ocr_source,''), COALESCE(api_model,'') FROM jobs WHERE note_path=?",
		"/new.note").Scan(&status, &ocrSource, &apiModel)
	if status != "done" {
		t.Errorf("status = %q, want done", status)
	}
	if ocrSource != "api" {
		t.Errorf("ocr_source = %q, want api", ocrSource)
	}
	if apiModel != "test-model" {
		t.Errorf("api_model = %q, want test-model", apiModel)
	}
}

// TestTransferJob_NoJob verifies TransferJob returns an error when no job exists
// for the given old path.
func TestTransferJob_NoJob(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	err := s.TransferJob(ctx, "/nonexistent.note", "/new.note")
	if err == nil {
		t.Error("expected error when no job exists for old path")
	}
}
```

Add `"path/filepath"` to the import block of `store_test.go` if not already present.

**Verification:**
```
go test -C /home/sysop/src/ultrabridge/.worktrees/content-hash-dedup ./internal/notestore/ -run TestTransferJob -v
```
Expected: Both tests pass.

**Commit:** `test: add TransferJob tests`
<!-- END_TASK_2 -->

<!-- END_SUBCOMPONENT_A -->

<!-- START_SUBCOMPONENT_B (tasks 3-4) -->

<!-- START_TASK_3 -->
## Task 3: Modify `pipeline.enqueue()` to add hash-based move detection

**Verifies:** content-hash-dedup.AC1.1, content-hash-dedup.AC1.2, content-hash-dedup.AC1.3, content-hash-dedup.AC1.4, content-hash-dedup.AC2.1

**Files:**
- Modify: `internal/pipeline/pipeline.go`

**Implementation:**

In `internal/pipeline/pipeline.go`, replace the section from the done-status guard through `proc.Enqueue` with:

```go
	// Skip files already successfully processed. Automatic detection should not
	// re-queue completed files — the mtime change was caused by our own write.
	job, err := p.proc.GetJob(ctx, path)
	if err == nil && job != nil && job.Status == processor.StatusDone {
		return
	}

	// Hash-based move/rename detection (best-effort: any failure falls through to normal enqueue).
	// Compute SHA-256 of the file and check if another path was already processed with
	// identical content. If so, transfer the job record rather than re-processing.
	if hash, hashErr := notestore.ComputeSHA256(path); hashErr == nil {
		if oldPath, found, _ := p.store.LookupByHash(ctx, hash); found && oldPath != path {
			if transferErr := p.store.TransferJob(ctx, oldPath, path); transferErr == nil {
				if setErr := p.store.SetHash(ctx, path, hash); setErr != nil {
					p.logger.Warn("failed to set hash after job transfer", "path", path, "err", setErr)
				}
				p.logger.Info("detected moved file, transferred job", "old", oldPath, "new", path)
				return
			}
			p.logger.Warn("job transfer failed, will re-process", "old", oldPath, "new", path)
		}
	}

	if err := p.proc.Enqueue(ctx, path); err != nil {
		p.logger.Warn("pipeline enqueue failed", "path", path, "err", err)
	}
```

No import changes needed — `notestore` is already imported in `pipeline.go`.

**Verification:**
```
go build -C /home/sysop/src/ultrabridge/.worktrees/content-hash-dedup ./...
go vet -C /home/sysop/src/ultrabridge/.worktrees/content-hash-dedup ./...
```
Expected: Clean build and vet.

**Commit:** `feat: hash-based move/rename detection in pipeline.enqueue`
<!-- END_TASK_3 -->

<!-- START_TASK_4 -->
## Task 4: Integration test for move detection in pipeline

**Verifies:** content-hash-dedup.AC1.1, content-hash-dedup.AC1.3, content-hash-dedup.AC1.4, content-hash-dedup.AC2.1

**Files:**
- Modify: `internal/pipeline/pipeline_test.go`

**Implementation:**

Add `openTestComponentsWithDB` helper and move detection tests to `internal/pipeline/pipeline_test.go`:

```go
// openTestComponentsWithDB returns the shared *sql.DB in addition to Store and Processor,
// so tests can manipulate job/notes state via raw SQL without accessing unexported fields.
func openTestComponentsWithDB(t *testing.T) (*notestore.Store, *processor.Store, *sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return notestore.New(db, dir), processor.New(db, processor.WorkerConfig{}), db, dir
}

// TestPipeline_MoveDetection_JobTransferred verifies AC1.1 and AC1.3:
// when a file with the same content appears at a new path, the reconciler
// transfers the done job to the new path without re-enqueueing.
func TestPipeline_MoveDetection_JobTransferred(t *testing.T) {
	ns, proc, db, dir := openTestComponentsWithDB(t)
	ctx := context.Background()

	// Create a file at pathA.
	pathA := filepath.Join(dir, "original.note")
	content := []byte("supernote note content for hash test")
	if err := os.WriteFile(pathA, content, 0644); err != nil {
		t.Fatalf("write pathA: %v", err)
	}

	pl := New(Config{NotesPath: dir, Store: ns, Proc: proc, Logger: slog.Default()})

	// First reconcile: discovers pathA, creates pending job.
	pl.reconcile(ctx)

	// Simulate completed processing: set job to done and store hash.
	hashA, err := notestore.ComputeSHA256(pathA)
	if err != nil {
		t.Fatalf("ComputeSHA256: %v", err)
	}
	if err := ns.SetHash(ctx, pathA, hashA); err != nil {
		t.Fatalf("SetHash: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE jobs SET status='done', finished_at=? WHERE note_path=?",
		time.Now().Unix(), pathA); err != nil {
		t.Fatalf("mark done: %v", err)
	}

	// Create pathB with identical content (simulates a move/rename).
	pathB := filepath.Join(dir, "moved.note")
	if err := os.WriteFile(pathB, content, 0644); err != nil {
		t.Fatalf("write pathB: %v", err)
	}

	// Second reconcile: discovers pathB (new mtime), should detect hash match and transfer.
	pl.reconcile(ctx)

	// AC1.1: pathB should have a done job (transferred from pathA).
	jobB, err := proc.GetJob(ctx, pathB)
	if err != nil {
		t.Fatalf("GetJob(pathB): %v", err)
	}
	if jobB == nil {
		t.Fatal("expected job for pathB after transfer, got nil")
	}
	if jobB.Status != processor.StatusDone {
		t.Errorf("pathB job status = %q, want done", jobB.Status)
	}

	// AC1.1: pathA should have no job (transferred away).
	jobA, _ := proc.GetJob(ctx, pathA)
	if jobA != nil {
		t.Errorf("expected no job for pathA after transfer, got status=%q", jobA.Status)
	}

	// AC1.3: pathB's notes.sha256 should be set.
	var sha256B string
	db.QueryRowContext(ctx, "SELECT COALESCE(sha256,'') FROM notes WHERE path=?", pathB).Scan(&sha256B)
	if sha256B == "" {
		t.Error("notes.sha256 for pathB should be set after transfer")
	}
	if sha256B != hashA {
		t.Errorf("notes.sha256 = %q, want %q", sha256B, hashA)
	}
}

// TestPipeline_MoveDetection_ContentChanged verifies AC1.4:
// when the moved file has different content, it gets enqueued normally.
func TestPipeline_MoveDetection_ContentChanged(t *testing.T) {
	ns, proc, db, dir := openTestComponentsWithDB(t)
	ctx := context.Background()

	// Create pathA, simulate done job with hash.
	pathA := filepath.Join(dir, "original.note")
	if err := os.WriteFile(pathA, []byte("original content"), 0644); err != nil {
		t.Fatalf("write pathA: %v", err)
	}
	pl := New(Config{NotesPath: dir, Store: ns, Proc: proc, Logger: slog.Default()})
	pl.reconcile(ctx)

	hashA, _ := notestore.ComputeSHA256(pathA)
	ns.SetHash(ctx, pathA, hashA)
	db.ExecContext(ctx, "UPDATE jobs SET status='done' WHERE note_path=?", pathA)

	// Create pathB with DIFFERENT content.
	pathB := filepath.Join(dir, "different.note")
	if err := os.WriteFile(pathB, []byte("completely different content"), 0644); err != nil {
		t.Fatalf("write pathB: %v", err)
	}

	pl.reconcile(ctx)

	// AC1.4: pathB should be enqueued (not transferred) because content differs.
	jobB, err := proc.GetJob(ctx, pathB)
	if err != nil {
		t.Fatalf("GetJob(pathB): %v", err)
	}
	if jobB == nil {
		t.Fatal("expected pathB to be enqueued, got nil job")
	}
	if jobB.Status != processor.StatusPending {
		t.Errorf("pathB job status = %q, want pending", jobB.Status)
	}
}
```

Add required imports to `pipeline_test.go` if not already present:
```go
import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/notestore"
	"github.com/sysop/ultrabridge/internal/processor"
)
```

`"log/slog"` is required for `slog.Default()` in the `New(Config{...Logger: slog.Default()})` calls. Add it explicitly if it is not already in the file's import block.

**Note on AC2.1:** The done-status guard test (`TestReconciler_NewAndUnchanged` or similar) already covers the mtime-only change regression. If it doesn't, verify that an existing test calls `reconcile()` twice on the same unchanged file and asserts no duplicate enqueue. The done-status guard fires before hash computation, so this is covered implicitly by the existing reconciler tests.

**Verification:**
```
go test -C /home/sysop/src/ultrabridge/.worktrees/content-hash-dedup ./internal/pipeline/ -run TestPipeline_MoveDetection -v
go test -C /home/sysop/src/ultrabridge/.worktrees/content-hash-dedup ./... -count=1
go vet -C /home/sysop/src/ultrabridge/.worktrees/content-hash-dedup ./...
```
Expected: All tests pass. Full test suite passes.

**Commit:** `test: add pipeline integration tests for move/rename detection`
<!-- END_TASK_4 -->

<!-- END_SUBCOMPONENT_B -->
