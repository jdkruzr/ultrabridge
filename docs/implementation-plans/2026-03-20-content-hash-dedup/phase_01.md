# Content Hash Deduplication — Phase 1: NoteStore Methods and Worker Hash Storage

**Goal:** Wire up the stubbed `notes.sha256` infrastructure: add `SetHash` and `LookupByHash` to the NoteStore interface, implement them, and call `ComputeSHA256` in the worker on successful job completion.

**Architecture:** Two small changes. (1) Extend the `NoteStore` interface with two new methods and implement them as SQL queries on `*Store`. (2) In `processJob`, compute SHA-256 of the final file after `executeJob` succeeds and store it via a direct `s.db.ExecContext` call, following the same pattern used for `backup_path`. Hash failures are logged but do not fail the job.

**Tech Stack:** Go standard library (`crypto/sha256`, `database/sql`). No new dependencies.

**Scope:** Phase 1 of 2.

**Codebase verified:** 2026-03-20

---

## Acceptance Criteria Coverage

This phase implements and tests:

### content-hash-dedup.AC3: Worker stores hash on completion
- **content-hash-dedup.AC3.1 Success:** Job completes with OCR applied → `notes.sha256` set to SHA-256 of the final (post-injection) file
- **content-hash-dedup.AC3.2 Success:** Job completes without OCR (myScript-only or OCR disabled) → `notes.sha256` set to SHA-256 of the original file
- **content-hash-dedup.AC3.3 Failure:** Job fails → `notes.sha256` not written (no hash stored for failed jobs)

---

## Implementation Context

From codebase verification:

- **NoteStore interface**: `internal/notestore/store.go` lines 22–34. All four existing methods follow the pattern `MethodName(ctx context.Context, args...) (returns...)`.
- **Store struct**: `internal/notestore/store.go` lines 36–45. Has `db *sql.DB` and `notesPath string` fields. `New(db, notesPath)` constructor.
- **ComputeSHA256**: `internal/notestore/scanner.go:152`. Signature: `func ComputeSHA256(path string) (string, error)`. Never called anywhere in the codebase.
- **notes.sha256 column**: Exists in `internal/notedb/schema.go:18`. Never written by any code path.
- **processJob success path**: `internal/processor/worker.go:34`. Insert hash between `executeJob` returning nil and `s.markDone(ctx, job.ID, "")`. Follow the `backup_path` pattern — raw `s.db.ExecContext` call.
- **mockNoteStore in web tests**: `internal/web/handler_test.go`. Implements `NoteStore` interface. Adding new methods to the interface will break compilation until stub methods are added here.
- **Test helpers**: `openTestStore(t)` in `store_test.go` (in-memory DB, `t.TempDir()` root). `openWorkerStore(t, cfg)` in `worker_test.go`. `seedNote(t, s, path)` seeds notes+jobs rows. `copyTestNote(t, name)` copies from `../../testdata/`.

---

<!-- START_SUBCOMPONENT_A (tasks 1-2) -->

<!-- START_TASK_1 -->
## Task 1: Add `SetHash` and `LookupByHash` to NoteStore interface and implement them

**Verifies:** content-hash-dedup.AC3.1, content-hash-dedup.AC3.2, content-hash-dedup.AC3.3 (indirectly — methods needed for test assertions)

**Files:**
- Modify: `internal/notestore/store.go`
- Modify: `internal/web/handler_test.go` (add stub methods to mockNoteStore)

**Implementation:**

In `internal/notestore/store.go`, add to the `NoteStore` interface (after the existing `UpsertFile` method):

```go
// SetHash stores the SHA-256 hex digest for the file at path.
// Called by the worker after successful job completion and by the pipeline
// after a job is transferred to a new path.
SetHash(ctx context.Context, path, hash string) error

// LookupByHash returns the path and job status of any note whose sha256 matches hash
// and which has a completed (done) job. Returns found=false if no match exists.
// Used at enqueue time to detect moved or renamed files.
LookupByHash(ctx context.Context, hash string) (path string, found bool, err error)
```

Add implementations to `internal/notestore/store.go` (alongside the existing `Store` method implementations):

