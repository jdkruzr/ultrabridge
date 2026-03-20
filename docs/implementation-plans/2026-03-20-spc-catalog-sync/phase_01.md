# SPC Catalog Sync Implementation Plan — Phase 1

**Goal:** Implement the `CatalogUpdater` interface, `spcCatalog` concrete type, and full test coverage against in-memory SQLite.

**Architecture:** `CatalogUpdater` is a single-method interface added to `internal/processor/processor.go` alongside the existing `Indexer`. `spcCatalog` in `internal/processor/catalog.go` holds a MariaDB `*sql.DB` and executes five SQL steps in `AfterInject` (stat+MD5, SELECT, UPDATE, INSERT, UPDATE). Tests use SQLite (already in go.mod via `modernc.org/sqlite`) with the three SPC subset tables injected after `notedb.Open`.

**Tech Stack:** Go stdlib (`crypto/md5`, `os`, `io`, `path/filepath`, `database/sql`, `time`, `encoding/hex`, `log/slog`), `modernc.org/sqlite` for tests (via `notedb.Open`)

**Scope:** 1 of 2 phases from original design

**Codebase verified:** 2026-03-20

---

## Acceptance Criteria Coverage

### spc-catalog-sync.AC1: f_user_file catalog row updated
- **spc-catalog-sync.AC1.1 Success:** After injection, `f_user_file.size` equals the byte count of the modified file on disk
- **spc-catalog-sync.AC1.2 Success:** After injection, `f_user_file.md5` equals the MD5 hex digest of the modified file
- **spc-catalog-sync.AC1.3 Success:** After injection, `f_user_file.update_time` is updated to the current timestamp
- **spc-catalog-sync.AC1.4 Failure:** If no `f_user_file` row exists for the file's `inner_name`, no update is attempted and the job still completes as done

### spc-catalog-sync.AC2: f_file_action audit row inserted
- **spc-catalog-sync.AC2.1 Success:** A new `f_file_action` row with `action='A'` is inserted, containing the correct `file_id`, `user_id`, `md5`, `size`, `inner_name`, and `file_name`
- **spc-catalog-sync.AC2.2 Success:** The inserted row has a unique `id` and matching `create_time`/`update_time`

### spc-catalog-sync.AC3: f_capacity quota adjusted
- **spc-catalog-sync.AC3.1 Success:** `f_capacity.used_capacity` is updated by `new_size − old_size` (may be positive or negative)
- **spc-catalog-sync.AC3.2 Edge:** If `new_size == old_size`, capacity delta is zero; update proceeds without error

### spc-catalog-sync.AC4: Best-effort — failures do not fail the job
- **spc-catalog-sync.AC4.1 Failure:** If `f_user_file` SELECT fails, remaining steps are skipped; job still completes as done
- **spc-catalog-sync.AC4.2 Failure:** If `f_user_file` UPDATE fails, `f_file_action` INSERT and `f_capacity` UPDATE still execute
- **spc-catalog-sync.AC4.3 Failure:** If `f_file_action` INSERT fails, `f_capacity` UPDATE still executes

---

<!-- START_SUBCOMPONENT_A (tasks 1-3) -->

<!-- START_TASK_1 -->
### Task 1: Add CatalogUpdater interface and WorkerConfig field

**Verifies:** None (interface declaration only)

**Files:**
- Modify: `internal/processor/processor.go:19-28`

**Implementation:**

After the `Indexer` interface block (line 19, after the closing `}`), insert the `CatalogUpdater` interface. Then add the `CatalogUpdater` field to `WorkerConfig`.

The edit to `processor.go` adds two things:

1. A new interface block after `Indexer` (insert after line 19):

```go
// CatalogUpdater is the interface the worker uses to update the SPC MariaDB
// catalog after a successful OCR injection. A nil CatalogUpdater disables
// catalog sync. Defined here (not in a separate package) to avoid circular imports.
type CatalogUpdater interface {
	// AfterInject updates the SPC MariaDB catalog to reflect a file that
	// was modified server-side by OCR injection. All DB operations are
	// best-effort: errors are logged but do not propagate.
	AfterInject(ctx context.Context, path string) error
}
```

