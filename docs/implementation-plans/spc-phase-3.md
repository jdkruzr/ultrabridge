# UB-as-SPC Phase 3 — Files: download (cloud → device) — Implementation Plan

Last verified: 2026-05-23

Phase 3 of the UB-as-SPC refactor (design plan: `docs/design-plans/2026-05-15-ub-as-spc-refactor.md`, Phase 3 section). Phase 1 (auth + tasks + Engine.IO) and Phase 2 (file listing + capacity, read path) are complete and hardware-validated. This phase completes the **read path**: the device can now **download** the files it can already browse, via SPC's presigned-URL OSS protocol — looking exactly like real SPC to the device.

## Entry state

- Phase 2 exit state holds: `list_folder`/`query_v3`/`by/path_v3`/capacity work on hardware; the device browses its native tree read-only; the `fileids` registry (path↔id + lazy MD5 cache) and `mapping.SafeResolve`/`mapping.EntryFor`/`capacity.Meter` exist and are tested.
- The device's only remaining failure is `POST /api/file/3/files/download_v3` → **404** (the "Private Cloud Sync Failed" it shows after browsing). That 404 is exactly what this phase closes.
- No OSS endpoints implemented. `UB_SPC_MODE=client` (default) remains regression-safe — no listener, no behavior change.

## One design decision locked before this plan (2026-05-23)

**The OSS signing secret is auto-generated, not the hardcoded SPC constant.** UB both *issues* and *verifies* these URLs; the device treats a signed URL as **opaque** (it never computes or verifies a signature itself — confirmed in `O_OssLocalController`: signing happens only in `generateDownloadUrl`, verification only in `downloadFile`; the device just `GET`s the `url` string it was handed). The real SPC `SECRET_KEY = "K+5xFzxbnB1iSZWqmu3Etw=="` is a single server-global constant (`SignVerifier.java:13`), **not** per-device or per-user — per-request variance is carried by path/timestamp/nonce, not the key. So UB's runtime secret can be anything the device can't tell the difference. We therefore:
- Generate a random `UB_SPC_OSS_SECRET` on first boot, persist it in settings (so reissued URLs stay valid across restart), and use it for live signing/verifying — better hygiene than shipping a published constant.
- **Also** pin a unit test that hardcodes §6's `K+5xFzxbnB1iSZWqmu3Etw==` plus the §6 worked-example inputs and asserts the exact `sha256_hex` output — proving the *algorithm* is byte-for-byte correct against real SPC, independent of the runtime secret.

Two read-the-`.java`-don't-guess unknowns are flagged inline (resolve during the task, like Phase 2's `synType`):
- **U1 — bad-signature error shape.** `O_OssLocalController.downloadFile` throws `FileDownloadException("Signature verification failed.")`. The exact HTTP status + envelope the device tolerates must be read from the exception handler (`@ControllerAdvice`/`@ExceptionHandler` for `FileDownloadException`) before finalizing Task 5; default placeholder is HTTP 403 + flat error envelope.
- **U2 — `/api/oss/download` auth.** Confirm the device sends *no* `x-access-token` on the raw `GET` (signature is the only auth). The plan registers it **outside** `auth.Middleware`; verify against the 0b tap capture / the controller (it extracts no `userId`).

## Validation approach (all sub-phases)

- **Tier 1 (every task):** `go test`, `go build`, `go vet` at repo root (`-C /home/jtd/ultrabridge`). TDD: test first.
- **Tier 2 (3d only):** real device (Supernote Nomad `SN078C10034074`) flipped to UB via NPM (`hydrae 192.168.9.30`, `docker exec sysop-app-1`, `13.conf` `set $port` 19072→8089 + `nginx -s reload`). Roll back by flipping `$port` back to 19072.
- Inline execution, task by task, commit per task, check in with the user. **No subagents.**
- DTO/VO field names are verbatim from `/home/sysop/spc-rev/cfr-decrypted/`; cite `<FQN.java>:<line>` in comments. When a wire detail is uncertain, **read the `.java` (server) or jadx-decompiled device app first — do not guess.**

## Wire facts established from the decompiled source (read once, here)

From `com/ratta/controller/{F_FileLocalController,O_OssLocalController}.java`, `com/ratta/file/{dto,vo}/`, `com/ratta/util/SignVerifier.java`, and `docs/spc-protocol.md` §6:

