# UB-as-SPC Phase 4 — Files: upload (device → cloud) + mutations + catalog cutover — Implementation Plan

Last verified: 2026-05-23

This is the highest-risk, largest-scope phase. It is subdivided into four
independently-mergeable sub-phases (4a–4d) plus device acceptance. Worked the
same way as Phases 1–3: inline, TDD per task, commit per task, check in with the
user between tasks. No subagent chains (see memory `feedback_inline_oversight_over_plan_execute`).

## Entry state
- Phase 3 exit state holds: download read-path works and is hardware-validated;
  the OSS signing primitive (`internal/spcserver/oss`) is proven — **its
  `UploadSignature`/`ValidateUpload` already exist and are golden-mastered**, so
  Phase 4 needs *no new crypto*.
- `UB_SPC_FILE_ROOT` is the device's native six-folder tree (dedicated, distinct
  from the OCR `NotesPath`); `fileids.Registry`, `mapping.EntryFor`,
  `mapping.SafeResolve` are in place and reused.
- `internal/processor/catalog.go` still exists and writes through to MariaDB.
- `UB_SPC_MODE=client` (default) binds no listener and must stay regression-safe.

## Design decisions locked before this plan (2026-05-23)
1. **Upload→OCR wiring is deferred to 4d.** 4a/4b get the device round-tripping
   first (file lands in `FILE_ROOT`, md5-verified, FILE-SYN fires); the explicit
   `processor.Enqueue` kick and the MariaDB catalog cutover are wired together in
   4d. (User decision, this session.)
2. **UB controls both ends of the presigned upload URL.** `upload/apply` mints
   `fullUploadUrl` = `/api/oss/upload?...&path=base64url(<innerName>)`; the device
   POSTs to it opaquely (never computes a signature). So the `path` query param
   carries the server-chosen **innerName** (a UUID), *not* the human target path —
   bytes always stage under a server-controlled name, and the human target path
   only materializes at `finish` after md5/size verification. This keeps the
   unverified upload byte-stream off the real tree entirely (defense in depth).
3. **Staging lives under `FILE_ROOT`**, not `NotesPath`: `<FILE_ROOT>/.staging/<innerName>`.
   The `.staging/` and `.recycle/` dot-dirs are already invisible to `list_folder`
   (it skips dot-prefixed entries — verify and assert).
4. **Upload byte-stream errors mirror download's U1**: bad/expired/tampered
   signature → **HTTP 500 + bare plain-text** (`FileUploadException` →
   `GlobalExceptionHandler`), not a JSON envelope. `/api/oss/upload` is **tokenless**
   (signature is its only auth, like the download GET — U2); `apply`/`finish` are
   JWT-protected.

## Validation approach (all sub-phases)
- **Tier 1 (automated):** `go test`/`vet`/`build` green; httptest handlers against
  a temp `FILE_ROOT` + package-local-migrated notedb; md5 golden against `md5sum`.
- **Tier 2 (device):** real Supernote Nomad `SN078C10034074` via the NPM re-flip
  recipe (memory `reference_spc_dev_topology`). tcpdump the cleartext NPM→neptune
  `:8089` leg when the wire is in doubt (the move that cracked Phase 3).

## Wire facts established from the decompiled source (read once, here)
From `/home/sysop/spc-rev/cfr-decrypted/` (`O_OssLocalController`,
`F_FileLocalController`, `com/ratta/file/{dto,vo}/FileUpload*`):

- **`POST /api/file/3/files/upload/apply`** — req `FileUploadApplyLocalDTO`
  `{equipmentNo, path, fileName, size}` (all String). resp `FileUploadApplyLocalVO`
  (extends BaseVO) `{equipmentNo, bucketName, innerName, xAmzDate, authorization,
  fullUploadUrl, partUploadUrl}`. UB fills `innerName` + `fullUploadUrl`; the AWS-
  style `bucketName/xAmzDate/authorization` are for SPC's real-OSS path and are
  left empty (the device uses `fullUploadUrl`). `partUploadUrl` empty (no chunked).
