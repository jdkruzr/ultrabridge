# SPC Protocol Reference

Working specification for re-implementing Supernote Private Cloud (SPC) endpoints in UltraBridge so the Supernote device cannot tell the difference. Built from CFR-decompiled output of `supernote-service.jar` v2.1.4.RELEASE. All citations are `<FQN.java>:<line>` against `/home/sysop/spc-rev/cfr-decrypted/`.

**Status:** Phase 0 complete (2026-05-22). 0a (CFR + endpoint enumeration), 0d (OSS HMAC, empirically validated), 0b (device wire observations), 0c (JWT acceptance: PASS) all done. Every risk R1–R6 closed. See §11 for the device-traffic summary.

---

## 1. Wire envelope

All REST responses extend `BaseVO` (`com/ratta/vo/BaseVO.java`):

```json
{
  "success": true|false,
  "errorCode": "<string, e.g. E0330 or empty on success>",
  "errorMsg": "<human message>",
  ...payload fields alongside these three at the top level...
}
```

**Critical:** VOs **extend** `BaseVO` rather than nesting payload under a `data` key. `LoginVO`'s `token` field sits directly next to `success`/`errorCode`/`errorMsg`, not under `data.token`.

## 2. Auth

- Header: `x-access-token` (constant `Constant.AUTHORIZE_TOKEN` = `"x-access-token"`, `Constant.java:47`).
- Algorithm: HS256 via `com.auth0:jwt` (`JwtTokenUserUtil.java:174` — `Algorithm.HMAC256(secret)`).
- **Signing secret:** the long string in `Constant.SECRET` (`Constant.java:46`):
  ```
  suernotea1hK52bgkf9N7PQ5E3KDqKeCIT719a6kh04eSTSBLv7e9tPtw2L8S6pEDMy7lAIv2CYjg5Ncy7ep5zDS7hH9CDAZnLieo66g7F8iZmClK9a1xEEPewXLhkM4KTKI7pz2Lkl7Cds4MpClNvNCVHPbfWKNyiFSGUztbnmqDWgNAinPBNamwDUQpT8RwCO1wc9vYTTQsmXm8ByioHC3QkRMZtHZnIWWCkIWECPzSJGOowNliAavzVCMsKadYnsH322n
  ```
  Stored as `JwtTokenUserUtil.secret` (`JwtTokenUserUtil.java:57`); used as the HMAC key bytes (UTF-8). **Not** the 32-char `Constant.JWT_SECRET = "7786df7fc3a34e26a61c034d5ec8245d"` — that constant exists but is not the JWT signing secret.
- TTLs (`Constant.java:10-12`):
  - `JWT_TTL` = 3600000 ms (1h, generic)
  - `JWT_TTL_8` = 28800000 ms (8h, used elsewhere)
  - `JWT_DAY_TTL` = 2592000000 ms (30 days)
  - `JWT_REFRESH_INTERVAL` = 3300000 ms (55min refresh threshold)
- **Two token flavors** (`JwtTokenUserUtil.createToken`, `JwtTokenUserUtil.java:97-110`):
  - **equipmentNo branch** (device/terminal login): claims `{userId, createTime, equipmentNo, key}`, signed **without** `.withExpiresAt(...)`. Effective TTL comes from Redis state keyed by the `key` claim.
  - **no-equipmentNo branch** (web flows): claims `{userId, createTime, key, exp}` — `exp` set to a far-future date.
- `key` claim format: `{userId}_{createTime_seconds}_{baseTokenCreateTime_ms}` and, in the equipmentNo branch, `+ "_" + equipmentNo`.
- **Real captured token (0b socket handshake)** decodes to:
  ```json
  {"createTime":1775346068, "exp":2086386068, "userId":"1184673925533868032",
   "key":"1184673925533868032_1775346068_1774555792279"}
  ```
  i.e. the device's live socket used the **exp-bearing** flavor (`exp` = 2036, ~60 yr out), no `equipmentNo`.
- Login response VO field name for the JWT is **`token`** (`LoginVO.token`), not `jwtToken` or `accessToken`. `LoginVO` fields: `token`, `counts`, `userName`, `avatarsUrl`, `lastUpdateTime`, `isBind`, `isBindEquipment`, `soldOutCount` (all alongside the three BaseVO fields).

**JWT acceptance — CONFIRMED (0c, 2026-05-22).** An unmodified device (Supernote Nomad, `SN078C10034074`) accepted a token we minted with `Constant.SECRET` and the claim shape above, then made authenticated calls carrying it in `x-access-token` and opened its Engine.IO socket. R2 closed. The device performs **no client-side JWT validation** — it stores and echoes whatever `token` the login response returns.

### Login handshake sequence (observed 0c)

The device runs this on a fresh login (after a logout, which is `POST /api/terminal/equipment/unlink` `{equipmentNo}`):