- **Two-round-trip download.** (1) `POST /api/file/3/files/download_v3` returns a presigned-URL VO; (2) the device `GET`s that URL to stream bytes. `generate/download/url` is the same minting primitive exposed directly (not seen in 0b, wired for completeness).
- **`download_v3`** request `FileDownloadLocalDTO{equipmentNo (String), id (Long, @NotNull)}` → `FileDownloadLocalVO extends BaseVO {equipmentNo, id (String), url, name, path_display, content_hash, size (Long), is_downloadable (bool)}`. Missing file → `errorCode = FileErrorCodeEnum.E0321`, `success:false` (`FileLocalServiceImpl.getDownloadUrl`). Note snake_case wire keys `path_display`/`content_hash`/`is_downloadable` (no `@JsonProperty`), same as `EntriesVO`.
- **`generate/download/url`** is **form/query params** (`@RequestParam filePath, fileName, pathId, ip`), *not* a JSON body → `FileDownloadApplyVO implements Serializable {url, signature, timestamp (Long), nonce, pathId}` (note: **not** a `BaseVO` — no `success` field).
- **`GET /api/oss/download`** query params `path` (base64url-encoded), `signature`, `timestamp` (Long), `nonce`, `pathId`. Validates signature + 24 h window, decrypts path, streams the file. **Supports `Range`** (`downloadFileRange` when the `Range` header is present) → resumable / 206.
- **Signing (`SignVerifier`, §6):** `encryptedPath = base64url-no-pad(UTF-8 path)`; `download_signature = sha256_hex(encryptedPath + str(timestamp_ms) + nonce + "K+5xFzxbnB1iSZWqmu3Etw==")`. Download TTL 86_400_000 ms (24 h). `nonce = UUID.randomUUID().toString()`. **No nonce-replay tracking** — the timestamp window is the only freshness guard. Despite the file name it is plain SHA-256 with the secret concatenated, **not** HMAC.
- **URL template:** `{scheme}://{host}/api/oss/download?path={enc}&signature={sig}&timestamp={ts}&nonce={uuid}&pathId={pathId}`. `{scheme}://{host}` is built server-side from the `x-forwarded-proto` + `host` headers (`requestUrl` in the controller) — UB is behind NPM, which sets these.
- **`pathId`** in real SPC is `String.valueOf(userId)`; the `GET` handler only uses it for logging + a convert-dir special case. For UB it is **not load-bearing** — we set it to the file's registry id (string) and ignore it on the way back in.
- **Path on the wire.** Real SPC encrypts its absolute server path (`/home/supernote/data/...`). UB controls both ends, so we encrypt the **`path_display`** (root-relative, e.g. `/Note/foo.note`) and `SafeResolve(root, decrypted)` on the `GET` side → abs path. The signature covers the encrypted path (tamper-proof); `SafeResolve` is defense-in-depth against traversal and tolerates the device's double-slash quirk.
- **Boox stays invisible.** Only paths under `UB_SPC_FILE_ROOT` have a registry id and resolve under `SafeResolve`; nothing else is reachable. No new exposure.

---

## Acceptance Criteria Coverage

### spc-phase-3.AC1: OSS signing primitive (`internal/spcserver/oss`)
- **spc-phase-3.AC1.1 Path codec:** `EncryptPath(p)` is base64url-no-pad of the UTF-8 bytes; `DecryptPath(EncryptPath(p)) == p`; a path containing a double slash (`Note/Personal//IMG_x.jpg`) round-trips byte-for-byte (no normalization in the codec).
- **spc-phase-3.AC1.2 Golden-master:** with the hardcoded `K+5xFzxbnB1iSZWqmu3Etw==` secret and §6's worked-example inputs (path `/home/supernote/data/test/foo.note`, ts `1715765576179`, nonce `b93fa5c9-189d-4c2a-a68e-861ac9b204be`), `DownloadSignature` produces the exact `sha256_hex` of §6's pinned data string (computed once, hardcoded as the expected constant). The upload variant's data string (fileSize term) is also pinned for Phase 4 reuse.
- **spc-phase-3.AC1.3 Validate:** a signature minted now validates; a `timestamp` older than 24 h is rejected; a tampered `path`, `signature`, `nonce`, or `timestamp` is rejected; comparison is constant-time (`subtle.ConstantTimeCompare`/`hmac.Equal`). An injectable clock drives the expiry test.