2. A new field in `WorkerConfig` (after `Indexer Indexer // nil = indexing disabled`):

```go
CatalogUpdater CatalogUpdater // nil = SPC catalog sync disabled
```

**Verification:**

```bash
go build -C /home/sysop/src/ultrabridge/.worktrees/spc-catalog-sync ./...
go vet -C /home/sysop/src/ultrabridge/.worktrees/spc-catalog-sync ./...
```

Expected: both succeed without errors.

**Commit:** `feat: add CatalogUpdater interface and WorkerConfig field`
<!-- END_TASK_1 -->

<!-- START_TASK_2 -->
### Task 2: Create spcCatalog implementation in catalog.go

**Verifies:** spc-catalog-sync.AC1.1, spc-catalog-sync.AC1.2, spc-catalog-sync.AC1.3, spc-catalog-sync.AC1.4, spc-catalog-sync.AC2.1, spc-catalog-sync.AC2.2, spc-catalog-sync.AC3.1, spc-catalog-sync.AC3.2, spc-catalog-sync.AC4.1, spc-catalog-sync.AC4.2, spc-catalog-sync.AC4.3

**Files:**
- Create: `internal/processor/catalog.go`

**Implementation:**

Create `internal/processor/catalog.go` with the following complete content. Read the `ed3d-house-style:coding-effectively` skill before writing.

Key design decisions embedded in this implementation:
- `inner_name` = `filepath.Base(path)` — SPC stores files by basename; this correlates the on-disk path to the catalog row
- Timestamps use `time.Now().UnixMilli()` as `int64` parameters (NOT `NOW()` SQL function) — this makes the code SQLite-compatible for tests while being equally valid for MariaDB
- `file_name` is fetched from `f_user_file` in the SELECT (using `COALESCE(file_name, ?)` with basename fallback) for use in the `f_file_action` INSERT
- `AfterInject` always returns `nil` — best-effort means the caller's nil check is only a guard; all errors are logged and swallowed
- The SELECT step is the only gating step: if it returns `sql.ErrNoRows`, log and return; if it returns any other error, log and return (remaining steps skipped per AC4.1)
- Steps 3, 4, 5 (UPDATE/INSERT/UPDATE) each have independent error checks that log-and-continue per AC4.2 and AC4.3