1. `POST /api/official/user/check/exists/server` — `{email, version}`
2. `POST /api/official/user/query/random/code` — `{account, version}` → returns a random code (challenge)
3. `POST /api/official/user/account/login/equipment` — `{account, password:<64-hex>, equipmentNo, equipment:3, loginMethod:"2", timestamp, version}`. `password` is the `webPassword` of the recipe in §2.1.
4. `POST /api/terminal/user/bindEquipment` — `{account, equipmentNo, flag:"1", label:[…folder manifest…], name:"Supernote Nomad", totalCapacity:"25485312", version}`. The `label` manifest observed: `["DOCUMENT/Document","NOTE/Note","NOTE/MyStyle","EXPORT","SCREENSHOT","INBOX"]`.
5. First authenticated call (`POST /api/user/query`) carries the new token in `x-access-token`.
6. Engine.IO socket opens; `ratta_ping` every 5 s.

`equipment=3` = terminal, `loginMethod="2"` = email.

### 2.1 Password verification recipe (RECOVERED 2026-05-22 via javap)

`loginEquipment` / `login` / `isPassword` resist **both** CFR and JADX — they carry control-flow obfuscation that trips decompiler block analysis ("Found unreachable blocks"). **Bytecode disassembly reads straight through it:** `javap -p -c -classpath /home/sysop/spc-rev/decrypted-classes com.ratta.user.info.LoginUtil`. The same applies to `SHA256Util` / `MD5StrUtil`. Use `javap` for any method the decompilers refuse.

Recovered from `LoginUtil.isPassword(servicePassword, random, webPassword, userId, key)`:

```
computed = servicePassword                              # stored DB credential
if random (the method arg) is NOT empty:                # web-login path (login())
    computed = MD5_hex(servicePassword + random)
else:                                                   # terminal/device path (loginEquipment(), random arg = null)
    randomCode = redis.get(key)                         # one-time code issued by /query/random/code
    computed = SHA256_hex(servicePassword + randomCode)
    redis.delete(key)                                   # one-time use
valid = computed.trim().equals(webPassword.trim())
```

- The **device/terminal flow takes the else branch**: `webPassword == SHA256_hex(servicePassword + randomCode)`. `webPassword` is `LoginDTO.password`.
- `SHA256_hex` (`SHA256Util.getSHA256Str` → `byte2Hex`) and `MD5_hex` (`MD5StrUtil`) are **standard lowercase zero-padded hex** of the digest over UTF-8 bytes. `byte2Hex` explicitly pads single-nibble bytes (`if (hex.length()==1) append("0")`) — no non-standard-hex quirk to replicate.
- On success the server zeroes the redis password-error counter; on failure it increments it and locks the account after a configurable count (`UserParamEnum.P002`/`P003`). UB can ignore lockout (single-user).
- **`servicePassword = MD5_hex(rawPassword)` — RESOLVED 2026-05-22.** No capture needed: UB's existing SPC *client* (`internal/tasksync/supernote/client.go:61-66`), which authenticates against real SPC in production, computes the login password as `SHA256_hex(MD5_hex(rawPassword) + randomCode)` (comment there: "SPC stores MD5(password)"). So the stored credential is `MD5_hex(rawPassword)` and the full device-side recipe is `webPassword = SHA256_hex( MD5_hex(rawPassword) + randomCode )`. UB-as-SPC stores the raw password and computes `servicePassword = MD5_hex(raw)` at validation time. (`MD5_hex`/`SHA256_hex` are both standard lowercase zero-padded hex.)

## 3. Engine.IO

- Protocol version: **Engine.IO v3** (confirmed live: handshake query `EIO=3&transport=websocket`).
- Server library: `corundumstudio/netty-socketio` 2.0.3.
- Ping cadence: `socket.pinginterval = 5000` ms, `socket.pingtimeout = 25000` ms.
- **`permessage-deflate`: NOT required (corrected 2026-05-23).** UB serves the socket uncompressed (no extension negotiated) and the device holds a stable connection fine. The 0b "negotiated/required" inference was wrong.
- **Heartbeat is CLIENT-driven (corrected 2026-05-23).** The device (standard `io.socket` engine.io-client, EIO3) sends the native ping `2` ~every `pingInterval`; the server replies pong `3`. The server does **not** send pings. The device does **NOT** send `ratta_ping` (the 0b note claiming both was wrong — `ratta:0` over many minutes on hardware).

> **§3 corrections — Phase 1 hardware validation (2026-05-23).** These supersede the 0b/0c inferences below where they conflict. Found by decompiling the device apps (the answers are client-side, not in captures). See memory `project_spc_socketio_breakthrough`.
> - **Server sends Socket.IO CONNECT (`40`) proactively.** The device connects to the default namespace `/`; its `io.socket.client.Socket.onopen()` returns early for `/` WITHOUT sending a CONNECT — it waits to RECEIVE `40` to fire `EVENT_CONNECT`. So the server emits the EIO open `0{...}` **then `40`**. Omit it and the client never reaches CONNECTED and reconnect-loops (30/60/120/240s backoff) while the server side looks healthy.
> - **Task push uses the `to-do` event, not `ServerMessage`.** The device's `TaskService` binds `to-do`; its `onReceive` unconditionally triggers a task sync (then pulls via REST `task/all`). `ServerMessage` = file channel, `digest` = digests. The **event name** routes the push; the inner `msgType` is not what the task listener checks.
> - The `sign` query param is accepted-and-ignored by UB (token verification is the gate); the device does not require UB to validate it.