### spc-phase-3.AC2: URL-minting handlers (`download_v3`, `generate/download/url`)
- **spc-phase-3.AC2.1 download_v3 success:** a valid `id` resolves (`Reg.PathFor`) to a `FileDownloadLocalVO` with `success:true`, a non-empty `url`, `id` (string), `name`, `path_display` (leading-`/` slash path), `content_hash` (lowercase MD5 via `Reg.MD5For`), `size` (file bytes), `is_downloadable:true`, flat envelope.
- **spc-phase-3.AC2.2 download_v3 missing:** an unknown id, or an id whose path no longer exists on disk, returns `success:false` with `errorCode == E0321` (verbatim from `FileErrorCodeEnum`), never a 500.
- **spc-phase-3.AC2.3 URL well-formed + valid:** the minted `url` is `{proto}://{host}/api/oss/download?path=…&signature=…&timestamp=…&nonce=…&pathId=…` using the request's forwarded proto/host; its `signature` validates under the runtime secret; its `path` decrypts to the file's `path_display`.
- **spc-phase-3.AC2.4 generate/download/url:** the form-param endpoint returns a `FileDownloadApplyVO{url, signature, timestamp, nonce, pathId}` (no `success` field — not a `BaseVO`) with a signature that validates.

### spc-phase-3.AC3: Byte streaming (`GET /api/oss/download`)
- **spc-phase-3.AC3.1 Round-trip:** a `GET` with a freshly minted valid URL streams the exact file bytes; the MD5 of the response body equals the MD5 of the file on disk.
- **spc-phase-3.AC3.2 Range/resumable:** a `GET` with a `Range: bytes=N-M` header returns `206 Partial Content` with the correct byte slice and `Content-Range` (delegated to `http.ServeContent`).
- **spc-phase-3.AC3.3 Reject bad URL:** an expired (`>24h`) or tampered URL is rejected with the device-expected error (U1 — read the exception handler; placeholder HTTP 403 + flat error envelope) and **leaks no bytes**.
- **spc-phase-3.AC3.4 Traversal guard:** the decrypted `path` is `SafeResolve`d under `UB_SPC_FILE_ROOT`; a path escaping the root (`../…`) is refused even with an otherwise-valid signature; a double-slash path resolves to the intended file.
- **spc-phase-3.AC3.5 Auth boundary:** `GET /api/oss/download` is reachable **without** an `x-access-token` (signature is its auth — U2); `download_v3` and `generate/download/url` remain behind `auth.Middleware`.

### spc-phase-3.AC4: Secret, wiring, regression
- **spc-phase-3.AC4.1 Secret lifecycle:** `UB_SPC_OSS_SECRET` is auto-generated (≥32 random bytes, hex) on first boot when unset, persisted to settings, and stable across restart; an explicitly configured value (env/DB) is honored unchanged.
- **spc-phase-3.AC4.2 Wiring + regression:** the three routes are registered (two protected, the `GET` unprotected); `UB_SPC_MODE=client` still binds no listener and changes nothing; the OCR pipeline, web UI, and Phase 1/2 endpoints are untouched.

### spc-phase-3.AC5: Device acceptance (tier 2)
- **spc-phase-3.AC5.1 Download works:** the device pulls a known `.note`/image from UB; the file opens on-device and the round-trip MD5 matches the file on disk; the post-browse "Private Cloud Sync Failed" no longer fires at the download step.
- **spc-phase-3.AC5.2 Larger/resumable:** a multi-MB file downloads completely (exercises Range/206).
- **spc-phase-3.AC5.3 Isolation intact:** Boox still invisible; task sync (Phase 1) and file browsing (Phase 2) and the web UI all still work.

---

## Sub-phase 3a — Signing primitive + secret config

### Task 1: `UB_SPC_OSS_SECRET` config key + auto-generate-on-first-boot

**Verifies:** spc-phase-3.AC4.1

**Files:** `internal/appconfig/keys.go`, `config.go`, `appconfig_test.go`