- **`POST /api/oss/upload`** — `multipart/form-data` with a **`file`** part, plus
  query `signature, timestamp, nonce, path`. SPC validates with **fileSize `0L`**
  (`O_OssLocalController:102`). resp `UploadFileVO` (bare BaseVO `success:true`).
- **`POST /api/file/2/files/upload/finish`** — req `FileUploadFinishLocalDTO`
  `{equipmentNo, path, size, fileName, content_hash, innerName}` — `content_hash`
  is **snake_case** (the lone snake_case request field, §8) and is the MD5. resp
  `FileUploadFinishLocalVO` (extends BaseVO) `{equipmentNo, path_display, id
  (String), size (Long), name, content_hash}`.
- **Device confirmed (0b) to use the `/api/file/3|2/files/upload/*` variants, NOT
  `/api/file/terminal/upload/*`** (spc-protocol §11). Terminal variants: skip.
- Mutations (`F_FileLocalController`): `delete_folder_v3` (:123), `move_v3` (:177),
  `copy_v3` (:184). DTO field names to be read verbatim at task time.
- File IDs are **String-in / Long-or-String-out** (§8); parse with `ParseInt` in
  handlers, same trap that bit download_v3.

## Acceptance Criteria Coverage

### spc-phase-4.AC1: Staging infrastructure (`internal/spcserver/staging`)
- **spc-phase-4.AC1.1 Schema:** a `spc_uploads` table (innerName PK, target path,
  claimed md5, claimed size, status, created_at/expires_at) is migrated
  package-locally in server mode (à la `fileids.Migrate`/`mcpauth.Migrate`), **not**
  by `notedb.Open`; idempotent; applies cleanly to a fresh notedb.
- **spc-phase-4.AC1.2 Stage:** `Stage(innerName, r io.Reader)` streams bytes to
  `<FILE_ROOT>/.staging/<innerName>` and returns the bytes-written count; creates
  `.staging/` if absent; an innerName containing a path separator or `..` is rejected.
- **spc-phase-4.AC1.3 Verify:** `Finalize` recomputes MD5 + size of the staged
  file and rejects (without promoting) when either mismatches the claimed values.
- **spc-phase-4.AC1.4 Atomic promote:** on match, the staged file is `os.Rename`d
  (atomic, same filesystem) to `SafeResolve(FILE_ROOT, <path>/<fileName>)`; parent
  dirs created; a target escaping `FILE_ROOT` is refused; the staged temp never
  lingers on success.
- **spc-phase-4.AC1.5 Orphan cleanup:** staged files older than the apply TTL are
  removed by a sweep (called on a timer in server mode); a fresh staged file is
  untouched. Injectable clock drives the test.

### spc-phase-4.AC2: Upload DTOs/VOs (types; no test of their own)
- `FileUploadApplyLocalDTO/VO`, `UploadFileVO`, `FileUploadFinishLocalDTO/VO` with
  field names verbatim (incl. snake_case `content_hash`, `path_display`); VOs embed
  `envelope.BaseVO`; `UploadFileVO` is a bare BaseVO.

### spc-phase-4.AC3: Upload endpoints (`internal/spcserver/handlers/upload.go`)
- **spc-phase-4.AC3.1 apply:** a valid `FileUploadApplyLocalDTO` returns a flat
  `FileUploadApplyLocalVO` with a non-empty `innerName` (UUID) and a `fullUploadUrl`
  of the form `{proto}://{host}/api/oss/upload?signature=…&timestamp=…&nonce=…&path=…`
  whose `signature` validates (`ValidateUpload`, fileSize 0) and whose `path`
  decrypts to the innerName; a `spc_uploads` row is recorded (status applied).
- **spc-phase-4.AC3.2 oss/upload:** a multipart POST with a valid signed URL streams
  the `file` part into staging (md5/size of the staged file == posted bytes); returns
  `UploadFileVO{success:true}`. A bad/expired/tampered signature → **HTTP 500 +
  plain-text** (`"Signature verification failed."`), no bytes staged.