### Socket handshake + auth (observed 0b)

The WebSocket connects to:
```
/socket.io/?token=<JWT>&type=ANDROID<uuid>&random=<unix_ms>&sign=<sig>&EIO=3&transport=websocket
```
- `token` — the JWT (in the **query string**, not the `x-access-token` header, for the socket).
- `type` — `ANDROID` + a UUID.
- `random` — unix-ms timestamp / nonce.
- `sign` — `SignVerifierSocketIO.signData(data)` where `data = token + "_ANDROID_" + random` (`SignVerifierSocketIO.java:26,33`). That is: `HMAC-SHA256(data, "K+5xFzxbnB1iSZWqmu3Etw==")` → standard Base64 → strip all non-`[a-zA-Z0-9]`. **Same `K+5x…` secret as the OSS signer** (§6), distinct from `Constant.SECRET` (the JWT secret). Phase 1c can validate this or accept-and-ignore.

### Channels and ports (per `SocketIoConstant.java`)

| Logical namespace | Port (JVM bean) | Events accepted/emitted |
|---|---|---|
| `_fileSocket_` | 18072 (`socketIOServer` bean) | `ServerMessage` (→client), `ClientMessage` (→server), `Received` (ack), `ratta_ping` |
| `_digestSocket_` | 18072 (same bean as file) | `digest` |
| `_todoSocket_` | 18073 (`socketIOServerStask` bean) | `to-do` |

**Task channel is not used by the device (confirmed 0b/0c).** SPC's internal nginx routes all `/socket.io/*` to port 18072, and the device only ever opened **one** socket.io connection (to that single endpoint). It never attempted a separate task-channel URL. Tasks sync via REST polling (`POST /api/file/schedule/task/all`, seen 7× in one 0b session), not Engine.IO push. **Phase 1c should implement a single Engine.IO listener** (file+digest namespace); the task port (`socketIOServerStask`/18073) is not needed for device compatibility.

### Frame payload structure

Events arrive as Socket.IO `42["<eventName>", {...json payload...}]`. Inside the payload JSON, two fields drive dispatch:

- `msgType`: one of `FILE-SYN`, `TASK-SYN`, `DIGEST-SYN` (constants in `SocketIoConstant.java`).
- Op field (sibling of msgType): one of `STARTSYNC`, `MODIFYFILE`, `MODIFYFOLDER`, `DELETEFILE`, `DELETEFOLDER`, `ADDFOLDER`, `COPYFILE`, `COPYFOLDER`, `MOVEFILE`, `MOVEFOLDER`, `DOWNLOADFILE` (file ADD is `DOWNLOADFILE` here — see `SocketIoConstant.ADDFILE = "DOWNLOADFILE"`), `ADD_DIGEST`, `UPDATE_DIGEST`, `DELETE_DIGEST`, `QUERY`, `SORT`, `WAITING`.

## 4. ResubmitCheck

`@ResubmitCheck` annotation (`com/ratta/check/ResubmitCheck.java`) on a handler enables Redis-backed POST-body deduplication. Defaults: `interval=1`, `timeUnit=SECONDS` (1 second). Individual handlers can override; consult the method-level annotation in each handler's source.

For UB's reimplementation: in-memory `sync.Map` keyed on `userId + endpoint + sha256(body)` with the annotation's TTL.

## 5. Endpoints

Full enumeration with method + class+line citation lives in `/home/sysop/spc-rev/cfr-decrypted/_endpoints.txt` (130 endpoints total). Below are the device-relevant subsets organized by UB-as-SPC phase.

### Phase 1 — auth + tasks

- `POST /api/official/user/account/login/equipment` — terminal login (U_LoginController:93)
- `POST /api/official/user/account/login/new` — alt login (U_LoginController:84); 0b confirms which the device hits
- `POST /api/user/query/token` — token refresh (U_LoginController:148)
- `POST /api/user/logout` (U_LoginController:110)
- `GET  /api/file/query/server` — boot-time server-reachability check (F_FileLocalController:235)
- Schedule (task lists):
  - `POST /api/file/schedule/group/all` (F_ScheduleController:116)
  - `POST /api/file/schedule/group` — create (F_ScheduleController:77)
  - `PUT  /api/file/schedule/group` — update (not in 130-line output; verify)
  - `DELETE /api/file/schedule/group/{taskListId}` (F_ScheduleController:93)
  - `POST /api/file/schedule/group/clear` (F_ScheduleController:101)
  - `GET  /api/file/schedule/group/{taskListId}` (F_ScheduleController:108)
- Schedule (tasks):
  - `POST /api/file/schedule/task/all` — paginated 20/page, `nextPageTokens` plural in request, `nextPageToken` singular in response (F_ScheduleController:163)
  - `POST /api/file/schedule/task` — create
  - `PUT  /api/file/schedule/task` — update
  - `PUT  /api/file/schedule/task/list` — bulk update
  - `DELETE /api/file/schedule/task/{taskId}` (F_ScheduleController:148)
  - `GET  /api/file/schedule/task/{taskId}` (F_ScheduleController:155)