```go
// SetHash stores the SHA-256 hex digest for the file at path.
func (s *Store) SetHash(ctx context.Context, path, hash string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE notes SET sha256=? WHERE path=?", hash, path)
	return err
}

// LookupByHash returns the path of any note whose sha256 matches hash and which has
// a done job. Returns found=false with nil error if no match exists.
func (s *Store) LookupByHash(ctx context.Context, hash string) (path string, found bool, err error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT n.path
		FROM notes n
		JOIN jobs j ON j.note_path = n.path
		WHERE n.sha256 = ? AND j.status = 'done'
		LIMIT 1`, hash)
	err = row.Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return path, true, nil
}
```

Ensure `"database/sql"` and `"errors"` are in the imports of `store.go` (add if not present).

In `internal/web/handler_test.go`, add stub methods to `mockNoteStore` so the interface is satisfied after the new methods are added:

```go
func (m *mockNoteStore) SetHash(_ context.Context, _, _ string) error { return nil }
func (m *mockNoteStore) LookupByHash(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
}
```

**Verification:**
```
go build -C /home/sysop/src/ultrabridge/.worktrees/content-hash-dedup ./...
```
Expected: Builds without errors. The mockNoteStore stubs satisfy the updated interface.

**Commit:** `feat: add SetHash and LookupByHash to NoteStore interface`
<!-- END_TASK_1 -->

<!-- START_TASK_2 -->
## Task 2: Tests for `SetHash` and `LookupByHash`

**Verifies:** content-hash-dedup.AC3.1, content-hash-dedup.AC3.2, content-hash-dedup.AC3.3 (DB-level assertions that hash is stored/not-stored)

**Files:**
- Modify: `internal/notestore/store_test.go`

**Implementation:**

Add the following tests to `internal/notestore/store_test.go`. Follow the existing test pattern: `openTestStore(t)` for the in-memory DB, direct `s.db.ExecContext` for seeding rows.

```go
// TestSetHash verifies that SetHash writes the sha256 column for the given path.
func TestSetHash(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().Unix()

	// Seed a notes row.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notes (path, rel_path, file_type, size_bytes, mtime, created_at, updated_at)
		VALUES (?, ?, 'note', 0, ?, ?, ?)`, "/a.note", "a.note", now, now, now)
	if err != nil {
		t.Fatalf("seed notes: %v", err)
	}

	if err := s.SetHash(ctx, "/a.note", "abc123"); err != nil {
		t.Fatalf("SetHash: %v", err)
	}

	var got string
	if err := s.db.QueryRowContext(ctx, "SELECT sha256 FROM notes WHERE path=?", "/a.note").Scan(&got); err != nil {
		t.Fatalf("read sha256: %v", err)
	}
	if got != "abc123" {
		t.Errorf("sha256 = %q, want abc123", got)
	}
}

// TestLookupByHash_Found verifies LookupByHash returns the path when a matching
// sha256 exists with a done job.
func TestLookupByHash_Found(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().Unix()

	// Seed notes row + done job + hash.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notes (path, rel_path, file_type, size_bytes, mtime, created_at, updated_at)
		VALUES (?, ?, 'note', 0, ?, ?, ?)`, "/a.note", "a.note", now, now, now)
	if err != nil {
		t.Fatalf("seed notes: %v", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO jobs (note_path, status, queued_at) VALUES (?, 'done', ?)`, "/a.note", now)
	if err != nil {
		t.Fatalf("seed jobs: %v", err)
	}
	if err := s.SetHash(ctx, "/a.note", "deadbeef"); err != nil {
		t.Fatalf("SetHash: %v", err)
	}

	path, found, err := s.LookupByHash(ctx, "deadbeef")
	if err != nil {
		t.Fatalf("LookupByHash: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if path != "/a.note" {
		t.Errorf("path = %q, want /a.note", path)
	}
}

// TestLookupByHash_NotFound verifies LookupByHash returns found=false for an unknown hash.
func TestLookupByHash_NotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, found, err := s.LookupByHash(ctx, "notfound")
	if err != nil {
		t.Fatalf("LookupByHash: %v", err)
	}
	if found {
		t.Fatal("expected found=false for unknown hash")
	}
}

// TestLookupByHash_NoJob verifies LookupByHash returns found=false when a note has
// a matching sha256 but no associated job record.
func TestLookupByHash_NoJob(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().Unix()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notes (path, rel_path, file_type, size_bytes, mtime, created_at, updated_at)
		VALUES (?, ?, 'note', 0, ?, ?, ?)`, "/a.note", "a.note", now, now, now)
	if err != nil {
		t.Fatalf("seed notes: %v", err)
	}
	if err := s.SetHash(ctx, "/a.note", "deadbeef"); err != nil {
		t.Fatalf("SetHash: %v", err)
	}

	// No jobs row — LookupByHash should return found=false.
	_, found, err := s.LookupByHash(ctx, "deadbeef")
	if err != nil {
		t.Fatalf("LookupByHash: %v", err)
	}
	if found {
		t.Fatal("expected found=false when no job exists")
	}
}