- **spc-phase-4.AC3.3 finish:** a `FileUploadFinishLocalDTO` whose `content_hash`+`size`
  match the staged file promotes it to `<path>/<fileName>` under `FILE_ROOT`, mints/
  returns its `id` (string) via `Reg.IDFor`, and returns a flat `FileUploadFinishLocalVO`
  with `path_display, id, size (Long), name, content_hash`. A mismatch returns the
  SPC error envelope and leaves the target absent. On success a FILE-SYN STARTSYNC
  push is fired (best-effort).
- **spc-phase-4.AC3.4 Auth boundary:** `apply` and `finish` are behind
  `auth.Middleware`; `POST /api/oss/upload` is reachable **without** an
  `x-access-token` (signature is its auth — U2).
- **spc-phase-4.AC3.5 Round-trip (no device):** apply → oss/upload → finish over
  httptest/curl promotes a known blob; on-disk md5 == posted md5; the file then
  appears in `list_folder`/`query_v3`.

### spc-phase-4.AC4: Mutation endpoints (`internal/spcserver/handlers/mutation.go`)
- **spc-phase-4.AC4.1 delete:** `delete_folder_v3` soft-deletes — moves the target to
  `<FILE_ROOT>/.recycle/<timestamp>/<originalRelPath>`; it disappears from
  `list_folder`/`query_v3`; the registry entry is invalidated.
- **spc-phase-4.AC4.2 move:** `move_v3` atomically renames within the tree (SafeResolve
  both ends); the registry path↔id mapping follows the file (id stable).
- **spc-phase-4.AC4.3 copy:** `copy_v3` disk-copies and the copy gets a fresh id; both
  paths list.
- **spc-phase-4.AC4.4 Auth + push:** all three are JWT-protected and fire FILE-SYN on
  success; traversal-guarded on every path argument.

### spc-phase-4.AC5: Pipeline integration + catalog cutover (4d)
- **spc-phase-4.AC5.1 OCR kick:** when a finished upload's target is a `.note`/`.pdf`
  that falls under the OCR-watched tree, `processor.Enqueue(path)` is called
  best-effort (failure logged, not propagated); the text becomes searchable.
- **spc-phase-4.AC5.2 Catalog cutover:** `internal/processor/catalog.go` +
  `catalog_test.go` deleted; `WorkerConfig.CatalogUpdater` + the `AfterInject()` call
  removed; the worker updates `notedb.notes.md5`/`size` directly post-injection;
  `grep -r "catalog.go\|CatalogUpdater\|AfterInject" internal/processor/` is empty;
  no MariaDB calls observed during OCR. `cmd/ultrabridge/main.go` stops passing the
  MariaDB pool into the processor (the pool stays — `tasksync/supernote` still uses it).
- **spc-phase-4.AC5.3 Regression:** `UB_SPC_MODE=client` binds no listener and changes
  nothing; existing OCR pipeline behavior is otherwise identical (Supernote + Boox).

### spc-phase-4.AC6: Device acceptance (tier 2)
- **spc-phase-4.AC6.1 Upload:** the device uploads a new/modified `.note`; it lands in
  `FILE_ROOT`, round-trip md5 matches, and it appears in the device's cloud browse.
  Within ~30s (post-4d) its text is searchable in the UB web UI.
- **spc-phase-4.AC6.2 Delete:** device delete → file moves to `.recycle/`, disappears
  from the device list and the web UI.
- **spc-phase-4.AC6.3 Move/copy:** device move/rename and copy behave correctly.
- **spc-phase-4.AC6.4 Isolation intact:** Boox still invisible; task sync (Phase 1),
  browse + download (Phase 2/3), and the web UI all still work.

---

## Sub-phase 4a — Staging infrastructure (pure infra, no endpoints)

