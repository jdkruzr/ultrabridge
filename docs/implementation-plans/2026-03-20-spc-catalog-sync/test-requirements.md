# SPC Catalog Sync -- Test Requirements

This document maps every acceptance criterion from the SPC Catalog Sync design to
either an automated test or a human verification step.

---

## AC-to-Test Mapping

| AC Identifier | Description | Verification | Test File / Approach |
|---|---|---|---|
| spc-catalog-sync.AC1.1 | `f_user_file.size` equals byte count of modified file | Automated | `internal/processor/catalog_test.go` -- `TestAfterInject_UpdatesUserFile` |
| spc-catalog-sync.AC1.2 | `f_user_file.md5` equals MD5 hex digest of modified file | Automated | `internal/processor/catalog_test.go` -- `TestAfterInject_UpdatesUserFile` |
| spc-catalog-sync.AC1.3 | `f_user_file.update_time` is updated to current timestamp | Automated | `internal/processor/catalog_test.go` -- `TestAfterInject_UpdatesUserFile` |
| spc-catalog-sync.AC1.4 | Missing `f_user_file` row: no update attempted, job completes | Automated | `internal/processor/catalog_test.go` -- `TestAfterInject_MissingUserFile` |
| spc-catalog-sync.AC2.1 | `f_file_action` row with `action='A'` inserted with correct fields | Automated | `internal/processor/catalog_test.go` -- `TestAfterInject_InsertsFileAction` |
| spc-catalog-sync.AC2.2 | Inserted row has unique `id` and matching `create_time`/`update_time` | Automated | `internal/processor/catalog_test.go` -- `TestAfterInject_InsertsFileAction` |
| spc-catalog-sync.AC3.1 | `f_capacity.used_capacity` adjusted by `new_size - old_size` | Automated | `internal/processor/catalog_test.go` -- `TestAfterInject_AdjustsCapacity` |
| spc-catalog-sync.AC3.2 | Zero delta (same size) proceeds without error | Automated | `internal/processor/catalog_test.go` -- `TestAfterInject_ZeroDeltaCapacity` |
| spc-catalog-sync.AC4.1 | SELECT failure: remaining steps skipped, job completes | Automated | `internal/processor/catalog_test.go` -- `TestAfterInject_SelectFails` |
| spc-catalog-sync.AC4.2 | UPDATE failure: INSERT and capacity UPDATE still execute | Automated | `internal/processor/catalog_test.go` -- `TestAfterInject_UpdateFails_ContinuesToInsertAndCapacity` |
| spc-catalog-sync.AC4.3 | INSERT failure: capacity UPDATE still executes | Automated | `internal/processor/catalog_test.go` -- `TestAfterInject_InsertFails_ContinuesToCapacity` |
| spc-catalog-sync.AC5.1 | `AfterInject` called on success path after `executeJob` returns nil | Automated | `internal/processor/worker_test.go` -- `TestWorker_CatalogUpdaterCalledOnSuccess` |
| spc-catalog-sync.AC5.2 | `AfterInject` not called when `executeJob` returns an error | Automated | `internal/processor/worker_test.go` -- `TestWorker_CatalogUpdaterNotCalledOnFailure` |
| spc-catalog-sync.AC6.1 | Nil `CatalogUpdater` causes no panic, behavior unchanged | Automated | `internal/processor/worker_test.go` -- `TestWorker_NilCatalogUpdater` |

---

## Automated Tests

### Phase 1 -- `internal/processor/catalog_test.go`

Tests use in-memory SQLite (via `notedb.Open`) with three SPC subset tables
(`f_user_file`, `f_file_action`, `f_capacity`). Failure injection uses SQLite
`RAISE(FAIL, ...)` triggers.