- Schedule (sort):
  - `POST /api/file/schedule/sort` (F_ScheduleController:171)
  - `PUT  /api/file/schedule/sort` (verify; not directly in the 130-line dump)
  - `DELETE /api/file/schedule/sort/{taskListId}` (F_ScheduleController:186)
- `POST /api/file/query/schedule/sort` (F_ScheduleController:194)
- Equipment binding stubs (0b confirms which device actually hits):
  - `POST /api/equipment/bind/status` (E_EquipmentController:101)
  - `POST /api/equipment/query/by/equipmentno` (E_EquipmentController:128)
  - `POST /api/terminal/user/activateEquipment` (E_EquipmentController:81)
  - `POST /api/terminal/user/bindEquipment` (E_EquipmentController:88)
  - `POST /api/terminal/equipment/unlink` (E_EquipmentController:95)

### Phase 2 — file listing + capacity (read path)

- `POST /api/file/2/files/synchronous/start` (F_FileLocalController:87)
- `POST /api/file/2/files/synchronous/end` (F_FileLocalController:94)
- `POST /api/file/2/files/list_folder` (F_FileLocalController:109)
- `POST /api/file/3/files/list_folder_v3` (F_FileLocalController:116)
- `POST /api/file/3/files/query_v3` — get by id (F_FileLocalController:163)
- `POST /api/file/3/files/query/by/path_v3` — get by path (F_FileLocalController:170)
- `POST /api/file/2/files/create_folder_v2` (F_FileLocalController:102)
- `POST /api/file/capacity/query` (F_FileLocalWebController:109)
- `POST /api/file/2/users/get_space_usage` (F_FileLocalController:190)
- `POST /api/file/2/files/query/deleteApi` — file-by-id (F_FileV2Controller:46)

### Phase 3 — download

- `POST /api/file/3/files/download_v3` — returns a presigned URL (F_FileLocalController:153)
- `POST /api/oss/generate/download/url` — same primitive, direct call (O_OssLocalController:152)
- `GET  /api/oss/download` — actual byte stream, query-string signature (O_OssLocalController:169)

### Phase 4 — upload + mutations

Upload (device picks one set; 0b confirms which):
- `POST /api/file/3/files/upload/apply` (F_FileLocalController:130)
- `POST /api/file/2/files/upload/finish` (F_FileLocalController:146)

Terminal upload variants (likely device-preferred):
- `POST /api/file/terminal/upload/apply` (F_TerminalFileUploadController:?)
- `POST /api/file/terminal/upload/finish` (F_TerminalFileUploadController:?)

OSS primitives:
- `POST /api/oss/generate/upload/url` (O_OssLocalController:77)
- `POST /api/oss/upload` — actual bytes, query-string signature (O_OssLocalController:97)
- `POST /api/oss/upload/part` — multipart chunked (O_OssLocalController:124)

Mutations:
- `POST /api/file/3/files/delete_folder_v3` (F_FileLocalController:123)
- `POST /api/file/3/files/move_v3` (F_FileLocalController:177)
- `POST /api/file/3/files/copy_v3` (F_FileLocalController:184)

### Phase 5 — polish + recycle + search

- `POST /api/file/recycle/list/query` (F_FileLocalWebController:158) — **web controller, not local** — device may not hit this; verify in 0b
- `POST /api/file/recycle/clear` (F_FileLocalWebController:164)
- `POST /api/file/recycle/delete` (F_FileLocalWebController:171)
- `POST /api/file/recycle/revert` (F_FileLocalWebController:178)
- `POST /api/file/list/search` (F_FileLocalWebController:151) — web controller
- `POST /api/file/label/list/search` (F_FileSearchController:44)
- Conditional (only if 0b shows device uses them):
  - `POST /api/file/note/to/pdf` (F_FileLocalController:197)
  - `POST /api/file/note/to/png` (F_FileLocalController:210)
  - `POST /api/file/pdfwithmark/to/pdf` (F_FileLocalController:223)

### Phase D — digests ("summary"), data-sync channel

Implemented (built 2026-05-25, pending hardware validation). `F_SummaryController`,
`@RequestMapping("/api/file")`, all JWT-protected. Storage: `internal/digeststore`
(canonical, faithful to `t_summary`/`t_summary_tag`); protocol adapter:
`internal/spcserver/handlers/summary.go`. `.mark` handwriting blobs reuse the OSS
signed-URL + staging path (§6, Phase 3/4).

- Items: `POST /add/summary` (→ id; idempotent on `uniqueIdentifier`), `PUT /update/summary`, `DELETE /delete/summary` (soft)
- Groups: `POST /add/summary/group`, `PUT /update/summary/group`, `DELETE /delete/summary/group` (cascades soft-delete to members), `POST /query/summary/group` (paginated)
- Queries: `POST /query/summary` (items, paginated), `POST /query/summary/hash` (lightweight id+md5 list the device diffs), `POST /query/summary/id` (by id list)
- Tags: `POST /add/summary/tag` (idempotent on name), `PUT /update/summary/tag`, `DELETE /delete/summary/tag`, `GET /query/summary/tag`
- `.mark` blobs: `POST /upload/apply/summary` (→ `innerName` + presigned `/api/oss/upload` URL; bytes land in `.staging/`, promoted to `<FileRoot>/.digests/<innerName>` at add/update time, verifying `handwriteMD5`), `POST /download/summary` (→ presigned `/api/oss/download` URL for the blob; `E0321` if the digest has no handwriting)
- Proactive `DIGEST-SYN` push over the `digest` socket event is **not** implemented (D3, capture-gated) — the device polls `query/summary/hash` every sync, so round-trip works without it.

