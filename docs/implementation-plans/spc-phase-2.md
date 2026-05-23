# UB-as-SPC Phase 2 — File listing + capacity (read path) — Implementation Plan

Last verified: 2026-05-23

Phase 2 of the UB-as-SPC refactor (design plan: `docs/design-plans/2026-05-15-ub-as-spc-refactor.md`, Phase 2 section). Phase 1 (auth + tasks + Engine.IO) is complete and hardware-validated. This phase makes the device's **cloud-files UI browse UB read-only**, looking exactly like the real SPC to the device.

## Entry state

- Phase 1 exit state holds: `internal/spcserver/` serves auth + task endpoints; the Engine.IO socket is stable on the single `:8089` listener; JWT middleware works; `query/summary/{hash,group,id}` and `GET /api/file/query/server` are already stubbed (verified 2026-05-23).
- No file-listing endpoints implemented (device's "Private Cloud Sync Failed" is this phase).
- `UB_SPC_MODE=client` (default) remains regression-safe — no listener, no behavior change.

## Two design decisions locked before this plan (2026-05-23)

1. **Dedicated SPC file root.** The device browses a dedicated `UB_SPC_FILE_ROOT`, **not** UB's OCR `NotesPath`. On the live install the configured Supernote `notes_path` is `…/starkruzr@gmail.com/Supernote/Note` (the `Note` subtree only, 30 files), whereas the device expects its full native manifest at the storage root `…/starkruzr@gmail.com/Supernote/` (`Note/`, `Document/`, `EXPORT/`, `SCREENSHOT/`, `INBOX/`, `MyStyle/` — exactly the `bindEquipment` `label` manifest). File listing therefore walks the filesystem under `UB_SPC_FILE_ROOT` directly; it does **not** reuse `notestore` (which knows only `Note/` and stores SHA-256, while the device wants MD5).
2. **The device sees only the Supernote source.** The Boox source and its WebDAV-receiver filesystem are **invisible to the device** — no `/Boox` root, not merged, never listed. Boox stays a UB-internal / web-UI concern. This keeps UB looking exactly like real SPC.
3. **MD5/`content_hash` computed now.** EntriesVO `content_hash` / UserFileVO `md5` are MD5, computed **lazily on first sighting and cached** in the id-registry row (invalidated on size/mtime change). Keeps the read path honest and simplifies Phase 3 download.

## Validation approach (all sub-phases)

- **Tier 1 (every task):** `go test`, `go build`, `go vet` at repo root (`-C /home/jtd/ultrabridge`). TDD: test first.
- **Tier 2 (2d only):** real device (Supernote Nomad `SN078C10034074`) flipped to UB via NPM (`hydrae 192.168.9.30`, `docker exec sysop-app-1`, `13.conf` `set $port` 19072→8089 + `nginx -s reload`). Roll back by flipping `$port` back to 19072.
- Inline execution, task by task, commit per task, check in with the user. **No subagents.**
- DTO/VO field names are verbatim from `/home/sysop/spc-rev/cfr-decrypted/`; cite `<FQN.java>:<line>` in comments. When a wire detail is uncertain, **read the `.java` (server) or jadx-decompiled device app first — do not guess** (this is what made Phase 1 work).

## Wire facts established from the decompiled source (read once, here)

From `com/ratta/controller/F_FileLocalController.java` + `com/ratta/file/{vo,dto}/`:

- **`EntriesVO`** (Dropbox-style; the core listing entry): `{tag, id, name, path_display, content_hash, is_downloadable, size, lastUpdateTime, parent_path}`. `tag` is `"file"`/`"folder"`; `id` is **String**; `size`/`lastUpdateTime` are **Long**; `is_downloadable` **bool**.
- **`list_folder`** request `ListFolderLocalDTO{equipmentNo, id (Long), recursive (bool)}` → `ListFolderLocalVO extends BaseVO {equipmentNo, entries:[]EntriesVO}`. `list_folder_v3` request `ListFolderV3DTO` has the same three fields.
- **`query_v3`** request `FileQueryLocalDTO{equipmentNo, id (String)}` → `FileQueryLocalVO extends BaseVO {equipmentNo, entriesVO}` (singular entry).
- **`query/by/path_v3`** request `FileQueryByPathLocalDTO{equipmentNo, path (String)}` → `FileQueryByPathLocalVO extends BaseVO {equipmentNo, entriesVO}`.
- **`synchronous/start`** request `SynchronousStartLocalDTO{equipmentNo}` → `SynchronousStartLocalVO extends BaseVO {equipmentNo, synType (Boolean)}`. **`synchronous/end`** request `SynchronousEndLocalDTO{equipmentNo, flag}` → `SynchronousEndLocalVO extends BaseVO {equipmentNo}`.
- **`capacity/query`** (`F_FileLocalWebController`, but device hits it) → `CapacityVO{usedCapacity, totalCapacity}` (both Long). **`get_space_usage`** request `CapacityLocalDTO{equipmentNo}` → `CapacityLocalVO extends BaseVO {used (Long), allocationVO{tag, allocated (Long)}, equipmentNo}`.
- **userId** is parsed `Long.valueOf(jwtTokenUtil.userId(x-access-token))`; **`equipmentno`** header (lowercase) carries the equipment number.
- **Path non-normalization:** the device emits double slashes (`Personal//IMG_…`, §6). All path handling must `path.Clean`/tolerate this.

**Endpoints the device actually hit in the 0b capture** (so we know which to implement fully vs. provide as safe aliases): `list_folder` (2), `synchronous/start`+`end` (2 each), `query_v3` (2), `query/by/path_v3` (3), `capacity/query` (1). **Not** observed: `list_folder_v3`, `get_space_usage`, `create_folder_v2`, `query/deleteApi`. The unobserved ones are implemented as cheap aliases (v3/get_space_usage share logic) or canned-success stubs (create_folder, deleteApi) so the device never 404s, while honoring the "no device writes this phase" exit state.

---

## Acceptance Criteria Coverage

### spc-phase-2.AC1: ID registry (path↔id, persisted, lazy MD5)
- **spc-phase-2.AC1.1 Bidirectional:** `IDFor(path)` returns a stable positive int64; the same path always yields the same id; `PathFor(id)` round-trips to the original path. Distinct paths get distinct ids.
- **spc-phase-2.AC1.2 Persistence:** ids survive process restart (backed by a `spc_file_ids` table); a path seen in a prior run resolves to the same id after reopen.
- **spc-phase-2.AC1.3 Lazy MD5:** `MD5For(path)` computes lowercase MD5 hex on first call and caches it; a second call does not re-read the file; changing the file's size or mtime invalidates the cache and recomputes.

### spc-phase-2.AC2: Filesystem→VO mapping + capacity
- **spc-phase-2.AC2.1 Entry mapping:** `EntryFor(absPath)` builds a flat `EntriesVO` with correct `tag` (`folder`/`file`), `name` (basename), `size` (0 for folders), `lastUpdateTime` (mtime ms), `is_downloadable` (true for files, false for folders), `id` (registry id as string), and `content_hash` (MD5 for files, `""` for folders).
- **spc-phase-2.AC2.2 SPC paths:** `path_display` is the root-relative path with a leading `/` and forward slashes; `parent_path` is its parent (`/` at root). A request path with double slashes resolves to the same entry as the cleaned path, and never escapes `UB_SPC_FILE_ROOT` (no `..` traversal).
- **spc-phase-2.AC2.3 Capacity:** `Usage()` returns the recursive byte sum of files under the root, cached 60 s (a second call within the window does not re-walk). `CapacityVO{usedCapacity,totalCapacity}` and `CapacityLocalVO{used,allocationVO{tag,allocated},equipmentNo}` serialize flat with `totalCapacity`/`allocated` == `UB_SPC_QUOTA_BYTES`.

### spc-phase-2.AC3: Handlers + wiring
- **spc-phase-2.AC3.1 Sync session:** `synchronous/start` → flat `SynchronousStartLocalVO{success:true, equipmentNo, synType}`; `synchronous/end` → flat `SynchronousEndLocalVO{success:true, equipmentNo}`.
- **spc-phase-2.AC3.2 list_folder:** a `null`/absent/`0` `id` lists the file root; a folder `id` lists that folder's children; folders precede files; each child is a well-formed `EntriesVO`; `recursive:true` returns the whole subtree. `list_folder_v3` behaves identically.
- **spc-phase-2.AC3.3 query by id/path:** `query_v3` resolves `id`→entry; `by/path_v3` resolves (cleaned) `path`→entry; a missing id/path returns `success:true` with a null `entriesVO` (the device probes existence this way), never a 500.
- **spc-phase-2.AC3.4 Capacity endpoints:** `capacity/query` → `CapacityVO`; `get_space_usage` → `CapacityLocalVO`; both report non-zero `used` for a non-empty root and the configured total.
- **spc-phase-2.AC3.5 Write stubs:** `create_folder_v2` and `query/deleteApi` return well-formed `success:true` without mutating the filesystem (honors "no device writes this phase").
- **spc-phase-2.AC3.6 Wiring + regression:** all routes are registered behind `auth.Middleware`; `UB_SPC_MODE=client` still binds no listener and changes nothing; the existing OCR pipeline and web UI are untouched.

### spc-phase-2.AC4: Device acceptance (tier 2)
- **spc-phase-2.AC4.1 Native tree:** the device's cloud-files UI shows its native folders (`Note`, `Document`, `EXPORT`, `SCREENSHOT`, `INBOX`, `MyStyle`) with the real files inside.
- **spc-phase-2.AC4.2 Navigation:** navigating into a nested folder (e.g. `Note/Personal`, `Document/Books`) updates the listing.
- **spc-phase-2.AC4.3 Capacity meter:** the device's storage meter shows non-zero used and the configured total.
- **spc-phase-2.AC4.4 Isolation:** the Boox source is **not** visible anywhere in the device UI; the web UI and OCR pipeline continue to work; task sync (Phase 1) still works.

---

## Sub-phase 2a — Config + ID registry

### Task 1: SPC file-root + quota config keys

**Verifies:** infrastructure for AC1/AC2/AC3 (no AC of its own)

**Files:** `internal/appconfig/keys.go`, `config.go`, `appconfig_test.go`

**Implementation:** same six-spot pattern as Phase 1 config keys:
- consts `KeySPCFileRoot = "spc_file_root"`, `KeySPCQuotaBytes = "spc_quota_bytes"`.
- `envVarForKey` → `UB_SPC_FILE_ROOT`, `UB_SPC_QUOTA_BYTES`.
- `defaultValues`: `KeySPCFileRoot` → `""` (empty = file listing disabled, returns an empty root — keeps default config inert); `KeySPCQuotaBytes` → `"1099511627776"` (1 TiB).
- `restartRequired`: both → true.
- `Config` fields `SPCFileRoot string`, `SPCQuotaBytes int64`; wire `loadConfigFromDB` (parse quota with `strconv.ParseInt`, fall back to default on parse error) + `configToMap`.

**Testing:** default config has `SPCFileRoot == ""` and `SPCQuotaBytes == 1<<40`; a DB-set quota string round-trips; a malformed quota string falls back to the default (not 0).

**Commit:** `feat(spcserver): file-root + quota config keys`

### Task 2: `fileids` package — schema migration + path↔id registry

**Verifies:** spc-phase-2.AC1.1, spc-phase-2.AC1.2

**Files:** `internal/spcserver/fileids/fileids.go`, `fileids_test.go`

**Implementation:** new leaf-ish package owning its own table (precedent: `mcpauth.Migrate`, not `notedb.Open`, keeps the table gated to server mode).
- `Migrate(ctx, db) error` creates idempotently:
  ```sql
  CREATE TABLE IF NOT EXISTS spc_file_ids (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    path      TEXT NOT NULL UNIQUE,
    md5       TEXT NOT NULL DEFAULT '',
    md5_size  INTEGER NOT NULL DEFAULT 0,
    md5_mtime INTEGER NOT NULL DEFAULT 0
  );
  ```
- `type Registry struct { db *sql.DB; root string; ... }` + `New(db, root)`.
- `IDFor(ctx, absPath) (int64, error)`: `INSERT OR IGNORE INTO spc_file_ids(path) VALUES(?)` then `SELECT id` — assigns on first sighting, returns existing otherwise. (AUTOINCREMENT ensures stable ids; the root path is registered by `New` so it gets a deterministic low id.)
- `PathFor(ctx, id) (string, bool, error)`: `SELECT path WHERE id=?`.
- Root handling: `New` registers `root` so `RootID()` is known; the handler treats a `null`/`0` request id as the root.

**Testing:** `IDFor` is stable across calls for the same path and distinct across paths; `PathFor(IDFor(p)) == p`; `PathFor(unknown)` → `found=false`; reopening a fresh `Registry` on the same DB file preserves ids (AC1.2).

**Commit:** `feat(spcserver): fileids path↔id registry + migration`

### Task 3: `fileids` lazy MD5 cache

**Verifies:** spc-phase-2.AC1.3

**Files:** `internal/spcserver/fileids/fileids.go`, `fileids_test.go`

**Implementation:** `MD5For(ctx, absPath) (string, error)`:
- `stat` the file for size+mtime; `SELECT md5, md5_size, md5_mtime`. If a non-empty md5 exists and `md5_size`/`md5_mtime` match the current stat → return cached.
- Otherwise stream the file through `crypto/md5`, lowercase-hex-encode, `UPDATE` the row with md5+size+mtime, return it. Best-effort: a read error returns `("", err)` and the handler maps it to `content_hash:""` rather than failing the listing.

**Testing:** first `MD5For` returns the correct hex (golden against `md5sum` of a temp file); a second call returns the same value without re-reading (inject a counting reader or assert via a sentinel: rewrite the file's bytes *without* changing size/mtime → still returns the cached value; then bump mtime → recomputes to the new digest).

**Commit:** `feat(spcserver): fileids lazy MD5 cache`

---

## Sub-phase 2b — Mapping + capacity

### Task 4: `mapping/file.go` — filesystem entry → EntriesVO

**Verifies:** spc-phase-2.AC2.1, spc-phase-2.AC2.2

**Files:** `internal/spcserver/mapping/file.go`, `file_test.go`, `internal/spcserver/dto/file.go` (the `EntriesVO` type; see Task 6 note)

**Implementation:** `EntryFor(ctx, root, absPath string, reg *fileids.Registry) (dto.EntriesVO, error)`:
- `os.Lstat`; `tag` = `"folder"` if dir else `"file"`.
- `id` = `strconv.FormatInt(reg.IDFor(absPath), 10)`.
- `name` = `filepath.Base(absPath)`.
- `path_display` = `/` + root-relative slash path (`filepath.Rel(root, absPath)`, `filepath.ToSlash`, leading `/`); root itself → `/`.
- `parent_path` = `path_display` of `filepath.Dir`, `/` at root.
- `size` = 0 for dirs, else `info.Size()`.
- `lastUpdateTime` = `info.ModTime().UnixMilli()`.
- `is_downloadable` = `tag == "file"`.
- `content_hash` = `tag=="file"` → `reg.MD5For(absPath)` (empty on error), else `""`.
- A `safeResolve(root, reqPath)` helper: `path.Clean` the (possibly double-slashed) request path, reject anything escaping root (`..`), return the absolute fs path. Used by the path-based handlers.

**Testing:** table tests over a temp tree (a dir + a file): assert every field; a folder has `size==0`, `is_downloadable==false`, `content_hash==""`; `path_display`/`parent_path` are slash paths with leading `/`; `safeResolve` cleans `Note//Personal` to the same path as `Note/Personal` and rejects `../escape`.

**Commit:** `feat(spcserver): filesystem→EntriesVO mapping`

### Task 5: `capacity.go` — du-style usage + capacity VOs

**Verifies:** spc-phase-2.AC2.3

**Files:** `internal/spcserver/capacity.go`, `capacity_test.go`, capacity VO types in `dto/file.go`

**Implementation:**
- `type Meter struct { root string; quota int64; mu; cached int64; at time.Time }` + `New(root, quota)`.
- `Usage(ctx) int64`: if `time.Since(at) < 60s` return cached; else `filepath.WalkDir` summing regular-file sizes, store cached+at, return. Walk errors are logged and skipped (a vanished file mid-walk must not fail the meter).
- VO constructors: `CapacityVO{usedCapacity, totalCapacity}` and `CapacityLocalVO{BaseVO, used, allocationVO{tag:"individual", allocated}, equipmentNo}` (confirm `tag` literal against `AllocationVO` usage during execution; `"individual"` is the placeholder).

**Testing:** a temp tree with known sizes → `Usage` equals the sum; a second call within 60 s returns the cached value even after adding a file (assert no change until the window is bypassed via an injectable clock or an exported `invalidate()` test hook); VO marshals flat with the quota as total.

**Commit:** `feat(spcserver): capacity meter + capacity VOs`

---

## Sub-phase 2c — Handlers + route wiring

### Task 6: File DTOs/VOs

**Verifies:** types for AC2/AC3 (no test of its own)

**Files:** `internal/spcserver/dto/file.go`

**Implementation:** declare, with verbatim field names + JSON tags and `<FQN.java>:<line>` citations: `EntriesVO`, `ListFolderLocalDTO`, `ListFolderLocalVO`, `FileQueryLocalDTO`, `FileQueryLocalVO`, `FileQueryByPathLocalDTO`, `FileQueryByPathLocalVO`, `SynchronousStartLocalDTO/VO`, `SynchronousEndLocalDTO/VO`, `CapacityLocalDTO`, `CapacityLocalVO`, `AllocationVO`, `CapacityVO`, and the create-folder + deleteApi DTOs. VOs that the device reads as a flat envelope embed `envelope.BaseVO` (anonymous). Note the snake_case wire fields (`path_display`, `content_hash`, `is_downloadable`) — JSON-tag them exactly.

**Commit:** `feat(spcserver): file listing DTOs/VOs`

### Task 7: Sync-session handlers (start/end)

**Verifies:** spc-phase-2.AC3.1

**Files:** `internal/spcserver/handlers/files.go`, `files_test.go`

**Implementation:** `FileHandler` struct holding `Root string`, `Quota int64`, `Reg *fileids.Registry`, `Meter *spcserver.Meter` (or pass the meter in), `Logger`. (If the file root is empty, handlers return an empty-but-valid root listing / zero usage so default config stays inert.)
- `SynchronousStart`: decode DTO, echo `equipmentNo`, return `synType`. **Read `F_FileLocalServiceImpl.synchronousStart` first** to determine `synType` semantics (full vs incremental sync signal); default to the value real SPC returns for a normal session and confirm on device in 2d.
- `SynchronousEnd`: decode, echo `equipmentNo`, `success:true`.

**Testing:** httptest POST → assert flat `{success:true, equipmentNo:…, synType:…}` and `{success:true, equipmentNo:…}`.

**Commit:** `feat(spcserver): file sync-session handlers`

### Task 8: list_folder (+ v3 alias)

**Verifies:** spc-phase-2.AC3.2

**Files:** `internal/spcserver/handlers/files.go`, `files_test.go`

**Implementation:** `ListFolder`: decode `ListFolderLocalDTO`; resolve parent abs path (`id` null/`0` → root, else `Reg.PathFor(id)`; unknown id → empty entries, `success:true`); `os.ReadDir`; map each child via `mapping.EntryFor`; **sort folders-before-files then by name**; if `recursive`, walk the subtree. Return `ListFolderLocalVO{OK(), equipmentNo, entries}`. `ListFolderV3` is the same logic (alias) — register both routes to it.

**Testing:** temp root with a nested dir + files → root listing returns the top-level entries folders-first; listing a child folder's id returns its children; `recursive:true` returns the flattened subtree; unknown id → empty list + success.

**Commit:** `feat(spcserver): list_folder + list_folder_v3 handlers`

### Task 9: query_v3 + by/path_v3

**Verifies:** spc-phase-2.AC3.3

**Files:** `internal/spcserver/handlers/files.go`, `files_test.go`

**Implementation:**
- `QueryByID`: decode `FileQueryLocalDTO`; `Reg.PathFor(id)`; if found+exists → `EntryFor` → `FileQueryLocalVO{OK(), equipmentNo, entriesVO}`; if missing → `OK()` with nil `entriesVO`.
- `QueryByPath`: decode `FileQueryByPathLocalDTO`; `safeResolve(root, path)`; `os.Lstat`; found → entry; missing → `OK()` + nil entry. Tolerate double-slash paths.

**Testing:** known id/path → populated `entriesVO`; unknown id and a non-existent path → `success:true`, `entriesVO` null; `Note//Personal` resolves like `Note/Personal`.

**Commit:** `feat(spcserver): query_v3 + query/by/path_v3 handlers`

### Task 10: capacity/query + get_space_usage

**Verifies:** spc-phase-2.AC3.4

**Files:** `internal/spcserver/handlers/files.go`, `files_test.go`

**Implementation:** `CapacityQuery` → `CapacityVO{usedCapacity: Meter.Usage(), totalCapacity: Quota}`. `GetSpaceUsage` → `CapacityLocalVO{OK(), used: Meter.Usage(), allocationVO:{tag, allocated: Quota}, equipmentNo}`.

**Testing:** non-empty root → `usedCapacity`/`used` > 0 and equal each other; `totalCapacity`/`allocated` == quota; both flat.

**Commit:** `feat(spcserver): capacity/query + get_space_usage handlers`

### Task 11: create_folder_v2 + query/deleteApi stubs

**Verifies:** spc-phase-2.AC3.5

**Files:** `internal/spcserver/handlers/files.go`, `files_test.go`

**Implementation:** both return well-formed `success:true` **without touching the filesystem** (not observed in 0b; honors no-writes exit state). `CreateFolderV2` → its success VO with an echoed `equipmentNo`; `QueryByIdDeleteApi` → `FileQueryV2VO`-shaped success with a null entry. A code comment records that these become real in Phase 4/5.

**Testing:** POST → flat success; assert no directory was created under a temp root.

**Commit:** `feat(spcserver): create_folder + deleteApi canned-success stubs`

### Task 12: Wire routes + main.go construction; no-device curl loop

**Verifies:** spc-phase-2.AC3.6 (operational), AC2.3/AC3.2 end-to-end over HTTP

**Files:** `internal/spcserver/server.go`, `cmd/ultrabridge/main.go`, `internal/spcserver/CLAUDE.md`

**Implementation:**
- `server.Config` gains `FileRoot string`, `QuotaBytes int64`; `server.go` constructs `fileids.Registry` + `Meter` + `FileHandler` and registers all routes behind `protect(...)`:
  `POST /api/file/2/files/synchronous/start`, `…/synchronous/end`, `…/list_folder`, `POST /api/file/3/files/list_folder_v3`, `…/query_v3`, `…/query/by/path_v3`, `POST /api/file/2/files/create_folder_v2`, `POST /api/file/capacity/query`, `POST /api/file/2/users/get_space_usage`, `POST /api/file/2/files/query/deleteApi`.
- `main.go` (server mode only): call `fileids.Migrate(ctx, notedb)`; pass `cfg.SPCFileRoot`/`cfg.SPCQuotaBytes` into `server.Config`. Client mode unchanged.
- Update `internal/spcserver/CLAUDE.md` (status → Phase 2, layout: `fileids/`, `capacity.go`, `handlers/files.go`, `mapping/file.go`; file-root + Boox-invisibility invariants).

**Testing:** `go build`/`vet`/`test` green. Manual: run UB `server` mode with `UB_SPC_FILE_ROOT=<temp tree>`; mint a token via the Phase 1 login loop; `curl` `list_folder` (null id) → top-level entries; `curl` into a child id → its children; `curl` `capacity/query` → non-zero used + quota total; `curl` `by/path_v3` with a double-slash path → resolves. Defaults (no `UB_SPC_MODE`) → `:8089` closed, UB unchanged (AC3.6 regression).

**Commit:** `feat(spcserver): wire file-listing routes + main.go construction`

---

## Sub-phase 2d — Device acceptance (tier 2)

### Task 13: Device file-browse acceptance

**Verifies:** spc-phase-2.AC4.1, AC4.2, AC4.3, AC4.4

**Files:** none (docs-only if no fix needed)

**Steps:**
1. Set `UB_SPC_FILE_ROOT=/mnt/supernote/supernote_data/starkruzr@gmail.com/Supernote` in `docker-compose.yml` (uncommitted; carries the device password) and redeploy on neptune.
2. Flip NPM (`hydrae`, `13.conf` `$port` 19072→8089 + reload).
3. On the device: open the cloud-files UI → confirm the native folder tree (`Note`, `Document`, `EXPORT`, `SCREENSHOT`, `INBOX`, `MyStyle`) with real files (AC4.1); navigate into `Note/Personal` and `Document/Books` (AC4.2); check the storage meter (AC4.3); confirm no Boox content appears anywhere and that task sync + the web UI still work (AC4.4).
4. Watch logs / the tap if anything 404s or errors; fix forward (most likely `synType`, an unexpected DTO field, or a path-display format the device rejects — read the relevant `.java`/device app rather than guessing).
5. Flip NPM back to 19072.

**Acceptance:** device browses UB's file tree read-only, sees its native folders and files, shows a sane capacity meter, and never sees Boox.

**Commit:** `docs(spcserver): record Phase 2 device-acceptance result`

---

## Exit state

- Build/test/vet green; `client` mode still inert.
- Real device's cloud-files UI browses the Supernote storage root read-only; nested navigation works; capacity meter populated; Boox invisible; Phase 1 task sync intact.
- No device writes yet (create_folder/delete are canned stubs).

## Files this phase touches

**Created:** `internal/spcserver/fileids/{fileids.go,fileids_test.go}`, `internal/spcserver/mapping/file.go` (+test), `internal/spcserver/capacity.go` (+test), `internal/spcserver/handlers/files.go` (+test), `internal/spcserver/dto/file.go`, `docs/implementation-plans/spc-phase-2.md`.
**Modified:** `internal/appconfig/{keys.go,config.go}` (+test), `internal/spcserver/server.go`, `cmd/ultrabridge/main.go`, `internal/spcserver/CLAUDE.md`, `docker-compose.yml` (uncommitted).
**Not touched:** `tasksync`/`sync`/`taskdb`/`notestore`/`notedb` packages (file listing reads the filesystem directly, not notestore).

## Compact/clear checkpoint

Merge Phase 2. `/clear` before Phase 3 (download — OSS presigned-URL byte streaming; the lazy MD5 cache built here feeds Phase 3's `content_hash` diffing).
