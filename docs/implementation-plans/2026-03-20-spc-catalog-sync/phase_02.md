# SPC Catalog Sync Implementation Plan — Phase 2

**Goal:** Wire `CatalogUpdater.AfterInject` into the worker's success path and connect the concrete implementation in main.go.

**Architecture:** `processJob` in `worker.go` calls `s.cfg.CatalogUpdater.AfterInject(ctx, job.NotePath)` after storing the SHA-256 hash and before `s.markDone`, using the same nil-guard pattern as `s.cfg.Indexer`. `main.go` assigns `processor.NewSPCCatalog(database)` to `workerCfg.CatalogUpdater` alongside the existing `Indexer: si` assignment — no new config flag.

**Tech Stack:** Go stdlib only; all dependencies from Phase 1

**Scope:** 2 of 2 phases from original design (depends on Phase 1)

**Codebase verified:** 2026-03-20

---

## Acceptance Criteria Coverage

### spc-catalog-sync.AC5: Worker calls AfterInject on correct path
- **spc-catalog-sync.AC5.1 Success:** `AfterInject` is called on the success path after `executeJob` returns nil
- **spc-catalog-sync.AC5.2 Failure:** `AfterInject` is not called when `executeJob` returns an error

### spc-catalog-sync.AC6: Nil CatalogUpdater is safe
- **spc-catalog-sync.AC6.1 Edge:** When `CatalogUpdater` is nil in `WorkerConfig`, `processJob` does not panic and behaves identically to before this change

---

<!-- START_SUBCOMPONENT_A (tasks 1-3) -->

<!-- START_TASK_1 -->
### Task 1: Call AfterInject in processJob success path

**Verifies:** spc-catalog-sync.AC5.1, spc-catalog-sync.AC5.2, spc-catalog-sync.AC6.1

**Files:**
- Modify: `internal/processor/worker.go:34-46`

**Implementation:**

In `processJob` (lines 23-47 of `worker.go`), the success branch is the `else` block starting at line 34. The SHA-256 store block runs lines 37-44, followed by `s.markDone(ctx, job.ID, "")` at line 45.

Insert the `CatalogUpdater` call between the end of the sha256 block and `s.markDone`. The old `else` block:

```go
} else {
    // Store SHA-256 of the final file state for move/rename detection (AC3.1, AC3.2).
    // Hash failure is non-critical — log and continue.
    if hash, hashErr := notestore.ComputeSHA256(job.NotePath); hashErr == nil {
        if _, dbErr := s.db.ExecContext(ctx,
            "UPDATE notes SET sha256=? WHERE path=?", hash, job.NotePath); dbErr != nil {
            s.logger.Warn("failed to store content hash", "path", job.NotePath, "err", dbErr)
        }
    } else {
        s.logger.Warn("failed to compute content hash", "path", job.NotePath, "err", hashErr)
    }
    s.markDone(ctx, job.ID, "")
}
```

Replace with:

```go
} else {
    // Store SHA-256 of the final file state for move/rename detection (AC3.1, AC3.2).
    // Hash failure is non-critical — log and continue.
    if hash, hashErr := notestore.ComputeSHA256(job.NotePath); hashErr == nil {
        if _, dbErr := s.db.ExecContext(ctx,
            "UPDATE notes SET sha256=? WHERE path=?", hash, job.NotePath); dbErr != nil {
            s.logger.Warn("failed to store content hash", "path", job.NotePath, "err", dbErr)
        }
    } else {
        s.logger.Warn("failed to compute content hash", "path", job.NotePath, "err", hashErr)
    }
    if s.cfg.CatalogUpdater != nil {
        if err := s.cfg.CatalogUpdater.AfterInject(ctx, job.NotePath); err != nil {
            s.logger.Warn("spc catalog update failed", "path", job.NotePath, "err", err)
        }
    }
    s.markDone(ctx, job.ID, "")
}
```

**Verification:**

```bash
go build -C /home/sysop/src/ultrabridge/.worktrees/spc-catalog-sync ./...
go vet -C /home/sysop/src/ultrabridge/.worktrees/spc-catalog-sync ./...
go test -C /home/sysop/src/ultrabridge/.worktrees/spc-catalog-sync ./internal/processor/
```

Expected: all succeed. Existing worker tests must still pass.

**Commit:** `feat: call CatalogUpdater.AfterInject on worker success path`
<!-- END_TASK_1 -->

<!-- START_TASK_2 -->
### Task 2: Wire NewSPCCatalog in main.go

**Verifies:** None (wiring only; verified by integration at runtime)

**Files:**
- Modify: `cmd/ultrabridge/main.go:94-103`

**Implementation:**

The `workerCfg` struct literal is currently at lines 94-99 of `main.go`:

```go
workerCfg := processor.WorkerConfig{
    OCREnabled: cfg.OCREnabled,
    BackupPath: cfg.BackupPath,
    MaxFileMB:  cfg.OCRMaxFileMB,
    Indexer:    si,
}
```

Keep the struct literal as-is, then add `CatalogUpdater` in a nil-guarded assignment after the struct:

```go
workerCfg := processor.WorkerConfig{
    OCREnabled: cfg.OCREnabled,
    BackupPath: cfg.BackupPath,
    MaxFileMB:  cfg.OCRMaxFileMB,
    Indexer:    si,
}
if database != nil {
    workerCfg.CatalogUpdater = processor.NewSPCCatalog(database)
}
```

The nil guard ensures graceful degradation: if `database` is nil (today this can't happen because `db.Connect` failure causes `os.Exit(1)`, but the guard protects against future refactors that make MariaDB optional). `processor` is already imported (line 22 of main.go).

**Verification:**

```bash
go build -C /home/sysop/src/ultrabridge/.worktrees/spc-catalog-sync ./cmd/ultrabridge/
go vet -C /home/sysop/src/ultrabridge/.worktrees/spc-catalog-sync ./cmd/ultrabridge/
```

Expected: both succeed.

**Commit:** `feat: wire NewSPCCatalog in main.go`
<!-- END_TASK_2 -->

<!-- START_TASK_3 -->
### Task 3: Add worker tests for AC5 and AC6

**Verifies:** spc-catalog-sync.AC5.1, spc-catalog-sync.AC5.2, spc-catalog-sync.AC6.1

**Files:**
- Modify: `internal/processor/worker_test.go` (append to end of file)

**Implementation:**

Read `ed3d-house-style:writing-good-tests` before writing. The existing `mockIndexer` pattern in `worker_test.go` (lines 19-33) is the direct template for `mockCatalogUpdater`.

Add at the end of `worker_test.go`:

**Mock type** (add after the `mockIndexer` type, or at end of file — either is fine):

```go
// mockCatalogUpdater records AfterInject calls for assertion.
type mockCatalogUpdater struct {
	called bool
	path   string
}

func (m *mockCatalogUpdater) AfterInject(_ context.Context, path string) error {
	m.called = true
	m.path = path
	return nil
}
```

**Test: AC5.1 — AfterInject called on success path**

```go
// AC5.1: AfterInject is called on the success path after executeJob returns nil.
func TestWorker_CatalogUpdaterCalledOnSuccess(t *testing.T) {
	notePath := copyTestNote(t, "20260318_154108 std one line.note")
	cat := &mockCatalogUpdater{}
	s := openWorkerStore(t, WorkerConfig{CatalogUpdater: cat})
	seedNote(t, s, notePath)

	s.processJob(context.Background(), &Job{ID: 1, NotePath: notePath})

	var status string
	s.db.QueryRow("SELECT status FROM jobs WHERE id=1").Scan(&status)
	if status != StatusDone {
		t.Errorf("status = %q, want done", status)
	}
	if !cat.called {
		t.Error("AfterInject was not called on success path")
	}
	if cat.path != notePath {
		t.Errorf("AfterInject called with path %q, want %q", cat.path, notePath)
	}
}
```

**Test: AC5.2 — AfterInject NOT called when executeJob returns an error**

Force a failure by providing a path that does not exist on disk. Use `seedNote` for consistency with existing tests (the file does not need to exist on disk until `processJob` runs the stat, at which point the open will fail).

```go
// AC5.2: AfterInject is not called when executeJob returns an error.
func TestWorker_CatalogUpdaterNotCalledOnFailure(t *testing.T) {
	cat := &mockCatalogUpdater{}
	s := openWorkerStore(t, WorkerConfig{CatalogUpdater: cat})
	path := "/nonexistent/missing.note"
	seedNote(t, s, path)

	job, err := s.claimNext(context.Background())
	if err != nil || job == nil {
		t.Fatalf("claimNext: %v (job=%v)", err, job)
	}

	s.processJob(context.Background(), job)

	var status string
	s.db.QueryRow("SELECT status FROM jobs WHERE id=?", job.ID).Scan(&status)
	if status == StatusDone {
		t.Fatalf("expected non-done status for failing job, got done")
	}
	if cat.called {
		t.Error("AfterInject must not be called when executeJob returns an error")
	}
}
```

**Test: AC6.1 — nil CatalogUpdater does not panic**

```go
// AC6.1: A nil CatalogUpdater in WorkerConfig causes no panic.
func TestWorker_NilCatalogUpdater(t *testing.T) {
	notePath := copyTestNote(t, "20260318_154108 std one line.note")
	s := openWorkerStore(t, WorkerConfig{}) // CatalogUpdater is nil
	seedNote(t, s, notePath)

	// Must not panic.
	s.processJob(context.Background(), &Job{ID: 1, NotePath: notePath})

	var status string
	s.db.QueryRow("SELECT status FROM jobs WHERE id=1").Scan(&status)
	if status != StatusDone {
		t.Errorf("status = %q, want done", status)
	}
}
```

**Verification:**

```bash
go test -C /home/sysop/src/ultrabridge/.worktrees/spc-catalog-sync ./internal/processor/ -run "TestWorker_CatalogUpdater|TestWorker_NilCatalogUpdater" -v
```

Expected: All three new tests pass.

Then run the full test suite:

```bash
go test -C /home/sysop/src/ultrabridge/.worktrees/spc-catalog-sync ./...
```

Expected: All tests pass across all packages.

**Commit:** `test: add worker tests for CatalogUpdater AC5 and AC6`
<!-- END_TASK_3 -->

<!-- END_SUBCOMPONENT_A -->