### Stubs (canned success or 404)

- **Summary: `F_SummaryController` — IMPLEMENTED in Phase D** (see above). Before Phase D (or when no `DigestStore` is wired) the three observed query endpoints (`query/summary/{hash,group,id}`) fall back to empty-success stubs and the write endpoints 404 — the original sync-unblocking behavior. 0b observed the device hitting the three query endpoints during normal sync; they must never 404.
- Login challenge endpoints (device hits on fresh login, see §2): `POST /api/official/user/check/exists/server`, `POST /api/official/user/query/random/code` — must return well-formed responses, not 404.
- Equipment: `POST /api/equipment/bind/status` (polled ~4×/session by the device — must succeed), `POST /api/terminal/user/bindEquipment` (login flow — must succeed), `POST /api/terminal/equipment/unlink` (logout).
- Sharing: `F_ShareController` (1 endpoint) — not seen; 404 OK.
- Dictionary / Reference: `B_DictionaryController` (5), `B_ReferenceController` (4) — not seen; 404 OK.
- Email server: `U_EmailServerController` (4) — not seen; 404 OK.
- User registration / password / valid-code / sensitive ops: `U_UserRegisterController`, `U_PasswordController`, `U_ValidCodeController`, `U_SensitiveOperationController`, `U_FigureVaildCodeController` — not seen; 404 OK.
- Web file controller (humans only): `F_FileLocalWebController` (17) — not hit by device; 404 OK.

## 6. OSS HMAC (signing primitive for upload/download URLs)

Specified in `com/ratta/util/SignVerifier.java` (full decompilation; ~80 lines, all readable). **Note:** despite the file name, the actual upload/download URL signing is plain SHA-256, **not** HMAC — the secret is concatenated into the data and the result hex-encoded. The class also contains a separate static HMAC-SHA256 + Base64 method (`signData` / `verifySignature`) for a different code path; do not confuse them.

**EMPIRICALLY VALIDATED against live device traffic (0b, 2026-05-22).** Two real signed URLs the device sent through the tap proxy were reproduced byte-for-byte by the algorithm below:
- Upload: path `L05PVEUvTm90ZQ` (`/NOTE/Note`), ts `1779425962069`, nonce `b4c1c01d-…`, fileSize `0` → sig `61e646…e98a5` ✓
- Download: path `Tk9URS9Ob3RlL1BlcnNvbmFsLy9JTUdf…` (`NOTE/Note/Personal//IMG_…jpg`), ts `1779426063967`, nonce `b4b5306c-…` → sig `79ef6d…bccad` ✓