```go
package processor

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// spcCatalog updates the Supernote Private Cloud MariaDB catalog after a
// successful OCR injection. All SQL operations are best-effort.
type spcCatalog struct {
	db *sql.DB
}

// NewSPCCatalog returns a CatalogUpdater backed by the given MariaDB connection.
func NewSPCCatalog(db *sql.DB) CatalogUpdater {
	return &spcCatalog{db: db}
}

// AfterInject updates the SPC catalog for the file at path. It performs five
// steps — stat+MD5, SELECT f_user_file, UPDATE f_user_file, INSERT f_file_action,
// UPDATE f_capacity — where each step after the SELECT is independent: a failure
// in one is logged and does not prevent the others from executing. If the SELECT
// finds no row, the remaining steps are skipped.
func (c *spcCatalog) AfterInject(ctx context.Context, path string) error {
	// Step 1: stat the file and compute its MD5.
	info, err := os.Stat(path)
	if err != nil {
		slog.Warn("spc catalog: stat failed", "path", path, "err", err)
		return nil
	}
	newSize := info.Size()

	f, err := os.Open(path)
	if err != nil {
		slog.Warn("spc catalog: open for md5 failed", "path", path, "err", err)
		return nil
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		slog.Warn("spc catalog: md5 compute failed", "path", path, "err", err)
		return nil
	}
	newMD5 := hex.EncodeToString(h.Sum(nil))

	// Step 2: look up the catalog row. inner_name is the file's basename — SPC
	// uses the filename as the stable identifier in f_user_file.
	innerName := filepath.Base(path)
	var fileID, userID, oldSize int64
	var fileName string
	err = c.db.QueryRowContext(ctx,
		"SELECT id, user_id, size, COALESCE(file_name, ?) FROM f_user_file WHERE inner_name = ?",
		innerName, innerName,
	).Scan(&fileID, &userID, &oldSize, &fileName)
	if err == sql.ErrNoRows {
		slog.Warn("spc catalog: no f_user_file row", "inner_name", innerName)
		return nil
	}
	if err != nil {
		slog.Warn("spc catalog: select f_user_file failed", "inner_name", innerName, "err", err)
		return nil
	}

	now := time.Now().UnixMilli()

	// Step 3: update f_user_file with new size, md5, and timestamp.
	if _, err := c.db.ExecContext(ctx,
		"UPDATE f_user_file SET size=?, md5=?, update_time=? WHERE id=?",
		newSize, newMD5, now, fileID,
	); err != nil {
		slog.Warn("spc catalog: update f_user_file failed", "file_id", fileID, "err", err)
	}

	// Step 4: insert an audit record. id uses UnixNano for uniqueness (fits bigint).
	actionID := time.Now().UnixNano()
	if _, err := c.db.ExecContext(ctx,
		`INSERT INTO f_file_action
			(id, user_id, file_id, file_name, inner_name, path, is_folder, size, md5, action, create_time, update_time)
			VALUES (?, ?, ?, ?, ?, 'NOTE/Note/', 'N', ?, ?, 'A', ?, ?)`,
		actionID, userID, fileID, fileName, innerName, newSize, newMD5, now, now,
	); err != nil {
		slog.Warn("spc catalog: insert f_file_action failed", "file_id", fileID, "err", err)
	}

	// Step 5: adjust quota by the size delta (may be negative if file shrank).
	delta := newSize - oldSize
	if _, err := c.db.ExecContext(ctx,
		"UPDATE f_capacity SET used_capacity = used_capacity + ?, update_time=? WHERE user_id=?",
		delta, now, userID,
	); err != nil {
		slog.Warn("spc catalog: update f_capacity failed", "user_id", userID, "err", err)
	}

	return nil
}
```

**Verification:**

```bash
go build -C /home/sysop/src/ultrabridge/.worktrees/spc-catalog-sync ./...
go vet -C /home/sysop/src/ultrabridge/.worktrees/spc-catalog-sync ./...
```

Expected: both succeed without errors.

**Commit:** `feat: add spcCatalog implementation`
<!-- END_TASK_2 -->

<!-- START_TASK_3 -->
### Task 3: Create catalog_test.go with full AC coverage

**Verifies:** spc-catalog-sync.AC1.1, spc-catalog-sync.AC1.2, spc-catalog-sync.AC1.3, spc-catalog-sync.AC1.4, spc-catalog-sync.AC2.1, spc-catalog-sync.AC2.2, spc-catalog-sync.AC3.1, spc-catalog-sync.AC3.2, spc-catalog-sync.AC4.1, spc-catalog-sync.AC4.2, spc-catalog-sync.AC4.3

**Files:**
- Create: `internal/processor/catalog_test.go`
- Reference: `internal/processor/worker_test.go` (test helper patterns: `openWorkerStore`, `seedNote`, `mockIndexer`)

Read `ed3d-house-style:writing-good-tests` before writing tests.

**Implementation:**

Test strategy overview:
- Tests are in `package processor` (same package as implementation — matches existing pattern)
- A `openCatalogDB` helper opens in-memory SQLite via `notedb.Open`, then creates the three SPC subset tables. Returns `*sql.DB` and a `*spcCatalog`.
- A `writeTempFile` helper writes content to a temp file and returns its path. This gives tests a real file to stat and MD5.
- For failure injection (AC4.2, AC4.3), use SQLite triggers with `RAISE(FAIL, ...)`: a trigger on `BEFORE UPDATE ON f_user_file` or `BEFORE INSERT ON f_file_action` that raises an error, simulating DB failures for those steps while leaving the other tables intact.
- For AC4.1 (SELECT fails), use a closed `*sql.DB`.