**Implementation:** same six-spot pattern as the Phase 2 keys:
- const `KeySPCOssSecret = "spc_oss_secret"`; `envVarForKey` → `UB_SPC_OSS_SECRET`; `defaultValues` → `""`; `restartRequired` → true; `Config` field `SPCOssSecret string`; wire `loadConfigFromDB` + `configToMap`.
- New helper `EnsureSPCOssSecret(ctx, db) (string, error)`: `GetSetting(KeySPCOssSecret)`; if empty, generate 32 bytes via `crypto/rand`, hex-encode, `SetSetting`, return it; else return the existing value. (Lives in appconfig so it is unit-testable without main.go; called from the server-mode block in Task 6.)

**Testing:** default config has `SPCOssSecret == ""`; `EnsureSPCOssSecret` on a fresh DB returns a 64-char hex string and persists it; a second call returns the *same* value (stable across "restart"); a pre-seeded value is returned untouched.

**Commit:** `feat(spcserver): UB_SPC_OSS_SECRET config key + first-boot generation`

### Task 2: `oss` package — path codec + sign/verify primitive

**Verifies:** spc-phase-3.AC1.1, spc-phase-3.AC1.2, spc-phase-3.AC1.3

**Files:** `internal/spcserver/oss/sign.go`, `sign_test.go`

**Implementation:** leaf package (imports nothing internal), so handlers + server can both use it.
- `EncryptPath(p string) string` = `base64.RawURLEncoding.EncodeToString([]byte(p))`; `DecryptPath(enc string) (string, error)` = inverse (`RawURLEncoding`, no padding).
- `type Signer struct { Secret string; Now func() time.Time }` (injectable clock; nil `Now` → `time.Now`).
- `DownloadSignature(encPath string, tsMillis int64, nonce string) string` = `sha256_hex(encPath + strconv.FormatInt(ts,10) + nonce + s.Secret)`.
- `UploadSignature(encPath string, tsMillis int64, nonce string, fileSize int64) string` (Phase 4 reuse) = same but with `+ strconv.FormatInt(fileSize,10)` before the secret (§6: caller always passes `0`).
- `ValidateDownload(sig string, tsMillis int64, nonce, encPath string) bool` = `now-ts <= 24h` **and** `subtle.ConstantTimeCompare(sig, DownloadSignature(...)) == 1`. (`ValidateUpload` analog, 30 min window — declare for Phase 4, test now.)
- `const realSPCSecret = "K+5xFzxbnB1iSZWqmu3Etw=="` lives in the **test** file only (it is a golden-master fixture, not a runtime default).

**Testing:** AC1.1 round-trip incl. double-slash path; AC1.2 golden-master — build a `Signer{Secret: realSPCSecret}`, feed §6's worked-example inputs, assert the exact `sha256_hex` (compute the digest of §6's pinned data string once, hardcode it; upload-variant data string pinned too); AC1.3 fresh-validates, 24h-expiry (injected clock), tamper rejection on each field, constant-time compare.

**Commit:** `feat(spcserver): OSS path codec + SHA-256 sign/verify primitive`

---

## Sub-phase 3b — URL-minting handlers

### Task 3: Download DTOs/VOs

**Verifies:** types for AC2 (no test of its own)

**Files:** `internal/spcserver/dto/file.go`

**Implementation:** add, with verbatim field names + JSON tags and `<FQN.java>:<line>` citations:
- `FileDownloadLocalDTO{EquipmentNo string, ID int64 json:"id"}` (`FileDownloadLocalDTO.java`; device sends `id` as a JSON number → `int64`/`*int64`).
- `FileDownloadLocalVO{ envelope.BaseVO; EquipmentNo, ID (string), URL, Name, PathDisplay json:"path_display", ContentHash json:"content_hash", Size *int64, IsDownloadable bool json:"is_downloadable" }` (`FileDownloadLocalVO.java`).
- `FileDownloadApplyVO{ URL, Signature, Timestamp int64, Nonce, PathID json:"pathId" }` — **no** `BaseVO` (implements `Serializable` only; `FileDownloadApplyVO.java`).

**Commit:** `feat(spcserver): download DTOs/VOs`

### Task 4: `download_v3` + `generate/download/url` handlers (URL minting)

**Verifies:** spc-phase-3.AC2.1, AC2.2, AC2.3, AC2.4

**Files:** `internal/spcserver/handlers/download.go`, `download_test.go`