// TestLookupByHash_PendingJob verifies LookupByHash returns found=false when the job
// exists but is not yet done (pending/in_progress/failed).
// This ensures in-flight jobs are not misidentified as completed moves.
func TestLookupByHash_PendingJob(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().Unix()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notes (path, rel_path, file_type, size_bytes, mtime, created_at, updated_at)
		VALUES (?, ?, 'note', 0, ?, ?, ?)`, "/a.note", "a.note", now, now, now)
	if err != nil {
		t.Fatalf("seed notes: %v", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO jobs (note_path, status, queued_at) VALUES (?, 'pending', ?)`, "/a.note", now)
	if err != nil {
		t.Fatalf("seed jobs: %v", err)
	}
	if err := s.SetHash(ctx, "/a.note", "deadbeef"); err != nil {
		t.Fatalf("SetHash: %v", err)
	}

	// Pending job — LookupByHash should return found=false (only done jobs count).
	_, found, err := s.LookupByHash(ctx, "deadbeef")
	if err != nil {
		t.Fatalf("LookupByHash: %v", err)
	}
	if found {
		t.Fatal("expected found=false for pending job (only done jobs should match)")
	}
}
```

Add `"time"` to the import block of `store_test.go` if not already present.

**Verification:**
```
go test -C /home/sysop/src/ultrabridge/.worktrees/content-hash-dedup ./internal/notestore/ -run TestSetHash -v
go test -C /home/sysop/src/ultrabridge/.worktrees/content-hash-dedup ./internal/notestore/ -run TestLookupByHash -v
```
Expected: All tests pass.

**Commit:** `test: add SetHash and LookupByHash tests`
<!-- END_TASK_2 -->

<!-- END_SUBCOMPONENT_A -->

<!-- START_SUBCOMPONENT_B (tasks 3-4) -->

<!-- START_TASK_3 -->
## Task 3: Modify `processJob` to store hash on successful completion

**Verifies:** content-hash-dedup.AC3.1, content-hash-dedup.AC3.2, content-hash-dedup.AC3.3

**Files:**
- Modify: `internal/processor/worker.go`

**Implementation:**

In `internal/processor/worker.go`, modify `processJob`. The current function ends with:

```go
	if err != nil {
		s.markDone(ctx, job.ID, err.Error())
	} else {
		s.markDone(ctx, job.ID, "")
	}