The three SPC subset tables use INTEGER timestamps (milliseconds) matching the Go-side `time.Now().UnixMilli()` in the implementation. `update_time` in f_user_file is `INTEGER` to accept the int64 parameter.

**SPC subset schema** (inject after `notedb.Open`):

```sql
CREATE TABLE f_user_file (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL,
    inner_name TEXT NOT NULL UNIQUE,
    file_name TEXT,
    size INTEGER NOT NULL DEFAULT 0,
    md5 TEXT NOT NULL DEFAULT '',
    update_time INTEGER
);

CREATE TABLE f_file_action (
    id INTEGER PRIMARY KEY,
    user_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    file_name TEXT,
    inner_name TEXT,
    path TEXT,
    is_folder TEXT,
    size INTEGER,
    md5 TEXT,
    action TEXT,
    create_time INTEGER,
    update_time INTEGER
);

CREATE TABLE f_capacity (
    user_id INTEGER PRIMARY KEY,
    used_capacity INTEGER NOT NULL DEFAULT 0,
    total_capacity INTEGER NOT NULL DEFAULT 0,
    update_time INTEGER
);
```

**Test list with AC mapping:**

| Test function | ACs verified |
|---|---|
| `TestAfterInject_UpdatesUserFile` | AC1.1, AC1.2, AC1.3 |
| `TestAfterInject_MissingUserFile` | AC1.4 |
| `TestAfterInject_InsertsFileAction` | AC2.1, AC2.2 |
| `TestAfterInject_AdjustsCapacity` | AC3.1 |
| `TestAfterInject_ZeroDeltaCapacity` | AC3.2 |
| `TestAfterInject_SelectFails` | AC4.1 |
| `TestAfterInject_UpdateFails_ContinuesToInsertAndCapacity` | AC4.2 |
| `TestAfterInject_InsertFails_ContinuesToCapacity` | AC4.3 |

**Test descriptions:**

- **`TestAfterInject_UpdatesUserFile`**: Write a file with known content to a temp path (set as `inner_name` basename). Insert a row in f_user_file and f_capacity. Call `AfterInject`. Query f_user_file and verify `size` matches `os.Stat`, `md5` matches `hex(md5(file_content))`, and `update_time` is ≥ the timestamp just before the call (use a time bound, not exact match, since `update_time` is set inside `AfterInject`).

- **`TestAfterInject_MissingUserFile`**: Write a temp file. Do NOT insert any f_user_file row. Call `AfterInject`. Verify it returns nil (no panic). Verify f_file_action and f_capacity tables remain empty.

- **`TestAfterInject_InsertsFileAction`**: Write a file, insert f_user_file and f_capacity rows. Call `AfterInject`. Query f_file_action: verify exactly one row exists with `action='A'`, `file_id` matching the f_user_file id, `user_id` matching, `md5` matching the file's MD5, `size` matching file size, `inner_name` = `filepath.Base(path)`. Verify `id` is non-zero and `create_time == update_time`.

- **`TestAfterInject_AdjustsCapacity`**: Insert f_user_file with `size=100`. Write a temp file with a different size (e.g., 200 bytes via `strings.Repeat`). Insert f_capacity with `used_capacity=1000`. Call `AfterInject`. Verify `used_capacity = 1000 + (200 - 100) = 1100`.

- **`TestAfterInject_ZeroDeltaCapacity`**: Write a temp file of N bytes. Insert f_user_file with `size=N` (same as actual file). Insert f_capacity with `used_capacity=500`. Call `AfterInject`. Verify `used_capacity` is still 500 (delta = 0).

- **`TestAfterInject_SelectFails`**: Open a `spcCatalog` with a closed `*sql.DB`. Write a temp file. Call `AfterInject`. Verify it returns nil (no panic). (The closed DB causes the SELECT to fail; best-effort means early return.)

