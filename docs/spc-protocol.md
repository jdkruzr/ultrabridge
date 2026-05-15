# SPC Protocol Reference

Working specification for re-implementing Supernote Private Cloud (SPC) endpoints in UltraBridge so the Supernote device cannot tell the difference. Built from CFR-decompiled output of `supernote-service.jar` v2.1.4.RELEASE. All citations are `<FQN.java>:<line>` against `/home/sysop/spc-rev/cfr-decrypted/`.

**Status:** Phase 0 in progress. 0a (CFR + endpoint enumeration) and 0d (OSS HMAC) closed by CFR inspection. 0b (device wire observations) and 0c (JWT acceptance verdict) pending physical-device sessions.

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
- **Terminal tokens are non-expiring at the JWT level.** Per `JwtTokenUserUtil.createToken` (`JwtTokenUserUtil.java:97-110`): if `equipmentNo` is provided (i.e., device login), the token is signed **without** `.withExpiresAt(...)`. Non-terminal tokens (web UI flows, where `equipmentNo` is null) **do** include `exp`. Effective TTL for terminal devices comes from Redis state keyed by the `key` claim.
- Claim shape (terminal token, observed in `JwtTokenUserUtil.java:107`):
  ```json
  {"userId": "<string>", "createTime": <unix-seconds>, "equipmentNo": "<string>", "key": "<redis cache key>"}
  ```
  Real token captured in the JAR's test code (`JwtTokenUserUtil.java:59`) decodes to:
  ```json
  {"createTime": 1752550338, "equipmentNo": "SN100C1000531", "userId": "1104633280458842112"}
  ```
  (No `key` claim in that sample — looks like the test omitted it.)
- Login response VO field name for the JWT is **`token`** (`LoginVO.token`), not `jwtToken` or `accessToken`.

**JWT acceptance test (0c) is still required** to confirm an unmodified device accepts tokens signed with `Constant.SECRET` and the above claim shape.

## 3. Engine.IO

- Protocol version: **Engine.IO v3** (confirmed live on the wire: access log shows `EIO=3&transport=websocket`).
- Server library: `corundumstudio/netty-socketio` 2.0.3.
- Ping cadence: `socket.pinginterval = 5000` ms, `socket.pingtimeout = 25000` ms.
- Custom keepalive event: `ratta_ping`.

### Channels and ports (per `SocketIoConstant.java`)

| Logical namespace | Port (JVM bean) | Events accepted/emitted |
|---|---|---|
| `_fileSocket_` | 18072 (`socketIOServer` bean) | `ServerMessage` (→client), `ClientMessage` (→server), `Received` (ack), `ratta_ping` |
| `_digestSocket_` | 18072 (same bean as file) | `digest` |
| `_todoSocket_` | 18073 (`socketIOServerStask` bean) | `to-do` |

**Deployment caveat (this dev install):** SPC container's internal nginx routes `/socket.io/*` to **port 18072 only**. Port 18073 is bound inside the container but unreachable externally. See `memory/reference_spc_dev_topology.md`. The device-side use of the task channel needs to be confirmed by 0b boot-trace.

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

### Stubs (canned success or 404)

- Sharing: `F_ShareController` (1 endpoint)
- Summary: `F_SummaryController` (16 endpoints; all under `/api/file/(add|delete|query|download)/summary/*`)
- Dictionary / Reference: `B_DictionaryController` (5), `B_ReferenceController` (4)
- Email server: `U_EmailServerController` (4)
- User registration / password / valid-code / sensitive ops: `U_UserRegisterController`, `U_PasswordController`, `U_ValidCodeController`, `U_SensitiveOperationController`, `U_FigureVaildCodeController`
- Web file controller (humans only): `F_FileLocalWebController` (17) — most likely 404 from device perspective

## 6. OSS HMAC (signing primitive for upload/download URLs)

Specified in `com/ratta/util/SignVerifier.java` (full decompilation; ~80 lines, all readable). **Note:** despite the file name, the actual upload/download URL signing is plain SHA-256, **not** HMAC — the secret is concatenated into the data and the result hex-encoded. The class also contains a separate static HMAC-SHA256 + Base64 method (`signData` / `verifySignature`) for a different code path; do not confuse them.

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

## 8. DTO/VO field-name gotchas

Match these verbatim — do not "fix" them:

| Field | Where | Note |
|---|---|---|
| `nextPageTokens` (plural) | `ScheduleTaskDTO` request | Input |
| `nextPageToken` (singular) | `ScheduleTaskAllVO` response | Output |
| `content_hash` (snake_case) | `FileUploadFinishLocalDTO` | The only snake_case field observed |
| `lastModify` (no trailing 'd') | `ScheduleSortDTO` | All other timestamps use `lastModified` |
| Task IDs | `String` in requests, `Long` in responses | Don't unify |
| `token` | `LoginVO` | JWT field name |

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

## 11. Open items pending 0b/0c (device-required)

- 0b — full endpoint-list-the-device-actually-hits, path encoding (NFC/NFD), Engine.IO frame contents (does device open multiple socket.io connections / different paths?), which upload variant the device uses, whether device hits note→pdf/png endpoints.
- 0c — does an unmodified device accept a JWT signed with `Constant.SECRET`?