### Task 1: `spc_uploads` schema + package-local migration
**Verifies:** spc-phase-4.AC1.1
- Add `internal/spcserver/staging/` (leaf-ish; imports notedb sql + oss only).
- `Migrate(ctx, db)` creates `spc_uploads` (innerName TEXT PK, target_path TEXT,
  file_name TEXT, claimed_md5 TEXT, claimed_size INTEGER, status TEXT, created_at
  INTEGER, expires_at INTEGER); idempotent. Called from server.go in server mode.
**Testing:** migrate a fresh `:memory:`/temp notedb twice → no error; row insert/read.

### Task 2: staging Store (stage / verify / atomic promote / orphan sweep)
**Verifies:** spc-phase-4.AC1.2, AC1.3, AC1.4, AC1.5
- `Store{Root, DB, Now}`; `Stage`, `Finalize(innerName, claimedMD5, claimedSize,
  targetRelPath) (absPath, error)`, `Sweep(ctx)`.
- Reuse `mapping.SafeResolve` for the target; reject separator/`..` in innerName.
**Testing:** AC1.2 stream a blob → staged size matches; bad innerName rejected.
AC1.3 md5/size mismatch → error, no promote. AC1.4 promote → file at target, temp
gone, traversal refused. AC1.5 injected clock: stale staged file swept, fresh kept.

---

## Sub-phase 4b — Upload endpoints (happy path)

### Task 3: Upload DTOs/VOs
**Verifies:** types for AC2/AC3 (no test of its own)
- Add to `internal/spcserver/dto/file.go`: the four upload types, field names
  verbatim; `UploadFileVO` bare BaseVO; apply/finish VOs embed BaseVO.

### Task 4: `upload/apply`, `oss/upload`, `upload/finish` handlers
**Verifies:** spc-phase-4.AC3.1, AC3.2, AC3.3, AC3.4
- `UploadHandler{Root, Reg, Signer, Staging, Notifier, Logger}`.
- `Apply`: gen innerName (UUIDv4, reuse `newNonce`-style), mint signed
  `/api/oss/upload?...&path=base64url(innerName)` via `Signer.UploadSignature(...,0)`,
  insert `spc_uploads` row, return flat VO.