| Test Function | ACs Verified | Description |
|---|---|---|
| `TestAfterInject_UpdatesUserFile` | AC1.1, AC1.2, AC1.3 | Writes a temp file with known content, calls `AfterInject`, then asserts `f_user_file.size` matches `os.Stat`, `md5` matches computed hex digest, and `update_time` is within a reasonable bound of `time.Now().UnixMilli()`. |
| `TestAfterInject_MissingUserFile` | AC1.4 | Calls `AfterInject` with no `f_user_file` row seeded. Asserts return is nil (no panic), and that `f_file_action` and `f_capacity` tables remain empty. |
| `TestAfterInject_InsertsFileAction` | AC2.1, AC2.2 | Seeds `f_user_file` and `f_capacity`, calls `AfterInject`, then queries `f_file_action` for exactly one row. Asserts `action='A'`, correct `file_id`, `user_id`, `md5`, `size`, `inner_name`, non-zero `id`, and `create_time == update_time`. |
| `TestAfterInject_AdjustsCapacity` | AC3.1 | Seeds `f_user_file` with `size=100`, `f_capacity` with `used_capacity=1000`, writes a 200-byte temp file. Calls `AfterInject` and asserts `used_capacity = 1100` (1000 + 100 delta). |
| `TestAfterInject_ZeroDeltaCapacity` | AC3.2 | Seeds `f_user_file` with `size=N`, `f_capacity` with `used_capacity=500`, writes a temp file of exactly N bytes. Calls `AfterInject` and asserts `used_capacity` remains 500. |
| `TestAfterInject_SelectFails` | AC4.1 | Creates an `spcCatalog` backed by a closed `*sql.DB`. Calls `AfterInject` and asserts it returns nil without panicking. |
| `TestAfterInject_UpdateFails_ContinuesToInsertAndCapacity` | AC4.2 | Seeds tables normally, then installs a `BEFORE UPDATE ON f_user_file` trigger that raises an error. Calls `AfterInject` and asserts: `f_user_file` row is unchanged (UPDATE blocked), `f_file_action` has one row (INSERT succeeded), `f_capacity.used_capacity` changed (capacity UPDATE succeeded). |
| `TestAfterInject_InsertFails_ContinuesToCapacity` | AC4.3 | Seeds tables normally, then installs a `BEFORE INSERT ON f_file_action` trigger that raises an error. Calls `AfterInject` and asserts: `f_file_action` is empty (INSERT blocked), `f_capacity.used_capacity` changed (capacity UPDATE succeeded). |

### Phase 2 -- `internal/processor/worker_test.go`

Tests use a `mockCatalogUpdater` that records whether `AfterInject` was called
and with which path.

| Test Function | ACs Verified | Description |
|---|---|---|
| `TestWorker_CatalogUpdaterCalledOnSuccess` | AC5.1 | Runs `processJob` with a valid `.note` file and a `mockCatalogUpdater`. Asserts the job completes as done, `AfterInject` was called, and the path argument matches `job.NotePath`. |
| `TestWorker_CatalogUpdaterNotCalledOnFailure` | AC5.2 | Runs `processJob` with a nonexistent file path to force `executeJob` failure. Asserts the job does not complete as done and `AfterInject` was never called. |
| `TestWorker_NilCatalogUpdater` | AC6.1 | Runs `processJob` with `WorkerConfig{}` (nil `CatalogUpdater`). Asserts the job completes as done with no panic. |

---

## Human Verification

All acceptance criteria are fully covered by automated tests. No human
verification steps are required.

**Rationale:** The `spcCatalog` implementation performs straightforward SQL
operations against well-defined table schemas. The in-memory SQLite approach
faithfully models the MariaDB subset schema, and SQLite triggers provide
reliable failure injection for the best-effort isolation criteria. The worker
integration tests use mock objects to verify call/no-call behavior and nil
safety. The `main.go` wiring (Phase 2, Task 2) is a single assignment with no
branching logic and is validated by `go build` and `go vet` -- no dedicated
test is needed beyond compilation.

One area to note: the `main.go` wiring itself is not unit-tested (it assigns
`processor.NewSPCCatalog(database)` to `workerCfg.CatalogUpdater`). This is
intentional -- it is pure wiring code with no conditional logic. Correct
compilation is sufficient verification. If desired, an end-to-end smoke test
against a real MariaDB instance could confirm the full pipeline, but this falls
outside the scope of automated unit/integration tests defined here.