- **`TestAfterInject_UpdateFails_ContinuesToInsertAndCapacity`**: Set up full schema. Insert f_user_file and f_capacity. Add a SQLite trigger: `CREATE TRIGGER fail_uf_update BEFORE UPDATE ON f_user_file BEGIN SELECT RAISE(FAIL, 'simulated failure'); END;`. Write a temp file. Call `AfterInject`. Verify: f_user_file row is unchanged (update was blocked by trigger). Verify: f_file_action has one row (INSERT still ran despite UPDATE failure). Verify: f_capacity.used_capacity changed (UPDATE f_capacity still ran).

- **`TestAfterInject_InsertFails_ContinuesToCapacity`**: Set up full schema. Insert f_user_file and f_capacity. Add a trigger: `CREATE TRIGGER fail_fa_insert BEFORE INSERT ON f_file_action BEGIN SELECT RAISE(FAIL, 'simulated failure'); END;`. Write a temp file. Call `AfterInject`. Verify: f_file_action is empty (INSERT was blocked). Verify: f_capacity.used_capacity changed (UPDATE f_capacity still ran).

**Helper functions to implement:**

```go
// openCatalogDB opens an in-memory SQLite DB with the notes schema plus
// the three SPC subset tables. Returns the DB and a spcCatalog backed by it.
func openCatalogDB(t *testing.T) (*sql.DB, *spcCatalog) {
    t.Helper()
    db, err := notedb.Open(context.Background(), ":memory:")
    if err != nil {
        t.Fatalf("notedb.Open: %v", err)
    }
    t.Cleanup(func() { db.Close() })
    mustExec(t, db, `CREATE TABLE f_user_file (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        user_id INTEGER NOT NULL,
        inner_name TEXT NOT NULL UNIQUE,
        file_name TEXT,
        size INTEGER NOT NULL DEFAULT 0,
        md5 TEXT NOT NULL DEFAULT '',
        update_time INTEGER
    )`)
    mustExec(t, db, `CREATE TABLE f_file_action (
        id INTEGER PRIMARY KEY,
        user_id INTEGER NOT NULL,
        file_id INTEGER NOT NULL,
        file_name TEXT,
        inner_name TEXT,
        path TEXT,
        is_folder TEXT,
        size INTEGER,
        md5 TEXT,
        action TEXT,
        create_time INTEGER,
        update_time INTEGER
    )`)
    mustExec(t, db, `CREATE TABLE f_capacity (
        user_id INTEGER PRIMARY KEY,
        used_capacity INTEGER NOT NULL DEFAULT 0,
        total_capacity INTEGER NOT NULL DEFAULT 0,
        update_time INTEGER
    )`)
    return db, &spcCatalog{db: db}
}

// mustExec runs a SQL statement and fails the test on error.
func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
    t.Helper()
    if _, err := db.Exec(query, args...); err != nil {
        t.Fatalf("mustExec %q: %v", query, err)
    }
}

// writeTempFile writes content to a temp file with the given basename and
// returns the full path.
func writeTempFile(t *testing.T, name, content string) string {
    t.Helper()
    path := filepath.Join(t.TempDir(), name)
    if err := os.WriteFile(path, []byte(content), 0644); err != nil {
        t.Fatal(err)
    }
    return path
}
```

**Verification:**

```bash
go test -C /home/sysop/src/ultrabridge/.worktrees/spc-catalog-sync ./internal/processor/ -run TestAfterInject -v
```

Expected: All `TestAfterInject_*` tests pass.

Then run the full package:

```bash
go test -C /home/sysop/src/ultrabridge/.worktrees/spc-catalog-sync ./internal/processor/
```

Expected: All tests pass (including existing worker tests).

**Commit:** `test: add spcCatalog tests covering AC1–AC4`
<!-- END_TASK_3 -->

<!-- END_SUBCOMPONENT_A -->