- `UploadStream` (`POST /api/oss/upload`): `r.ParseMultipartForm`, validate sig
  (fileSize 0) + window before touching disk; on bad sig → `uploadError` (500 +
  plain text, mirror download's `downloadError`); `Staging.Stage(innerName, part)`.
- `Finish`: look up `spc_uploads` row by innerName, `Staging.Finalize(...)` (md5/size
  verify + promote), `Reg.IDFor(target)`, fire `Notifier.Notify` best-effort, return
  flat `FileUploadFinishLocalVO`. Parse any String ids with `ParseInt`.
**Testing:** httptest, temp Root + migrated DB. AC3.1 apply → parse fullUploadUrl,
assert `ValidateUpload` accepts + `DecryptPath(path)==innerName` + row written.
AC3.2 multipart POST to the minted URL → staged; tampered sig → 500 plain-text, nothing
staged. AC3.3 finish with correct md5/size → file promoted, VO fields correct (md5
golden), id non-empty; wrong md5 → error envelope, target absent. AC3.4 apply/finish
require token (E0712 without), oss/upload reachable tokenless.

### Task 5: Wire upload routes + main.go construction + no-device curl loop
**Verifies:** spc-phase-4.AC3.5, AC5.3 (regression)
- server.go: share the existing `reg`; construct `UploadHandler` with `Staging.Store`
  + the FILE notifier; register `POST .../upload/apply` and `.../upload/finish` behind
  `protect()`, `POST /api/oss/upload` unprotected; call `staging.Migrate` + start the
  `Sweep` ticker in server mode.
- main.go: construct staging Store under `FILE_ROOT`; thread the notifier.
**Testing:** build/vet/test green. neptune `server` mode: login loop → token; curl
apply → fullUploadUrl; curl multipart upload to it (no token); curl finish → md5
matches on-disk; the file then shows in `list_folder`. Default config → `:8089` closed.

---

## Sub-phase 4c — Mutation endpoints (delete / move / copy)

### Task 6: `delete_folder_v3` soft-delete to `.recycle/`
**Verifies:** spc-phase-4.AC4.1, AC4.4 (delete)
- Read the `delete_folder_v3` DTO verbatim; move target → `.recycle/<ts>/<relpath>`,
  invalidate registry entry, fire FILE-SYN. Assert `.recycle/` excluded from listing.

### Task 7: `move_v3` + `copy_v3`
**Verifies:** spc-phase-4.AC4.2, AC4.3, AC4.4 (move/copy)
- move: SafeResolve both ends, atomic rename, registry follows (id stable).
- copy: disk copy, fresh id, both list. Both JWT-protected + FILE-SYN.

---

## Sub-phase 4d — Pipeline integration + catalog cutover

### Task 8: Kick the OCR processor on finished uploads
**Verifies:** spc-phase-4.AC5.1
- In `Finish` (or a hook off it), if the promoted target is `.note`/`.pdf` under the
  OCR-watched tree, `processor.Enqueue(path)` best-effort. Thread the processor Store
  into `UploadHandler` (interface seam; nil = no-op so tests stay light).
**Testing:** a fake enqueuer records the path for a `.note`; a `.txt`/non-watched path
is not enqueued; enqueue error is swallowed.

### Task 9: Catalog cutover (remove MariaDB write-through)
**Verifies:** spc-phase-4.AC5.2, AC5.3
- Delete `internal/processor/catalog.go` + `catalog_test.go`; remove
  `WorkerConfig.CatalogUpdater` + the `AfterInject()` call; worker updates
  `notedb.notes.md5`/`size` directly. main.go stops passing MariaDB into the processor
  (pool stays for tasksync). `grep` clean; OCR behavior identical, no MariaDB during OCR.
**Testing:** existing processor tests still pass (minus the deleted catalog test);
vet/build clean; the grep guard is empty.

---

## Sub-phase 4e — Device acceptance (tier 2)

### Task 10: Device upload/delete/move/copy acceptance
**Verifies:** spc-phase-4.AC6.1–AC6.4
- Build image, redeploy on neptune, flip NPM to UB (`$port` 8089 + reload). On the
  device: create/modify a `.note` and sync → confirm it lands in `FILE_ROOT`, md5
  matches, shows in cloud browse, and (post-4d) is searchable in the web UI within
  ~30s. Delete → `.recycle/`, disappears. Move/copy → correct. Confirm Boox invisible
  and Phases 1–3 + web UI intact. tcpdump the `:8089` leg if any step misbehaves.
- Flip NPM back to real SPC; record the result in CLAUDE.md + a Phase 4 memory.

---

## Exit state
- Build/test green; device fully creates, downloads, deletes, moves, copies files
  against UB; OCR runs against UB-owned uploads with no MariaDB write-through.
- `internal/processor` no longer touches MariaDB; `internal/tasksync/supernote/`
  still uses it (Phase 5 deletes it).

## Files this phase touches
**Created:** `internal/spcserver/staging/{staging.go,staging_test.go}`,
`internal/spcserver/handlers/{upload.go,upload_test.go,mutation.go,mutation_test.go}`,
this plan.
**Modified:** `internal/spcserver/dto/file.go`, `internal/spcserver/server.go`,
`internal/spcserver/server_test.go`, `cmd/ultrabridge/main.go`,
`internal/spcserver/notify/notifier.go` (FILE-SYN variant if needed),
`internal/spcserver/CLAUDE.md`, `docs/spc-protocol.md`.
**Deleted:** `internal/processor/catalog.go`, `internal/processor/catalog_test.go`.
**Not touched:** `internal/sync/`, `internal/tasksync/`, `internal/db/` (Phase 5).

## Compact/clear checkpoint
Merge Phase 4. **DNS-flip moment** (out of scope here — operational). Real SPC stays
alive one verification week as escape hatch. `/clear` before Phase 5 (recycle CRUD,
file search, SPC-client code removal).