(Note the download path's **double slash** `Personal//IMG` — the device emits non-normalized paths; UB path handling must tolerate this.)

### Constants

- `SECRET_KEY = "K+5xFzxbnB1iSZWqmu3Etw=="` — used as the **literal string bytes**, not as a base64-decoded key (`SignVerifier.java:13`).
- Upload TTL: 1800000 ms (30 min) (`SignVerifier.java:55`).
- Download TTL: 86400000 ms (24 h) (`SignVerifier.java:70`).
- Path encoding: `Base64.URLEncoder.withoutPadding(path.getBytes(UTF-8))` (`O_OssLocalController.java:192`). Despite the method name `encryptPath`, it's just URL-safe Base64 without `=` padding.

### Algorithm

```
def upload_signature(encrypted_path, timestamp_ms, nonce_uuid, file_size):
    # file_size is passed as 0L (Long.valueOf(0L)) from the caller in O_OssLocalController:80
    # so str(file_size) is always "0" in practice; if null, empty string.
    data = encrypted_path + str(timestamp_ms) + nonce_uuid + str(file_size or "") + "K+5xFzxbnB1iSZWqmu3Etw=="
    return sha256_hex(data)

def download_signature(encrypted_path, timestamp_ms, nonce_uuid):
    data = encrypted_path + str(timestamp_ms) + nonce_uuid + "K+5xFzxbnB1iSZWqmu3Etw=="
    return sha256_hex(data)

def validate_upload(sig, ts, nonce, path, file_size):
    if now_ms - ts > 1800000: return False  # 30 min window
    return sig == upload_signature(path, ts, nonce, file_size)

def validate_download(sig, ts, nonce, path):
    if now_ms - ts > 86400000: return False  # 24 h window
    return sig == download_signature(path, ts, nonce)
```

### URL templates

```
Upload:    {scheme}://{host}/api/oss/upload?signature={sig}&timestamp={ts}&nonce={uuid}&path={base64url_path}
Download:  {scheme}://{host}/api/oss/download?path={base64url_path}&signature={sig}&timestamp={ts}&nonce={uuid}&pathId={pathId}
```

`nonce` is `UUID.randomUUID().toString()` (Java default, lowercase hex with hyphens). **No nonce-replay tracking exists** — the timestamp window is the only freshness guarantee.

### Worked example (for golden-master tests in Phase 3a)

Inputs:
- path = `/home/supernote/data/test/foo.note` → encrypted (base64url-no-pad of UTF-8 bytes) = `L2hvbWUvc3VwZXJub3RlL2RhdGEvdGVzdC9mb28ubm90ZQ`
- timestamp = `1715765576179`
- nonce = `b93fa5c9-189d-4c2a-a68e-861ac9b204be`
- fileSize = `0`

Upload signature data string:
```
L2hvbWUvc3VwZXJub3RlL2RhdGEvdGVzdC9mb28ubm90ZQ1715765576179b93fa5c9-189d-4c2a-a68e-861ac9b204be0K+5xFzxbnB1iSZWqmu3Etw==
```

Download signature data string:
```
L2hvbWUvc3VwZXJub3RlL2RhdGEvdGVzdC9mb28ubm90ZQ1715765576179b93fa5c9-189d-4c2a-a68e-861ac9b204beK+5xFzxbnB1iSZWqmu3Etw==
```

(Phase 3a unit test should reproduce these `sha256_hex` outputs and pin them as constants.)

## 7. Error codes

Full enums in `com/ratta/enums/`:

- `BaseErrorCodeEnum` — common errors
- `FileErrorCodeEnum` — file/sync errors (incl. **`E0330 = "NextSyncToken timeout"`**, `FileErrorCodeEnum.java:38`)
- `OssErrorCodeEnum` — OSS upload/download errors (e.g. `E1305` returned on chunked-upload failure, `O_OssLocalController.java:131`)
- `EquipmentErrorCodeEnum`, `UserErrorCodeEnum`

When the device sends a stale `nextSyncToken` (older than 5 days per `ScheduleServiceImpl`), server returns `E0330` and the device must fall back to a full pull.

### Codes UB-as-SPC actually emits

These are copied **verbatim** (code + English message) from `FileErrorCodeEnum.java`
so the device sees byte-identical errors. Keep this table in sync when a handler
starts returning a new code.

| Code | Message | Where UB returns it |
|---|---|---|
| `E0321` | This file does not exist | `download_v3`: unknown/stale file id (`handlers/download.go`) |
| `E0324` | This file cannot be uploaded | `upload/finish`: staged file fails md5/size verify (`handlers/upload.go`) |
| `E0318` | The folder or file you want to delete does not exist | `delete_folder_v3`: unknown/stale id (`handlers/mutation.go`) |
| `E0320` | The folder or file you want to move or rename does not exist | `move_v3`: unknown/stale source id (`handlers/mutation.go`) |
| `E0308` | File does not exist | `copy_v3`: unknown/stale source id (`handlers/mutation.go`) |
| `E0322` | A file with the same name already exists | `move_v3`/`copy_v3`: collision at target with `autorename=false` |
| `E0712` | *(auth envelope)* | any JWT-protected route hit without a valid `x-access-token` (`auth.Middleware`) |

Note: the OSS byte endpoints (`GET /api/oss/download`, `POST /api/oss/upload`) do
**not** use these JSON codes — a bad signature returns HTTP 500 + bare plain-text
(`FileDownload/UploadException` → `GlobalExceptionHandler`), per §6 / U1.

## 8. DTO/VO field-name gotchas

Match these verbatim — do not "fix" them:

| Field | Where | Note |
|---|---|---|
| `nextPageTokens` (plural) | `ScheduleTaskDTO` request | Input |
| `nextPageToken` (singular) | `ScheduleTaskAllVO` response | Output |
| `content_hash` (snake_case) | `FileUploadFinishLocalDTO` | The only snake_case field observed |
| `lastModify` (no trailing 'd') | `ScheduleSortDTO` | All other timestamps use `lastModified` |
| Task IDs | `String` in requests, `Long` in responses | Don't unify |
| **File IDs** | **`String` in requests** (`download_v3`/`query_v3` send `"id":"16"`), `Long`/`String` in responses | Decompiled DTO says `Long`, but the device sends a **quoted string**. Jackson coerces it server-side; Go's `encoding/json` rejects string→int64, so type the DTO field `string` and `ParseInt` in the handler. **Confirmed from device traffic 2026-05-23** — typing it `*int64` made `download_v3` silently return `E0321` for every real request. |
| `token` | `LoginVO` | JWT field name |

### Upload `path` means different things in apply vs finish (device-confirmed 2026-05-24)

The device is **inconsistent** about what `path` carries in the two upload calls
(captured on hardware via the cleartext `:8089` tap):

- **`upload/apply`**: `path` is the **full destination path including the filename**
  — e.g. `{"path":"/NOTE/Note/20260524_181931 Testy.note","fileName":"20260524_181931 Testy.note","size":57898}` (note `size` is a **number** here).
- **`upload/finish`**: `path` is the **parent directory** (with a trailing slash) and
  `fileName` is separate — e.g. `{"path":"/NOTE/Note/","fileName":"20260524_181931 Testy.note","content_hash":"…","innerName":"…","size":"57898"}` (`size` a **string** here).

So the promotion target must be computed at **finish** as `path.Join(finish.path,
finish.fileName)` — **never** by joining the apply-recorded path with a filename
(that double-nests into `…/X.note/X.note`). UB ignores apply's `path` for placement
and trusts finish's parent+fileName. Found because the first device upload created a
*directory* named like the note containing the note (`staging.Finalize` was joining
the apply full-path with the fileName).

### `move_v3`/`copy_v3` `to_path` is the full destination path (device-confirmed 2026-05-24)

Despite the decompiled `@ApiModelProperty("Target parent directory")`, the device
sends `to_path` as the **full destination path including the (new) filename**, NOT
a parent dir. A device rename sent `{"id":"862","to_path":"/NOTE/Note/<newname>.note","autorename":false}`.
So the move/copy target is `SafeResolve(to_path)` directly — do **not** join the
source basename onto it (same double-nesting bug as upload: it produced
`…/<newname>.note/<oldname>.note`). The new filename is `filepath.Base(to_path)`.

### Summary (digest) DTO casing + sync semantics (Phase D — hardware-confirmed 2026-05-25)

Unlike the file DTOs (snake_case: `to_path`, `content_hash`), the `F_SummaryController`
DTOs/VOs carry **no `@JsonProperty`** and so serialize their **camelCase** Java field
names verbatim (`uniqueIdentifier`, `parentUniqueIdentifier`, `md5Hash`,
`sourceType`, …). Confirmed against the decompiled source AND a live device trace:

- **`handwriteMD5` (uppercase) in the request/domain, but `handwriteMd5` (lowercase
  d5) in the response `SummaryInfoVO`** (`SummaryInfoVO.java:15`). UB models both exactly.
- **`SummaryDO.isSummaryGroup` and `isDeleted` are Strings `"Y"`/`"N"`, not booleans**
  (`SummaryDO.java:25,36`).
- **`createTime`/`updateTime` are epoch-millis NUMBERS** (device-confirmed: it pulled a
  populated `query/summary/id` carrying `"createTime":1779683680101` and accepted it).
  UB emits omitempty millis — correct.
- **`upload/apply/summary` returns `partUploadUrl:""`** — device-confirmed to use the
  single-shot `fullUploadUrl` for a 29 KB `.mark` (no chunking). The real *file*-upload
  apply VO also carries `bucketName`/`xAmzDate`/`authorization` S3 fields; the device does
  NOT need them on the digest apply (UB's lean 3-field VO was accepted).

**Item identity lives in `metadata`, not the top-level fields** (device-confirmed): on
`add/summary` the device sends an empty top-level `uniqueIdentifier` (and no top-level
`author`/`fileId`/`tags`); the item's stable id is `metadata.unique_identifier` and the
author is `metadata.author`. UB stores `metadata` verbatim (lossless round-trip) and lifts
`metadata.unique_identifier` into the `unique_identifier` column for dedup. PDF digests are
`sourceType:1` with a `metadata.document_location_data` chapter/page span; note digests are
`sourceType:2` with `note_fileId`/`note_pageId`/`note_page`.

**`metadataMap` (in `query/summary/hash`) must preserve numeric literals** — decode with
`json.UseNumber`, else a number like `source_size:18992668` stringifies as
`"1.8992668e+07"` (device-confirmed corruption, fixed).

**Digest sync is DEVICE-AUTHORITATIVE on delete** (device-confirmed): a digest deleted on
the device sends `DELETE /delete/summary {"id":N}` (UB soft-deletes — works). But a
digest soft-deleted **server-side only** does NOT propagate down — the device, seeing its
local digest missing from `query/summary/hash`, **re-asserts it via `PUT /update/summary`**.
UB currently no-ops that update (its `GetByID` excludes soft-deleted), so the row stays
deleted while the device keeps re-pushing → benign perpetual re-push + divergence. Fine for
D1 (device round-trip). **D2 tombstone (built 2026-05-26):** a UB/web-initiated delete now
pushes a `DELETE_DIGEST` over the **`digest`** Socket.IO event so the device removes its
local copy instead of re-asserting. The wire shape replicates the real server's
`SocketDigestMessageData<DigestMessageTemplate>` (`SocketIoConstant.EVENT_DIGEST` /
`MSG_TYPE_DIGEST = "DIGEST-SYN"`): on a delete only `messageType`/`dataType`/`equipmentNo`/
`timestamp`/`id` are populated (`dataType` = sourceType: "1"=PDF, "2"=note). `id` is UB's
own digest id — the same id it returns in `query/summary/hash`, so the device's local key
matches. `equipmentNo` is sent as `"ultrabridge"` (the device is expected to key on `id`);
**both the `equipmentNo` value and the device's honoring of the frame are pending hardware
capture** (NPM flip → `:8089` tcpdump). `update/summary` can also *add* handwriting to a previously text-only
digest (new `.mark` uploaded + promoted) and move an item between groups via
`parentUniqueIdentifier` — both device-confirmed.

## 9. Storage paths and timing constants (SPC-side, FYI)

From `Constant.java` and `application.yml` style references in code:

- `local.file.directory = /home/supernote/data`
- `local.file.convert.directory = /home/supernote/convert`
- `local.file.watcher.polling.interval = 5000` ms
- `local.part.upload.targetDirPath` — chunked-upload temp area
- UB stores notes under its own `NotesPath` per source config; the SPC paths are irrelevant for UB-as-SPC except as documentation of the original semantics.

## 10. Deployment topology — this dev install only

See `memory/reference_spc_dev_topology.md`. Summary:

```
device → supernote.broken.works:443 (Let's Encrypt, NPM on hydrae 192.168.9.30)
       → HTTP cleartext → neptune 192.168.9.52:19072
       → supernote-service container :8080 (internal nginx)
         /           → :9888 (Vue UI, out of scope)
         /api/*      → :19071 (Spring backend)
         /socket.io/* → :18072 (Engine.IO file+digest only; :18073 not exposed)
```

R3 (TLS pinning) closed by this topology — device accepts the public LE cert NPM serves.

## 11. Device wire observations (0b + 0c, 2026-05-22)

Captured via a Go tap proxy (`/home/sysop/spc-rev/tap/`) and a JWT stub (`/home/sysop/spc-rev/jwt-test/`) inserted on the cleartext leg. Raw captures preserved at `/home/sysop/spc-rev/tap/tap-0b-*.jsonl` and `/home/sysop/spc-rev/jwt-test/0c-capture-*.log`. Device: Supernote Nomad, `SN078C10034074`.

**Endpoints the device actually hit in one full sync session (0b):**
```
 7  POST /api/file/schedule/task/all          (tasks via REST polling)
 4  POST /api/equipment/bind/status
 3  POST /api/file/schedule/group/all
 3  POST /api/file/3/files/query/by/path_v3
 2  PUT  /api/file/schedule/task/list
 2  POST /api/oss/upload                       (signed; validated §6)
 2  POST /api/file/query/summary/hash
 2  POST /api/file/query/summary/group
 2  POST /api/file/3/files/upload/apply        ← UPLOAD VARIANT (not terminal/*)
 2  POST /api/file/3/files/query_v3
 2  POST /api/file/2/files/upload/finish
 2  POST /api/file/2/files/synchronous/start
 2  POST /api/file/2/files/synchronous/end
 2  POST /api/file/2/files/list_folder
 1  POST /api/user/query
 1  POST /api/file/query/summary/id
 1  POST /api/file/query/server
 1  POST /api/file/capacity/query
 1  POST /api/file/3/files/download_v3
 1  POST /api/file/3/files/delete_folder_v3
 1  GET  /api/oss/download                     (signed; validated §6)
```

**Resolved questions:**
- **Upload variant:** the device uses `/api/file/3/files/upload/apply` + `/api/oss/upload` + `/api/file/2/files/upload/finish`. The `/api/file/terminal/upload/*` variants are **not** used. → Phase 4b.
- **Engine.IO:** single socket.io connection, EIO v3, permessage-deflate, 5 s heartbeat (`2`/`3` + `42["ratta_ping"]`). No task channel. → Phase 1c single listener.
- **note→pdf/png:** not hit during normal sync. → Phase 5c stays conditional/skippable.
- **Summary endpoints ARE hit** (`query/summary/{hash,group,id}`). → must stub (see §5).
- **JWT acceptance: YES** (0c). Device accepts `Constant.SECRET`-signed token, no client-side validation.
- **Path normalization:** device emits non-normalized paths with double slashes (`Personal//IMG_…`). UB must tolerate.

**Phase 0 risk table — all closed:** R1 (OSS, validated), R2 (JWT accepted), R3 (LE cert, no pinning), R4 (EIO v3), R5 (E0330 + error enums), R6 (path encoding observed).

### 1b device-login test (2026-05-22, against UB server mode)

Real device flipped to UB (NPM `13.conf` `$port` 19072→8089) logged in and made authenticated calls. Wire facts confirmed:
- **Real-SPC tokens verify under `Constant.SECRET`.** Reproduced the device's own token's HMAC-SHA256 signature exactly with `Constant.SECRET` — so real device tokens (not just UB-minted ones) verify under the same secret. The token shape is `{createTime, equipmentNo, userId, key:"<userId>_<createTime>_<ms>_<equipmentNo>"}` with **no `exp`** (terminal tokens are non-expiring) — UB's `Verify` must not require `exp`.
- **Login flow observed:** `check/exists/server → query/random/code → account/login/equipment → terminal/user/bindEquipment`, all 200; then `/api/user/query` with the new token. (Note: UB writes error envelopes as HTTP 200 + `success:false`, so NPM access-log status alone does not indicate auth pass/fail.)
- **Engine.IO (1c preview):** post-login the device repeats `GET /socket.io/?sign=…&random=…&EIO=3&transport=websocket&type=<equipmentNo>&token=<JWT>` every ~5 s (404 until 1c) and converges on the new UB userId for both REST and socket — no userId split-brain.