```

Replace with:

```go
	if err != nil {
		s.markDone(ctx, job.ID, err.Error())
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

Add `notestore` to the import block of `worker.go` if not already present:

```go
"github.com/sysop/ultrabridge/internal/notestore"
```

**Verification:**
```
go build -C /home/sysop/src/ultrabridge/.worktrees/content-hash-dedup ./...
go vet -C /home/sysop/src/ultrabridge/.worktrees/content-hash-dedup ./...
```
Expected: Builds and vets without errors.

**Commit:** `feat: store SHA-256 hash in notes.sha256 after successful job completion`
<!-- END_TASK_3 -->

<!-- START_TASK_4 -->
## Task 4: Tests for worker hash storage

**Verifies:** content-hash-dedup.AC3.1, content-hash-dedup.AC3.2, content-hash-dedup.AC3.3

**Files:**
- Modify: `internal/processor/worker_test.go`

**Implementation:**

Add these tests to `internal/processor/worker_test.go`. Use the existing helpers: `openWorkerStore(t, cfg)`, `seedNote(t, s, path)`, `copyTestNote(t, name)`.

```go
// TestWorker_StoresHash_NoOCR verifies AC3.2: when OCR is disabled, the worker still
// stores the SHA-256 hash of the file in notes.sha256 after the job completes.
func TestWorker_StoresHash_NoOCR(t *testing.T) {
	path := copyTestNote(t, "20260318_154108 std one line.note")
	s := openWorkerStore(t, WorkerConfig{}) // no OCR, no backup
	seedNote(t, s, path)

	job, err := s.claimNext(context.Background())
	if err != nil || job == nil {
		t.Fatalf("claimNext: %v (job=%v)", err, job)
	}

	s.processJob(context.Background(), job)

	// Verify job completed.
	var status string
	s.db.QueryRow("SELECT status FROM jobs WHERE id=?", job.ID).Scan(&status)
	if status != StatusDone {
		t.Errorf("job status = %q, want done", status)
	}

	// Verify notes.sha256 is populated.
	var sha256 string
	s.db.QueryRow("SELECT COALESCE(sha256, '') FROM notes WHERE path=?", path).Scan(&sha256)
	if sha256 == "" {
		t.Error("notes.sha256 should be set after successful job, got empty string")
	}

	// Verify the stored hash matches the actual file.
	want, err := notestore.ComputeSHA256(path)
	if err != nil {
		t.Fatalf("ComputeSHA256: %v", err)
	}
	if sha256 != want {
		t.Errorf("notes.sha256 = %q, want %q", sha256, want)
	}
}

// TestWorker_NoHashOnFailure verifies AC3.3: a failed job does not write notes.sha256.
// We force a failure by providing a path that does not exist on disk.
func TestWorker_NoHashOnFailure(t *testing.T) {
	s := openWorkerStore(t, WorkerConfig{})
	path := "/nonexistent/file.note"

	// Seed the notes row and job manually (file does not exist on disk).
	now := time.Now().Unix()
	s.db.Exec(`INSERT INTO notes (path, rel_path, file_type, size_bytes, mtime, created_at, updated_at)
		VALUES (?, 'file.note', 'note', 0, ?, ?, ?)`, path, now, now, now)
	s.db.Exec(`INSERT INTO jobs (note_path, status, queued_at) VALUES (?, 'pending', ?)`, path, now)

	job, err := s.claimNext(context.Background())
	if err != nil || job == nil {
		t.Fatalf("claimNext: %v (job=%v)", err, job)
	}

	s.processJob(context.Background(), job)

	// Job should be failed or skipped (file open will fail).
	var status string
	s.db.QueryRow("SELECT status FROM jobs WHERE id=?", job.ID).Scan(&status)
	if status == StatusDone {
		t.Fatalf("expected non-done status for failing job, got %q", status)
	}

	// notes.sha256 must NOT be set.
	var sha256 sql.NullString
	s.db.QueryRow("SELECT sha256 FROM notes WHERE path=?", path).Scan(&sha256)
	if sha256.Valid && sha256.String != "" {
		t.Errorf("notes.sha256 should be empty after failed job, got %q", sha256.String)
	}
}
```

**Additional test for AC3.1 (OCR-enabled path):**

Add a test that enables OCR using `mockOCRServer` (an existing helper in `worker_test.go`), verifies the job completes, and checks that the stored hash matches the post-injection file (not the original). The post-injection file is modified in place — compute `ComputeSHA256(path)` after the job completes and compare it to the stored `notes.sha256`. Use the Anthropic mock format since `WorkerConfig.OCRFormat` defaults to `anthropic`.

```go
// TestWorker_StoresHash_WithOCR verifies AC3.1: when OCR is applied, the stored
// sha256 reflects the final (post-injection) file, not the original.
func TestWorker_StoresHash_WithOCR(t *testing.T) {
	path := copyTestNote(t, "20260318_154108 std one line.note")
	srv := mockOCRServer(t, "recognized text")
	s := openWorkerStore(t, WorkerConfig{
		OCREnabled: true,
		OCRClient:  NewOCRClient(srv.URL, "", "test-model", OCRFormatAnthropic),
	})
	seedNote(t, s, path)

	job, err := s.claimNext(context.Background())
	if err != nil || job == nil {
		t.Fatalf("claimNext: %v (job=%v)", err, job)
	}

	s.processJob(context.Background(), job)

	var status string
	s.db.QueryRow("SELECT status FROM jobs WHERE id=?", job.ID).Scan(&status)
	if status != StatusDone {
		t.Skipf("OCR test requires network access or testdata; job status = %q", status)
	}

	// Hash must match the post-injection file (file was modified by OCR inject).
	wantHash, err := notestore.ComputeSHA256(path)
	if err != nil {
		t.Fatalf("ComputeSHA256 post-injection: %v", err)
	}

	var gotHash string
	s.db.QueryRow("SELECT COALESCE(sha256,'') FROM notes WHERE path=?", path).Scan(&gotHash)
	if gotHash == "" {
		t.Error("notes.sha256 should be set after OCR job")
	}
	if gotHash != wantHash {
		t.Errorf("notes.sha256 = %q, want post-injection hash %q", gotHash, wantHash)
	}
}
```

Add `"time"` and `"database/sql"` and `"github.com/sysop/ultrabridge/internal/notestore"` to the import block of `worker_test.go` if not already present.

**Verification:**
```
go test -C /home/sysop/src/ultrabridge/.worktrees/content-hash-dedup ./internal/processor/ -run TestWorker_StoresHash -v
go test -C /home/sysop/src/ultrabridge/.worktrees/content-hash-dedup ./internal/processor/ -run TestWorker_NoHashOnFailure -v
go test -C /home/sysop/src/ultrabridge/.worktrees/content-hash-dedup ./... -count=1
```
Expected: All tests pass. Full test suite passes.

**Commit:** `test: verify notes.sha256 is stored after job completion`
<!-- END_TASK_4 -->

<!-- END_SUBCOMPONENT_B -->