**Implementation:** `DownloadHandler{ Root string; Reg *fileids.Registry; Signer *oss.Signer; Logger *slog.Logger }`. A small `requestBaseURL(r)` helper = `{x-forwarded-proto||"https"}://{Host}`.
- `DownloadV3`: decode `FileDownloadLocalDTO`; `Reg.PathFor(id)`; if not found or `os.Lstat` fails → `FileDownloadLocalVO` with `success:false`, `errorCode=E0321`. Else build `entry := mapping.EntryFor(...)` (reuse for `path_display`/`content_hash`/`size`/`name`) → `encPath := oss.EncryptPath(entry.PathDisplay)`, `ts := now.UnixMilli()`, `nonce := uuid`, `sig := Signer.DownloadSignature(encPath, ts, nonce)`, assemble the `/api/oss/download?…` URL on `requestBaseURL(r)`, `pathId = id`. Populate the VO (`is_downloadable:true`). Flat envelope.
- `GenerateDownloadURL`: read `filePath`/`fileName`/`pathId` form params; `encPath := oss.EncryptPath(path.Join(filePath, fileName))`; sign; return `FileDownloadApplyVO`.
- `nonce` generation: `crypto/rand` UUIDv4 string (a tiny local helper or reuse an existing one — check `socketio`/`login` for a uuid helper before adding).

**Testing:** httptest with a temp root + `fileids.Migrate`'d DB. AC2.1 known id → all VO fields correct (MD5 golden against `md5sum`); AC2.2 unknown id and a registered-but-deleted path → `success:false`,`E0321`; AC2.3 parse the returned `url`, assert host/query shape and that `Signer.ValidateDownload` accepts its components and `DecryptPath(path)==entry.PathDisplay`; AC2.4 `generate/download/url` form post → valid `FileDownloadApplyVO`.

**Commit:** `feat(spcserver): download_v3 + generate/download/url URL-minting handlers`

---

## Sub-phase 3c — Byte streaming + wiring

### Task 5: `GET /api/oss/download` byte-streaming handler

**Verifies:** spc-phase-3.AC3.1, AC3.2, AC3.3, AC3.4

**Files:** `internal/spcserver/handlers/download.go`, `download_test.go`

**Implementation:** `DownloadStream(w, r)`:
- Read query params `path`,`signature`,`timestamp`,`nonce` (`pathId` ignored beyond logging).
- `ts, err := strconv.ParseInt(timestamp)`; if `!Signer.ValidateDownload(sig, ts, nonce, encPath)` → **U1** error (read the `FileDownloadException` handler; placeholder: `envelope.WriteError` HTTP 403, no body bytes). Return before any file access.
- `decoded, err := oss.DecryptPath(encPath)`; `abs, err := mapping.SafeResolve(Root, decoded)` (rejects traversal, tolerates double slash). `os.Open`; `os.Stat` for modtime/size; set `Content-Disposition: attachment; filename="…"` and `Content-Type: application/octet-stream`; `http.ServeContent(w, r, name, modTime, file)` — this gives Range/206/If-Modified-Since for free.

**Testing:** AC3.1 mint a URL via Task 4's handler, `GET` it, assert body MD5 == file MD5; AC3.2 `GET` with `Range: bytes=0-3` → 206 + first 4 bytes + `Content-Range`; AC3.3 expired ts (injected clock) and flipped signature → 403, empty body, file never opened; AC3.4 a hand-crafted (validly signed) URL whose path is `../../etc/passwd` → refused; a double-slash path resolves to the real file.

**Commit:** `feat(spcserver): GET /api/oss/download byte-streaming handler (Range-aware)`

### Task 6: Wire routes + main.go construction; no-device round-trip curl loop

**Verifies:** spc-phase-3.AC3.5, AC4.2, and AC2/AC3 end-to-end over HTTP

**Files:** `internal/spcserver/server.go`, `cmd/ultrabridge/main.go`, `internal/spcserver/CLAUDE.md`

**Implementation:**
- `spcserver.Config` gains `OssSecret string`. `server.go` builds `signer := &oss.Signer{Secret: s.cfg.OssSecret}` and `dh := &handlers.DownloadHandler{Root, Reg, Signer: signer, Logger}` (reuse the same `Reg` instance already constructed for `FileHandler`). Register:
  - `s.mux.Handle("POST /api/file/3/files/download_v3", protect(dh.DownloadV3))`
  - `s.mux.Handle("POST /api/oss/generate/download/url", protect(dh.GenerateDownloadURL))`
  - `s.mux.HandleFunc("GET /api/oss/download", dh.DownloadStream)` — **not** wrapped in `protect` (signature is the auth; U2).
- `main.go` server-mode block: `secret, _ := appconfig.EnsureSPCOssSecret(ctx, noteDB)` (best-effort; log on error), pass `OssSecret: secret` into `spcserver.Config`. Client mode unchanged.
- Update `internal/spcserver/CLAUDE.md`: status → Phase 3; layout `oss/`, `handlers/download.go`; invariants (opaque-URL/auto-gen secret, GET-not-JWT, encrypt path_display + SafeResolve on return, Range support).

**Testing:** `go build`/`vet`/`test` green. Manual on neptune: `server` mode, `UB_SPC_FILE_ROOT=<temp tree with a known file>`; mint a token via the Phase 1 login loop; `curl` `download_v3` (a real id from `list_folder`) → grab `url`; `curl "$url" -o out` (no token) → `md5sum out` == on-disk md5; `curl -H 'Range: bytes=0-9' "$url"` → 206; tamper a char in `signature` → 403; default config (no `UB_SPC_MODE`) → `:8089` closed (AC4.2 regression).

**Commit:** `feat(spcserver): wire download routes + first-boot OSS secret`

---

## Sub-phase 3d — Device acceptance (tier 2)

### Task 7: Device download acceptance

**Verifies:** spc-phase-3.AC5.1, AC5.2, AC5.3

**Files:** none (docs-only if no fix needed)

**Steps:**
1. Redeploy on neptune (image rebuild; `UB_SPC_FILE_ROOT` already set in `docker-compose.yml`). No new env needed — the OSS secret self-generates on first boot.
2. Flip NPM (`hydrae`, `13.conf` `$port` 19072→8089 + reload).
3. On the device: open the cloud-files UI, browse into a folder (Phase 2), and **download** a known `.note` and an image (AC5.1) — confirm each opens on-device. Download a multi-MB file to exercise Range (AC5.2). Confirm the post-browse "Private Cloud Sync Failed" no longer fires at the download step.
4. Verify round-trip integrity: compare the device-side file (or re-list `content_hash`) against the on-disk MD5.
5. Confirm Boox still invisible; task sync + web UI + browsing all still work (AC5.3).
6. Watch logs / the tap on any 404/4xx/5xx; fix forward (most likely U1 error shape, the `path_display`-vs-absolute encoding, or a VO field the device rejects — read the relevant `.java`/device app rather than guessing).
7. Flip NPM back to 19072.

**Acceptance:** device downloads files from UB; round-trip MD5 matches; Range works; Boox invisible; Phases 1–2 intact.

**Commit:** `docs(spcserver): record Phase 3 device-acceptance result`

---

## Exit state

- Build/test/vet green; `client` mode still inert.
- Device can browse **and** download files from UB; the read path is complete. Upload still device-blocked (no endpoints — Phase 4).
- The OSS HMAC/path primitive (`internal/spcserver/oss`) is proven and reusable by Phase 4 (upload signing + the `spc_uploads`/staging flow).

## Files this phase touches

**Created:** `internal/spcserver/oss/{sign.go,sign_test.go}`, `internal/spcserver/handlers/download.go` (+test), `docs/implementation-plans/spc-phase-3.md`.
**Modified:** `internal/appconfig/{keys.go,config.go}` (+test, `EnsureSPCOssSecret`), `internal/spcserver/dto/file.go` (download DTOs/VOs), `internal/spcserver/server.go` (routes + `OssSecret`), `cmd/ultrabridge/main.go` (secret bootstrap + wiring), `internal/spcserver/CLAUDE.md`.
**Not touched:** `tasksync`/`sync`/`taskdb`/`notestore`/`notedb` packages; the OCR pipeline; the web UI. Download reads the filesystem under `UB_SPC_FILE_ROOT` directly, same as Phase 2.

## Compact/clear checkpoint

Merge Phase 3. `/clear` before Phase 4 (upload — `/upload/apply` + `/api/oss/upload` + `/upload/finish`; the `oss` signer + `fileids` registry built here are reused; Phase 4 adds the `spc_uploads` staging table and fires the Engine.IO `FILE-SYN`/STARTSYNC nudge + kicks `internal/processor` to OCR/index the uploaded file).
