# UltraBridge → Supernote Private Cloud (UB-as-SPC) Refactor

## Context

UltraBridge currently runs as a sidecar to Supernote Private Cloud (SPC): UB syncs tasks via SPC's REST API, listens to SPC's Engine.IO server for file-change notifications, and writes file metadata directly into SPC's MariaDB after OCR injection. This two-store model has caused recurring pain — pagination bugs, "phantom" tasks, dupe loops, write-through races, and an entire `internal/tasksync/` engine devoted to reconciling two ostensibly-equivalent stores. UB has never been authoritative.

The blocker on doing better — i.e. having UB *be* the cloud the device talks to (the original "NoteBridge" vision) — used to be that SPC's endpoint semantics were unknown. That blocker is gone: the SPC Java JAR is fully decompiled at `/home/sysop/spc-rev/decrypted-classes/`, with selective CFR output at `/home/sysop/spc-rev/cfr-decrypted/`. We have the spec.

**Outcome we want:** the Supernote device boots, points its `cloud.host` DNS at UB instead of SPC, and UB serves every endpoint the device hits — login, task sync, file list/upload/download, Engine.IO push. UB becomes the single source of truth for both notes and tasks. MariaDB and the SPC container are no longer required.

This is a project-scale refactor, not a session-scale fix.

---

## Design constraint: device sees no difference

Supernote devices are not customizable in any meaningful way. The device-side change for this refactor is **DNS only**: `cloud.host` resolves to UB instead of SPC. No firmware patch, no settings tweak, no special build, no custom app. UB must be **indistinguishable from SPC on the wire** — same URL paths, same JWT format and (default) secret, same Engine.IO ports and protocol version, same success/error envelope, same field names in DTOs. Anywhere a temptation arises to "improve" the protocol or rename a field for cleanliness, the answer is no — match cfr-decrypted byte-for-byte.

This constraint shapes every other decision below.

---

## Phase isolation principles

Each top-level phase is a self-contained unit of work designed to be executed in its own session with its own context window. Between top-level phases, the engineer should be able to `/clear` or `/compact` without losing the ability to pick up the next phase cold. Sub-phases within a top-level phase are independently-merged commits/PRs but share session context. Every phase obeys these rules:

1. **Each sub-phase produces a runnable, testable artifact** that can be merged to `main` independently. No sub-phase leaves the codebase in a half-broken state.
2. **Each top-level phase's first task is to write its own implementation plan** at `docs/implementation-plans/spc-phase-N.md`. That document is detailed, sub-phase-by-sub-phase and task-by-task, and assumes zero memory of previous phases. The design plan you're reading now stays compact and stable.
3. **Each top-level phase declares Entry state and Exit state explicitly.** Entry state = what's true in the codebase when the phase starts (verifiable via grep/build). Exit state = what's true when the phase is done; becomes the next phase's Entry state.
4. **Each top-level phase declares "Context to load on a cold session"** — exact files and memory IDs to read first when starting fresh.
5. **Persistent reference docs are the inter-phase memory.** Phase 0's outputs (captured protocol notes, decompiled-source notes) are written to `internal/spcserver/CLAUDE.md` and `docs/spc-protocol.md`. Phase 1+ reads those docs instead of holding facts in working memory.
6. **No forward references between top-level phases.** Phase N's plan must not depend on knowing what Phase N+1 will do. If a future cleanup is anticipated, it lives only in the phase that performs it.
7. **Compact/clear checkpoints between top-level phases.** Within a phase, sub-phases share session context; between top-level phases, `/clear`.

The 6 top-level phases below are subdivided into ~20 sub-phases. Sub-phase count is not a constraint; correctness is.

---

## High-level approach (stable across all phases)

- **Greenfield package** `internal/spcserver/` contains all device-facing handlers, JWT auth, the Engine.IO server, and SPC-shape DTOs. Built up sub-phase by sub-phase; never modified by code outside the package except to call into it.
- **Translate at the controller boundary**: device-facing endpoints map SPC-shaped JSON ↔ UB's existing SQLite stores (`internal/taskdb`, `internal/notedb`, `internal/notestore`). No replicated MariaDB schema; no second store.
- **JWT**: replicate SPC's HS256 with the literal hardcoded secret as the default (`UB_SPC_JWT_SECRET` env var allows rotation once we've verified the device doesn't validate locally).
- **Engine.IO direction flip**: `internal/sync/notifier.go` is currently a client; UB needs a server. Hand-roll on `gorilla/websocket` (already a dep); 1-day spike on `googollee/go-socket.io` may shortcut.
- **Coexistence**: UB-as-SPC binds a separate listener; real SPC keeps serving the device until Phase 4 ships. DNS flip = the cutover. Real SPC stays up one more week as escape hatch.
- **Spec source**: `/home/sysop/spc-rev/cfr-decrypted/` is ground truth for every endpoint. JADX at `/home/sysop/spc-rev/decompiled/` is a structural reference but its method bodies are unreliable.

## Deployment topology (this dev environment, verified 2026-05-15)

Material for Phase 0b/0c procedures and for Phase 1c (Engine.IO server design):

- **Inbound path**: device → `supernote.broken.works:443` (Let's Encrypt cert served by NPM on `hydrae` / `192.168.9.30`) → HTTP cleartext → `neptune:19072` → SPC container internal nginx on `:8080` → routes by URL:
  - `/` → `:9888` (Vue UI; out of scope, see "Scope dropped")
  - `/api/*` → `:19071` (Spring backend, JWT-protected REST)
  - `/socket.io/*` → `:18072` (Engine.IO — file+digest only)
- **R3 closed**: NPM serves a publicly-trusted Let's Encrypt cert; device accepts it; no TLS pinning concern.
- **R4 closed (preliminarily)**: access log confirms `EIO=3&transport=websocket` on the wire. netty-socketio 2.0.3 = EIO v3 confirmed live.
- **Task channel (`:18073`) is not externally reachable** in this deployment. The internal nginx routes all `/socket.io/*` to 18072. Either the device only uses the file+digest channel (with task events somehow multiplexed there) or task sync is REST-poll only. Phase 0b boot-trace is the next confirmation point. Phase 1c should default to a single Engine.IO listener (file+digest namespace on `UB_SPC_SOCKETIO_FILE_PORT`); the task port is conditional on 0b evidence.
- **0b body-capture is cheap**: nginx already JSON-logs every request to `/mnt/supernote/sndata/logs/web/access.log` (host-readable). `req_body` field is empty because `proxy_request_buffering off` is global; either flip it for the capture window or insert a small Go reverse proxy on the `neptune:19072 → container:8080` leg.
- **0c flip is trivial**: add a new NPM proxy host (e.g. `supernote-test.broken.works`) with its own LE cert pointing at a stub Go server. No device cert install required; just DNS override on the device for that hostname. Real SPC stays undisturbed.

This topology is specific to this dev install, **not** a deployment model for end users. End-user deployment is just "point `cloud.host` at UB."

## Verified protocol facts (sweep done 2026-05-15; corrected from earlier drafts)

Sweep against the decompiled tree confirmed plan coverage and surfaced these specific corrections — **encode these in `docs/spc-protocol.md` during Phase 0**:

- **Envelope shape is `{success, errorCode, errorMsg}`** — NOT `{success, code, msg, data}`. `BaseVO` has three fields: `success` (bool), `errorCode` (String), `errorMsg` (String). VOs **extend** `BaseVO` directly; payload fields sit alongside the envelope fields, not nested under `data`.
- **JWT signing uses `Constant.SECRET`, not `Constant.JWT_SECRET`.** `JwtTokenUserUtil.secret = Constant.SECRET`, which is a ~280-character string starting `suernotea1hK52bgkf9N7PQ5E3KDqKeCIT719a6kh04eSTSBLv7e9tPtw2L8S6pEDMy7lAIv2CYjg5Ncy7ep5zDS7hH9CDAZnLieo66g7F8iZmClK9a1xEEPewXLhkM4KTKI7pz2Lkl7Cds4MpClNvNCVHPbfWKNyiFSGUztbnmqDWgNAinPBNamwDUQpT8RwCO1wc9vYTTQsmXm8ByioHC3QkRMZtHZnIWWCkIWECPzSJGOowNliAavzVCMsKadYnsH322n`. The 32-char `JWT_SECRET = "7786df7fc3a34e26a61c034d5ec8245d"` is used for something else (possibly Redis key derivation). Phase 0c JWT acceptance test uses `Constant.SECRET`.
- **JWT TTLs (verbatim from Constant.java):** standard 1h (`JWT_TTL=3600000`), device 8h (`JWT_TTL_8=28800000`), 30-day (`JWT_DAY_TTL=2592000000`), refresh threshold 55min (`JWT_REFRESH_INTERVAL=3300000`).
- **Auth header is `x-access-token`** (`Constant.AUTHORIZE_TOKEN`).
- **Engine.IO uses netty-socketio 2.0.3** (corundumstudio/socketio), which speaks Engine.IO v3 / Socket.IO v1.
- **Engine.IO event model:** events are sent as Socket.IO `42[<eventName>, <jsonPayload>]` frames. Event names from `SocketIoConstant.java`:
  - File channel (port 18072): `ServerMessage` (server→client), `ClientMessage` (client→server), `Received` (ack), `ratta_ping` (keepalive)
  - Task channel (port 18073): `to-do`
  - Digest channel (port 18072 — same server bean as file): `digest`
  - Inside the JSON payload, the `msgType` field carries `FILE-SYN`, `TASK-SYN`, or `DIGEST-SYN`. The op (`STARTSYNC`, `MODIFYFILE`, `DELETEFILE`, `ADDFOLDER`, `COPYFILE`, `MOVEFILE`, `DOWNLOADFILE`, `ADD_DIGEST`, `UPDATE_DIGEST`, `DELETE_DIGEST`, `QUERY`, `SORT`, `WAITING`) is a separate field.
  - Three logical namespaces (`_fileSocket_`, `_todoSocket_`, `_digestSocket_`) split across **two server ports**: `socketIOServer()` hosts file+digest on `socket.port=18072`; `socketIOServerStask()` hosts task on `socket.task.port=18073`.
- **`ResubmitCheck` annotation defaults `interval=1`, `timeUnit=SECONDS` (1 second, not 5).** Individual endpoints can override; observed overrides include schedule/group/task creation. Phase 1d's dedup map uses 1s as the default with per-endpoint overrides driven by what cfr-decrypted shows on each annotated method.
- **`E0330` exists** in `FileErrorCodeEnum`: `"NextSyncToken timeout"`. Returned when the client's `nextSyncToken` is stale (>5 days); device must fall back to full pull.
- **DTO field-name gotchas** (must match verbatim):
  - `ScheduleTaskDTO` (request) uses **`nextPageTokens` (plural)** as the input; `ScheduleTaskAllVO` (response) uses **`nextPageToken` (singular)**. Different.
  - `FileUploadFinishLocalDTO.content_hash` is **snake_case** (the only snake_case field observed); everything else is camelCase.
  - `ScheduleSortDTO.lastModify` (no `d` at the end); all other timestamps use `lastModified`.
  - Task IDs: `String` in request DTOs, `Long` in response VOs.
  - `LoginVO.token` is the JWT field name (not `jwtToken` or `accessToken`).
- **`LoginDTO` fields:** `account` (@NotBlank), `password` (@NotBlank), `equipment` (Integer; 3 = terminal), `equipmentNo`, `loginMethod` (@NotBlank; "1"=phone, "2"=email, "3"=WeChat), `countryCode`, `browser`, `language`, `timestamp`.
- **`socket.pinginterval=5000`, `socket.pingtimeout=25000`** — keepalive cadence for the Engine.IO server.
- **Storage paths SPC uses** (for reference; UB stores under its own NotesPath): `local.file.directory=/home/supernote/data`, `local.file.convert.directory=/home/supernote/convert`, `local.file.watcher.polling.interval=5000`.

### Endpoints discovered in sweep not in the original phase lists (slot them in)

- **Phase 1 stubs** — add: `GET /api/file/query/server` (a server-reachability check the device likely calls on boot; canned 200 with `{success:true}`).
- **Phase 2** — add: `POST /api/file/3/files/query_v3` (get file by ID, v3) and `POST /api/file/3/files/query/by/path_v3` (get file by path, v3). Device-facing reads.
- **Phase 4** — add: `POST /api/file/terminal/upload/apply` and `POST /api/file/terminal/upload/finish` (terminal-specific upload variants — Phase 0b boot trace will confirm whether the device prefers these over the `/api/file/3/files/upload/*` variants; **implement whichever the device actually calls** and stub the other).
- **Phase 5b** — add: `POST /api/file/label/list/search` (label-based search alongside FTS5 hit search).
- **Phase 5c (conditional)** — add alongside note→pdf and note→png: `POST /api/file/pdfwithmark/to/pdf`.
- **Equipment binding flow** (drop, but possibly stub more aggressively in Phase 1a depending on Phase 0b boot trace): `POST /api/terminal/user/activateEquipment`, `POST /api/terminal/user/bindEquipment`, `POST /api/terminal/equipment/unlink`, `POST /api/equipment/query/by/equipmentno`. If the device pings these during normal sync (not just initial pairing), stub canned-success responses in Phase 1a.

---

## Phase 0 — De-risking and persistent reference docs

**Goal:** Convert open unknowns into written facts that later phases can read. No production code merged.

### Entry state
- `/home/sysop/spc-rev/cfr-decrypted/` contains 1 Java file (the previously-decompiled `ScheduleServiceImpl.java`).
- `/home/sysop/spc-rev/decrypted-classes/` contains all ~437 raw .class files.
- No `internal/spcserver/` package exists.
- No `docs/spc-protocol.md` exists.

### Context to load on a cold session
- This design plan
- Memory `project_spc_classfinal_decrypted.md` — decryption procedure if more class material is needed
- Memory `reference_supernote_service_internals.md` — runtime layout, JAR location, JADX/CFR locations

### Sub-phases

**0a — CFR decompile + endpoint enumeration (no device required)**
- Run CFR against the classes listed below; output to `/home/sysop/spc-rev/cfr-decrypted/`.
- Enumerate every URL from compiled mapping tables: `unzip -p supernote-service.jar 'BOOT-INF/classes/com/ratta/controller/*.class' | strings | grep -E "^/api/"`. This catches controllers JADX/CFR may have missed (R9).
- Classes to decompile:
  ```
  com.ratta.constants.Constant
  com.ratta.user.dto.LoginDTO
  com.ratta.user.service.impl.*
  com.ratta.user.util.JwtTokenUserUtil
  com.ratta.controller.U_LoginController
  com.ratta.controller.F_ScheduleController
  com.ratta.controller.F_FileLocalController
  com.ratta.file.service.impl.FileLocalServiceImpl
  com.ratta.controller.O_OssLocalController
  com.ratta.oss.local.LocalFileUtil
  com.ratta.controller.E_EquipmentController
  com.ratta.aspect.ResubmitCheckAspect
  com.ratta.socket.io.*
  ```
- Driver: `java -jar /home/sysop/spc-rev/cfr-0.152.jar /home/sysop/spc-rev/decrypted-classes/com/ratta/<package>/<Class>.class --extraclasspath /home/sysop/spc-rev/supernote-service.jar:/home/sysop/spc-rev/decrypted-classes --outputdir /home/sysop/spc-rev/cfr-decrypted`
- **Exit / acceptance:** The classes listed are present in `cfr-decrypted/` with real method bodies (not `Method not decompiled`). The endpoint enumeration is committed as a draft section in `docs/spc-protocol.md`.

**0b — Device boot-trace capture (requires real device + mitmproxy)**
- Stand up `mitmproxy` between device and real SPC; install root cert on device if needed.
- Record one full device boot + sync session.
- Document in `docs/spc-protocol.md`:
  - TLS posture (self-signed accepted? CN/SAN required? cert pinning?)
  - Engine.IO version on the wire (`EIO=3` vs `EIO=4` from initial socket.io handshake query string)
  - Actual error codes the device reacts to (`E0330` = token expired, etc.)
  - Path normalization (NFC/NFD), URL encoding of Chinese filenames
  - Full list of endpoints the device hits on boot (URL + method + payload sketch)
- **Exit / acceptance:** `docs/spc-protocol.md` contains the TLS / EIO / error-code / endpoint-list sections. R3, R4, R5, R6 closed.

**0c — JWT acceptance test (requires real device)**
- Stand up a minimal stub HTTP server that returns an HS256 JWT signed with `7786df7fc3a34e26a61c034d5ec8245d` (the SPC literal secret from `Constant.java`) on the login endpoint.
- Point a device at it via DNS.
- Observe: does the device proceed past login? Or does it reject the token?
- **Exit / acceptance:** `docs/spc-protocol.md` documents the binary result (accepted / not accepted). R2 closed. If not accepted, follow-up work (extract real secret from device firmware) is logged as a new sub-phase before Phase 1 starts.

**0d — OSS HMAC scheme decoding (CFR first, mitmproxy fallback)**
- Primary: CFR `com.ratta.oss.local.LocalFileUtil` and `com.ratta.controller.O_OssLocalController`. Read the actual signing function body; document inputs (path? timestamp? nonce? userId?), hash function, encoding (hex/base64) in `docs/spc-protocol.md`.
- Fallback if CFR can't decompile cleanly: capture an actual upload request from 0b's mitmproxy session; reverse-engineer the signature from observed inputs/outputs.
- **Exit / acceptance:** `docs/spc-protocol.md` contains a fully-specified OSS HMAC algorithm sufficient to implement Phase 3a from scratch. R1 closed. Phase 4 unblocked.

**0e — Consolidate into reference docs**
- Polish `docs/spc-protocol.md` from the draft sections produced in 0a–0d.
- Write `internal/spcserver/CLAUDE.md` — domain doc for the new package (even though the package doesn't exist yet). Summarize purpose, conventions (DTOs match cfr-decrypted verbatim, envelope shape, JWT middleware), and point at `docs/spc-protocol.md` for protocol facts.
- **Exit / acceptance:** Reading `docs/spc-protocol.md` and `internal/spcserver/CLAUDE.md` alone (no other context) is sufficient for an engineer to implement any endpoint in Phase 1+. A reviewer with no prior context can verify Phase 0 by reading these two docs.

### Exit state (top-level Phase 0)
- `/home/sysop/spc-rev/cfr-decrypted/` populated as listed in 0a.
- `docs/spc-protocol.md` exists and is self-sufficient.
- `internal/spcserver/CLAUDE.md` exists (placeholder OK since no source files yet).
- No code changes to UB's runtime packages.

### Files this phase touches
**Created**: `docs/spc-protocol.md`, `internal/spcserver/CLAUDE.md`, decompiled output under `/home/sysop/spc-rev/cfr-decrypted/`
**Modified**: none
**Not touched**: any UB runtime package

### Compact/clear checkpoint
Commit Phase 0 outputs. `/clear` before starting Phase 1. Phase 1's plan-generation reads this design plan + `docs/spc-protocol.md` + `internal/spcserver/CLAUDE.md`.

---

## Phase 1 — Skeleton + Auth + Tasks

**Goal:** Device logs into UB and syncs its task list end-to-end. Files come later. This phase exercises auth, JWT, ResubmitCheck, the Engine.IO server, and SPC-shape DTOs on the cheapest possible subsystem.

### Entry state
- Phase 0 exit state holds: `docs/spc-protocol.md` exists with TLS/EIO/JWT/HMAC facts.
- `internal/spcserver/CLAUDE.md` exists (stub).
- No `internal/spcserver/*.go` source files yet.
- Existing `internal/tasksync/supernote/` (REST client) and `internal/sync/notifier.go` (Engine.IO client) are unchanged and still in use.

### Context to load on a cold session
- This design plan
- `docs/spc-protocol.md`
- `internal/spcserver/CLAUDE.md`
- `internal/taskdb/CLAUDE.md`, `internal/taskstore/CLAUDE.md`
- `internal/tasksync/supernote/client.go` — reference for byte-level wire format and field names
- `internal/sync/notifier.go` — reference for Engine.IO frame handling

### Sub-phases

**1a — HTTP listener skeleton + envelope + config wiring**
- New code: `internal/spcserver/server.go` (TLS-capable HTTP router on `UB_SPC_LISTEN_ADDR`, default `:8089`), `internal/spcserver/envelope.go` (SPC success/error envelope helper — `{success: bool, errorCode: string, errorMsg: string}` matching `BaseVO`; VOs **extend** `BaseVO`, payload fields sit alongside the three envelope fields, **not** nested in `data`)
- Config keys added to `internal/appconfig/keys.go`: `UB_SPC_LISTEN_ADDR`, `UB_SPC_TLS_CERT`, `UB_SPC_TLS_KEY`, `UB_SPC_MODE` (`client` | `server`, default `client`)
- Wire into `cmd/ultrabridge/main.go`: spawn the SPC server goroutine when `UB_SPC_LISTEN_ADDR` is set.
- Single stub endpoint: `POST /api/equipment/bind/status` → `{"success":true,"data":{"bound":true}}`.
- **Acceptance:** `curl -k https://localhost:8089/api/equipment/bind/status` returns the canned envelope. `UB_SPC_MODE=client` produces identical runtime behavior to today. Build/test green.

**1b — JWT auth + login endpoint**
- New code: `internal/spcserver/auth/jwt.go` (HS256 issue/verify; default secret = **`Constant.SECRET`** (the long ~280-char string from `JwtTokenUserUtil.secret`, not the 32-char `JWT_SECRET`); TTLs per Constant.java — terminal login uses `JWT_TTL_8 = 28800000` (8h); override via `UB_SPC_JWT_SECRET`), `internal/spcserver/auth/middleware.go` (extracts **`x-access-token`** header, populates request context), `internal/spcserver/dto/login.go` (`LoginDTO` with `account`/`password`/`equipment`/`equipmentNo`/`loginMethod` etc., `LoginVO` extending `BaseVO` with `token` field for the JWT), `internal/spcserver/handlers/login.go` (`POST /api/official/user/account/login/equipment`)
- Config keys added: `UB_SPC_JWT_SECRET`, `UB_SPC_DEVICE_ACCOUNT`, `UB_SPC_DEVICE_PASSWORD`
- Protect a stub endpoint with the auth middleware (e.g. `POST /api/user/query`).
- **Acceptance:** Real device pointed at UB via DNS logs in successfully (terminal flow, `equipment=3`). `curl` with the returned JWT in `x-access-token` succeeds on the protected stub; without the JWT fails with the SPC-expected error envelope.

**1c — Engine.IO v3 server (connection-only)**
- New code: `internal/spcserver/socketio/server.go` (Engine.IO v3 server matching netty-socketio 2.0.3 wire behavior — handshake, polling-to-WebSocket upgrade, frame parse, Engine.IO ping/pong **plus** `ratta_ping` custom keepalive, connection registry keyed by `equipmentNo` from JWT)
- **Two ports, three logical namespaces**: file channel + digest channel both live on `UB_SPC_SOCKETIO_FILE_PORT` (default `:18072`, equivalent to SPC's `socketIOServer` bean); task channel on `UB_SPC_SOCKETIO_TASK_PORT` (default `:18073`, equivalent to `socketIOServerStask` bean). Namespaces: `_fileSocket_`, `_digestSocket_`, `_todoSocket_`.
- Event-name registry (events accepted/emitted, no business logic yet): `ServerMessage` and `ClientMessage` (file channel; payload carries `msgType=FILE-SYN`), `to-do` (task channel; `msgType=TASK-SYN`), `digest` (digest channel; `msgType=DIGEST-SYN`), `Received` (ack), `ratta_ping` (keepalive). Ping interval / timeout per SPC: `5000`/`25000` ms.
- Config keys added: `UB_SPC_SOCKETIO_FILE_PORT`, `UB_SPC_SOCKETIO_TASK_PORT`
- **Acceptance:** Real device connects to both ports and stays connected. Engine.IO ping/pong + `ratta_ping` keep-alive works for ≥30 min without disconnect.

**1d — Task endpoints + mapping + dedup**
- New code: `internal/spcserver/dto/` (task DTOs matching cfr-decrypted **verbatim** — see the "DTO field-name gotchas" list in the verified-protocol-facts section: `nextPageTokens` plural in request vs `nextPageToken` singular in response, `lastModify` in ScheduleSortDTO without trailing `d`, task IDs are String in requests / Long in responses), `internal/spcserver/dedup/dedup.go` (in-memory ResubmitCheck — sync.Map keyed by `userId+endpoint+SHA256(payload)`; **default TTL 1 second per the `@ResubmitCheck` annotation default**; per-endpoint overrides driven by what the cfr-decrypted method-level annotations specify; background GC ticker), `internal/spcserver/handlers/schedule.go`, `internal/spcserver/mapping/task.go` (`taskdb.Task` ↔ SPC `f_schedule_task` DTO; reuse field-mapping helpers from `internal/taskstore/`)
- Wire Engine.IO `TASK-SYN` / `STARTSYNC` emission on task writes (from device, from web UI, from CalDAV).
- Endpoints (all under `/api/file/schedule/`): `POST /group/all`, `POST /group`, `PUT /group`, `DELETE /group/{taskListId}`, `POST /group/clear`, `GET /group/{taskListId}`, `POST /task/all` (paginated 20/page), `POST /task`, `PUT /task`, `PUT /task/list`, `DELETE /task/{taskId}`, `GET /task/{taskId}`, `POST /sort`, `PUT /sort`, `DELETE /sort/{taskListId}`. Plus `POST /api/file/query/schedule/sort`, `POST /api/user/query/token`, `POST /api/user/logout`.
- **Acceptance:** Real device with `UB_SPC_MODE=server`:
  1. Sees its task lists in its task UI.
  2. Device edit → web UI reflects within 5s.
  3. Web UI edit → device receives Engine.IO STARTSYNC and pulls within 5s.
  4. Flipping `UB_SPC_MODE=client` and pointing DNS back at real SPC: everything works as before.

### Exit state (top-level Phase 1)
- Build green: `go build -C /home/sysop/src/ultrabridge ./...`
- Test green: `go test -C /home/sysop/src/ultrabridge ./internal/spcserver/...`
- Full end-to-end task sync working in `server` mode; regression-safe in `client` mode.
- `internal/tasksync/supernote/` and `internal/sync/notifier.go` untouched.

### Files this phase touches
**Created**: everything under `internal/spcserver/` listed in 1a–1d; `docs/implementation-plans/spc-phase-1.md`
**Modified**: `cmd/ultrabridge/main.go`, `internal/appconfig/keys.go`
**Not touched**: anything in `internal/tasksync/`, `internal/sync/`, `internal/processor/`, `internal/db/`, or any other existing package

### Compact/clear checkpoint
Merge Phase 1 to main. `/clear` before starting Phase 2.

---

## Phase 2 — File listing + capacity (read path)

**Goal:** Device can browse the cloud in its file UI, read-only.

### Entry state
- Phase 1 exit state holds: `internal/spcserver/` exists, exposes task endpoints, has Engine.IO task channel on `:18073` and file channel on `:18072` (file channel accepts connections only), JWT middleware works.
- No file-listing endpoints implemented.

### Context to load on a cold session
- This design plan, Phase 2 section
- `docs/spc-protocol.md` (file-listing endpoints, DTOs)
- `internal/spcserver/CLAUDE.md`
- `internal/notestore/CLAUDE.md`, `internal/notedb/CLAUDE.md`
- `internal/spcserver/server.go` and `internal/spcserver/handlers/` — to understand existing handler patterns

### Deliverables

**First task:** write `docs/implementation-plans/spc-phase-2.md`.

**New code:**
- `internal/spcserver/handlers/files.go` — sync session + list_folder + capacity + create_folder handlers
- `internal/spcserver/mapping/file.go` — filesystem entry ↔ `f_user_file` DTO
- `internal/spcserver/capacity.go` — `du`-style sum with a 60s cache
- Maybe: `md5` column on `notedb.notes` if not present (verify by reading `internal/notedb/schema.go` first; add via migration if needed)

**Endpoints:**
- `POST /api/file/2/files/synchronous/start`, `synchronous/end`
- `POST /api/file/2/files/list_folder`, `POST /api/file/3/files/list_folder_v3`
- `POST /api/file/capacity/query`, `POST /api/file/2/users/get_space_usage`
- `POST /api/file/2/files/create_folder_v2`
- `POST /api/file/2/files/query/deleteApi` (file-by-id)

**Config:**
- `UB_SPC_QUOTA_BYTES` (default 1 TiB; fake total capacity number)

### Exit state
- Build/test green.
- Real device's cloud-files UI shows existing notes from both Supernote NotesPath and Boox NotesPath.
- Folder navigation through nested directories works.
- Capacity meter shows non-zero used and configured total.
- No writes from device yet.

### Acceptance test
- Device opens cloud-files UI → directory tree loads.
- Device navigates into a nested folder → list updates.
- Device's storage meter shows reasonable values.
- Web UI continues to work; existing OCR pipeline continues to work.

### Files this phase touches
**Created**: `internal/spcserver/handlers/files.go`, `internal/spcserver/mapping/file.go`, `internal/spcserver/capacity.go`, `docs/implementation-plans/spc-phase-2.md`
**Modified**: `internal/spcserver/server.go` (new route registration); possibly `internal/notedb/schema.go` (add `md5` column if missing) and migration code
**Not touched**: tasksync/sync/db packages

### Compact/clear checkpoint
Merge Phase 2. `/clear` before Phase 3.

---

## Phase 3 — Files: download (cloud → device)

**Goal:** Device can download files from UB via SPC's presigned-URL OSS protocol.

### Entry state
- Phase 2 exit state holds: file-listing endpoints work; device can see files but not download them.
- OSS HMAC scheme is fully specified in `docs/spc-protocol.md` from Phase 0d.
- No OSS endpoints implemented.

### Context to load on a cold session
- This design plan, Phase 3 section
- `docs/spc-protocol.md` — especially the OSS HMAC section
- `internal/spcserver/handlers/files.go` (Phase 2 code) for handler patterns
- `internal/notestore/` — `GetByPath()` and file-stream APIs

### Sub-phases

**3a — OSS HMAC sign/verify primitive (unit-testable, no handlers)**
- New code: `internal/spcserver/oss/sign.go` implementing the algorithm specified in `docs/spc-protocol.md`. Server secret: `UB_SPC_OSS_SECRET`, auto-generated on first boot and persisted in settings.
- Comprehensive unit tests: round-trip sign/verify, expired-timestamp rejection, tampered-payload rejection, nonce-replay rejection (if Phase 0d shows nonces are tracked).
- Config keys added: `UB_SPC_OSS_SECRET`
- **Acceptance:** `go test ./internal/spcserver/oss/...` passes. Signed URL with known fixed inputs produces the exact byte sequence specified in `docs/spc-protocol.md` (golden-master test).

**3b — Download handlers**
- New code: `internal/spcserver/oss/download.go` (verify signature/timestamp/nonce, stream file from `notestore.GetByPath()`), `internal/spcserver/handlers/download.go` (`POST /api/file/3/files/download_v3` returns a presigned URL pointing at UB itself; `POST /api/oss/generate/download/url` same; `GET /api/oss/download` streams the bytes)
- **Acceptance:** Real device pulls a known .note file from UB; round-trip md5 matches the file on disk. Expired or tampered presigned URL returns the SPC error code the device expects (per `docs/spc-protocol.md`). Concurrent downloads of different files succeed without deadlock.

### Exit state (top-level Phase 3)
- Build/test green.
- Device can browse AND download files. Upload still device-blocked (no endpoints).
- HMAC primitive is reusable by Phase 4.

### Files this phase touches
**Created**: `internal/spcserver/oss/sign.go`, `internal/spcserver/oss/download.go`, `internal/spcserver/handlers/download.go`, `docs/implementation-plans/spc-phase-3.md`
**Modified**: `internal/spcserver/server.go` (route registration), `internal/appconfig/keys.go` (`UB_SPC_OSS_SECRET`)
**Not touched**: tasksync/sync/db/processor packages

### Compact/clear checkpoint
Merge Phase 3. `/clear` before Phase 4.

---

## Phase 4 — Files: upload (device → cloud) + mutations [COMPLETE 2026-05-25]

**Goal:** Device can upload modified files to UB, and delete / move / copy them.

> **As-built note (2026-05-25).** Phase 4 shipped **purely additive** — the MariaDB catalog cutover this section originally bundled was **removed from Phase 4 and deferred to Phase 5b** (coexistence principle: don't tear down the legacy SPC integration until the whole stack is built + soaked). The authoritative as-built record is `docs/implementation-plans/spc-phase-4.md`. Hardware-validated: upload (byte-exact + OCR'd + searchable), rename/move (id-stable), copy (fresh id), delete (soft-delete to `.recycle/`). Three wire bugs the decompiled annotations got wrong were found+fixed (see `docs/spc-protocol.md` §8). UB runs on its OWN `UB_SPC_FILE_ROOT`, not the real SPC's data dir.

This was the highest-risk and largest-scope phase, subdivided into independently-mergeable sub-phases.

### Entry state
- Phase 3 exit state holds: download path works; OSS HMAC primitive proven.
- Phase 0d OSS HMAC scheme is fully decoded (this phase cannot start otherwise).
- `internal/processor/catalog.go` still exists and writes through to MariaDB.
- `internal/db/` still exists and holds the MariaDB pool.

### Context to load on a cold session
- This design plan, Phase 4 section
- `docs/spc-protocol.md` — OSS upload flow (apply → upload → finish)
- `internal/spcserver/oss/sign.go` (Phase 3 code) — HMAC reuse
- `internal/processor/CLAUDE.md`, `internal/processor/catalog.go`, `internal/processor/worker.go` — the MariaDB write-through to be removed
- `internal/notedb/schema.go` — schema for the new `spc_uploads` staging table
- `internal/notestore/` — file write APIs

### Sub-phases

**4a — Staging infrastructure (pure infra, no endpoints)**
- New code: notedb schema migration adding `spc_uploads` table (innerName, target path, claimed md5, claimed size, presigned URL TTL, status), `internal/spcserver/staging/staging.go` (atomic-rename helper from `<NotesPath>/.staging/<innerName>` to target path; md5/size verification; orphan-cleanup goroutine)
- Unit tests: atomic-rename across temp + real paths, md5/size mismatch rejection, concurrent stage-and-rename safety.
- **Acceptance:** `go test ./internal/spcserver/staging/...` passes. New migration applies cleanly to a fresh notedb.

**4b — Upload endpoints (happy path)**
- New code: `internal/spcserver/oss/upload.go` (verify HMAC; stream body to `<NotesPath>/.staging/<innerName>`), `internal/spcserver/handlers/upload.go` (apply + finish handlers on top of 4a's staging helper; fire Engine.IO `FILE-SYN` on finish; kick `internal/processor` to OCR/index)
- Endpoints: `POST /api/file/3/files/upload/apply`, `POST /api/oss/generate/upload/url`, `POST /api/oss/upload`, `POST /api/file/2/files/upload/finish`
- **Acceptance:** Real device uploads a new .note → file lands in NotesPath → OCR pipeline runs → text searchable in web UI within ~30s. Round-trip md5 verified by both device and server.

**4c — Mutation endpoints (delete / move / copy)**
- New code: `internal/spcserver/handlers/mutation.go`
- Endpoints:
  - `POST /api/file/3/files/delete_folder_v3` — soft-delete: move to `<NotesPath>/.recycle/<timestamp>/<originalPath>` and set `recycled_at` column. Recycle-CRUD endpoints come in Phase 5.
  - `POST /api/file/3/files/move_v3` — atomic rename within the tree
  - `POST /api/file/3/files/copy_v3` — disk copy + new notedb entry
- Schema: add `recycled_at` column to `notedb.notes` if not present.
- **Acceptance:** Real device delete → file moves to `.recycle/`, disappears from list_folder, web UI no longer lists it. Move/rename → file at new path, notedb row updated. Copy → both copies present and OCR'd.

**4d — [MOVED TO PHASE 5b] Catalog cutover** — NOT done in Phase 4. Deferred per the coexistence principle (keep the legacy integration intact until the full stack is soaked). Phase 4d as-built is instead the additive OCR-kick on uploads (`pipeline.Enqueue`). The cutover spec below is retained for Phase 5b:
- Delete: `internal/processor/catalog.go`, `internal/processor/catalog_test.go`
- Delete: `processor.WorkerConfig.CatalogUpdater` field
- Modify `internal/processor/worker.go`: remove `catalogUpdater.AfterInject()` call. After OCR injection, the worker updates `notedb.notes.md5` and `size` to reflect the post-injection file directly.
- Modify `cmd/ultrabridge/main.go`: stop passing MariaDB into the processor. The MariaDB pool itself remains because `internal/tasksync/supernote/` still uses it; Phase 5 deletes it.
- **Acceptance:** `grep -r "catalog.go\|CatalogUpdater\|AfterInject" internal/processor/` returns nothing. `go vet` and `go test` clean. Existing OCR pipeline behavior identical to before (no MariaDB calls observed in logs while OCR runs).

### Exit state (top-level Phase 4) — as built
- Build/test green; hardware-validated.
- Device fully creates, downloads, deletes, moves, copies files against UB; uploads OCR'd + searchable.
- **Purely additive**: the MariaDB catalog write-through and all legacy SPC-client code remain intact (cutover deferred to Phase 5b). Real-SPC flip-back is a working escape hatch.
- UB runs on its own dedicated `UB_SPC_FILE_ROOT` (not the real SPC's data dir).

### Files this phase touches — as built
**Created**: `internal/spcserver/staging/{staging.go,store.go}`, `internal/spcserver/handlers/{upload.go,mutation.go}` (+ tests), `docs/implementation-plans/spc-phase-4.md` (the `oss` upload signing reused Phase 3's `oss/sign.go`, no separate `upload.go`)
**Modified**: `internal/spcserver/server.go`, `internal/spcserver/notify/notifier.go`, `internal/spcserver/fileids/fileids.go`, `internal/spcserver/dto/file.go`, `internal/pipeline/pipeline.go`, `cmd/ultrabridge/main.go`, `docs/spc-protocol.md`
**Deleted**: nothing (coexistence — cutover is Phase 5b)

### Compact/clear checkpoint
Phase 4 merged + hardware-validated 2026-05-25; NPM flipped back to real SPC (no permanent DNS cutover yet). `/clear` before Phase 5 (verification + gated teardown).

---

## Phase 5 — Verification + gated deprecation (re-scoped 2026-05-25)

**Goal:** Confirm UB-as-SPC is feature-complete for the device, then — only after a real soak — remove the legacy SPC-client code so the Supernote container can be shut down. **The recycle-browse/restore and file-search features originally planned here are OUT OF SCOPE** (see below).

> **Re-scope note (2026-05-25, after the Phase 4 hardware session).** Inspecting the live SPC stack (`mariadb`/`supernotedb` + decompiled source) established that **UB-as-SPC is functionally feature-complete for the device as of Phase 4**:
> - The cloud recycle bin is the `f_recycle_file` table. The **device's** only interaction with it is **delete-into-recycle** (`delete_folder_v3` → `FileLocalUtil.processSingleFileDeletion`), which UB already implements (Phase 4 soft-delete to `.recycle/`, hardware-validated).
> - Recycle **list/clear/delete/revert** and **file search** live ONLY on `F_FileLocalWebController` / `F_FileSearchController` — the **Partner-app/web** surface, which the device never hits (0b capture §11).
> - **Partner-app support is a real future goal but explicitly out of scope for this project** (user decision 2026-05-25). So the former 5a (recycle endpoints) and 5b (file search) are **dropped** — they serve a client we are not building for. They would belong to a separate "Partner/web surface" project (its own login flow, `F_FileController`/`F_FileV2Controller`/`F_ShareController` families, sharing semantics).

### Entry state
- Phase 4 exit state holds: device fully creates / downloads / renames / moves / copies / deletes files against UB; uploads are OCR'd and searchable; UB runs on its own dedicated `UB_SPC_FILE_ROOT` (NOT the real SPC's data dir).
- **Phase 4 was purely additive** — the legacy SPC integration is fully intact: `internal/processor/catalog.go` + its MariaDB catalog write-through, `internal/tasksync/supernote/`, `internal/sync/`, `internal/db/` all still present and working. Real-SPC flip-back remains a working escape hatch.
- Device has been running against UB for ≥1 verification week with no regressions **before any teardown begins**.

### Context to load on a cold session
- This design plan, Phase 5 section; `docs/spc-protocol.md`
- `memory/project_spc_phase5_watchlist`, `project_spc_coexistence_no_premature_cutover`, `project_spc_dedicated_file_tree`, `project_spc_phase4_upload`
- `internal/processor/CLAUDE.md`, `internal/processor/catalog.go`, `worker.go` (catalog cutover scope)
- `internal/sync/CLAUDE.md`, `internal/tasksync/supernote/CLAUDE.md`, `internal/db/` (deletion scope)

### Sub-phases

**5a — Verification sweep (no new features)**
- Capture a fresh full device session (sync, upload, download, rename, move, copy, delete) against UB and confirm **no endpoint the device hits is unimplemented** (no unexpected 404/5xx in the NPM/tcpdump logs). The 0b set says we are complete; this is the soak-period confirmation.
- **Acceptance:** a clean device session shows zero unhandled device-hit endpoints over ≥1 week.

**5b — Catalog cutover (deferred from Phase 4; gated on 5a + soak)**
- This is the teardown the coexistence principle deferred. Delete `internal/processor/catalog.go` + `catalog_test.go`; remove `WorkerConfig.CatalogUpdater` + the `AfterInject()` call; the worker updates `notedb.notes.md5`/`size` directly post-injection; `cmd/ultrabridge/main.go` stops passing MariaDB into the processor.
- **Acceptance:** `grep -r "catalog.go\|CatalogUpdater\|AfterInject" internal/processor/` empty; OCR behavior identical; no MariaDB calls during OCR.

**5c — Note rendering endpoints (CONDITIONAL; device does NOT hit these per 0b — likely skip)**
- Only if a capture ever shows the device hitting `POST /api/file/note/to/pdf|png`. Reuse `go-sn`/`internal/booxrender`. Default: skip.

**5d — Engine.IO DIGEST-SYN events (CONDITIONAL; only if device misbehaves without them)**
- Extend `internal/spcserver/socketio/server.go` to emit `DIGEST-SYN`. Default: skip (Phase 1–4 worked without it).

**5e — Big cleanup PR (deletes + config-key removals) — the final teardown, gated on the full soak**
- Delete: `internal/sync/notifier.go`, `notifier_test.go`, `internal/sync/CLAUDE.md`
- Delete: `internal/tasksync/supernote/` (entire subdirectory)
- Delete or simplify: `internal/tasksync/engine.go` (the two-store reconciliation logic; if no callers remain, delete; else simplify to drive only the new spcserver Notifier)
- Delete: `internal/db/` (entire subdirectory)
- Modify `cmd/ultrabridge/main.go`: remove `loadDBEnv()`, MariaDB connect, `ResolveUserID()` calls
- Remove config keys from `internal/appconfig/keys.go`: `UB_SN_API_URL`, `UB_SN_PASSWORD`, `UB_SN_ACCOUNT`, `UB_SN_SYNC_ENABLED`, `UB_SN_SYNC_INTERVAL`, `UB_SOCKETIO_URL`, `UB_DB_HOST`, `UB_DB_PORT`, `UB_SUPERNOTE_DBENV_PATH`, `UB_USER_ID`
- Remove dbenv file loading
- Delete: `tests/` MariaDB integration tests
- Modify `docker-compose.yml`: remove `supernote-service` and `mariadb` services
- Optionally: remove `UB_SPC_MODE` setting (server is the only mode), or keep it defaulting to `server` for one more release as a safety valve
- **Acceptance:** `grep -ri "mariadb\|UB_SN_\|tasksync/supernote\|sync/notifier" internal/ cmd/` returns nothing. `docker compose up` brings up only UB (no SPC container, no MariaDB). Web UI / CalDAV / MCP / Boox / RAG / chat all still work.

### Exit state (top-level Phase 5)
- UB is the cloud. The Supernote container can be shut down. CalDAV, web UI, MCP, RAG, Boox pipeline, Supernote OCR pipeline all carry over unchanged except the now-deleted MariaDB catalog write-through (5b).
- Recycle-browse/restore and file search are **not** present (Partner/web surface, out of scope) — the device never needs them.

### Files this phase touches
**Created**: `docs/implementation-plans/spc-phase-5.md`, conditionally `handlers/render.go` (5c, likely skipped)
**Modified**: `internal/processor/worker.go`/`processor.go` (catalog cutover), `internal/appconfig/keys.go`, `cmd/ultrabridge/main.go`, `docker-compose.yml`
**Deleted**: `internal/processor/catalog.go` + `catalog_test.go` (5b), `internal/sync/`, `internal/tasksync/supernote/`, `internal/db/`, MariaDB integration tests, possibly `internal/tasksync/engine.go` (5e)
**NOT created (dropped, Partner/web surface = out of scope)**: recycle endpoints, file-search endpoint

---

## Phase D: Digest support (first-class) — D1 DONE + hardware-validated 2026-05-25

The Supernote **Digest** feature (the SPC API calls it "summary") is a first-class
capability for this platform — a digest is a user-curated saved excerpt from a
notebook plus a handwritten `.mark` annotation, organized into groups with tags.
Split into three sub-phases (plan: `~/.claude/plans/okay-so-we-have-sunny-flame.md`):

**D1 — protocol round-trip: DONE + hardware-validated 2026-05-25.** The real
`F_SummaryController` surface (`add/update/delete summary` + `…/group` + `…/tag` +
`query/summary{,/hash,/id,/group}` + `.mark` `upload/apply/summary` +
`download/summary`) is implemented over `internal/digeststore` (the canonical store,
faithful to `t_summary`/`t_summary_tag`) via `internal/spcserver/handlers/summary.go`.
`.mark` blobs reuse the Phase 3/4 OSS signed-URL + staging path (`.digests/`). Purely
additive: when no `DigestStore` is wired the three query endpoints fall back to the
old empty-success stubs and the writes 404. Validated on the Nomad both directions
(push/pull/`.mark` byte-exact/delete/update); wire findings + the two fixes
(item-identity-in-metadata, `metadataMap` numeric preservation) and the
device-authoritative-delete semantic are in `spc-protocol.md §8` + memory
`project_spc_phaseD_digests`.

**D2 — UB-native surfacing (not built):**
- index digest `content` into FTS (`digest_content`/`digest_fts` mirroring `note_fts`)
- RAG-embed digest text (`rag.Embedder`/`EmbedStore`)
- a `DigestService` + `internal/web` Digests tab + `/api/v1/digests`

**D3 — proactive `DIGEST-SYN` push (capture-gated, not built):** Engine.IO `DIGEST-SYN`
over the `digest` socket event (already known — see CLAUDE.md socket gotchas), fired on
UB-side digest writes. Only if a capture shows the device needs it (it polls
`query/summary/hash` every sync, so round-trip works without it).

This is its own phase-sized effort. See memory `project_spc_no_analogue_features`
and `docs/future-work/spc-no-analogue-features.md`.

## Scope dropped (not implemented in any phase)

- **SPC Vue web UI (`/` → `:9888` in the SPC container).** Pure browser-facing, human-only. UB's existing `internal/web/` (Files / Tasks / Search / Chat / Settings / processor C&C / sync status tabs) is the human-facing replacement. The SPC-server listener UB exposes serves only `/api/*` and `/socket.io/*`; a bare browser visit to `supernote.broken.works/` post-cutover returns 404 (or NPM-level redirect to `ultrabridge.broken.works`).
- User registration, SMS login, valid codes, password reset, sensitive operations, email server config — single-user; user just edits config.
- Sharing (`F_ShareController`) — single-user, no peers.
- ~~Summary/tag (`F_SummaryController`) — UB's RAG/search supersedes.~~ **WRONG — corrected 2026-05-25.** "Summary" is the **Digest** feature, a FIRST-CLASS Supernote capability (user-curated saved excerpts + handwritten `.mark` annotations) — NOT superseded by RAG (that's on-demand retrieval; digests are intentional curated artifacts). It is **deferred, not dropped** — see "Future: Digest support" below. The Phase 1 `query/summary/{hash,group,id}` empty-success stub exists ONLY to keep task sync unblocked.
- Equipment activation/binding/warranty/manual — stub `bind/status` to "bound" in Phase 1a, 404 the rest.
- Reference/dictionary — empty 200 stubs if device hits them.
- File V2 query-by-path — only if device boots into it.
- FTP file upload (`/upload/ftp/...`) — SPC-internal, not device-facing.
- OSS multipart upload (`/api/oss/upload/part`) — only matters for >50MB files; defer until proven needed.
- Multi-user. UB stays single-user. If user has two devices, Engine.IO push routes by `equipmentNo` from JWT.
- Redis. Zero Redis. ResubmitCheck → in-memory map. Token cache → JWT verify is stateless.

---

## Risks and mitigations

| ID | Risk | Severity | Owning sub-phase | Mitigation |
|---|---|---|---|---|
| R1 | OSS HMAC signing scheme not decoded | High → **CLOSED** | 0d | **Fully decoded by CFR inspection 2026-05-15.** Algorithm is plain SHA-256 over `path + timestamp + nonce + (fileSize or "") + secret`, where secret is the literal string `K+5xFzxbnB1iSZWqmu3Etw==` (not base64-decoded). Path is base64url-no-pad of UTF-8 bytes. Upload window 30 min, download window 24 h, no nonce-replay tracking. See `docs/spc-protocol.md` §6. No mitmproxy fallback needed. |
| R2 | Device validates JWT with a different secret than the JAR's hardcoded value | Medium → **CLOSED** | 0c | **Closed 2026-05-22.** Live device (Supernote Nomad SN078C10034074) accepted a token we minted with `Constant.SECRET` + the right claim shape, made authenticated calls with it, and opened its Engine.IO socket. Device does no client-side JWT validation. See `docs/spc-protocol.md` §2. |
| R3 | Device pins TLS certs | Medium → **CLOSED** | 0b | **Closed pre-0b by deployment-topology walk 2026-05-15.** NPM serves a Let's Encrypt cert for `supernote.broken.works` and the device already accepts it. No pinning. |
| R4 | Engine.IO version mismatch (v3 vs v4) | Medium → **CLOSED** | 0b | **Closed pre-0b by access-log inspection 2026-05-15.** Wire shows `EIO=3&transport=websocket` matching netty-socketio 2.0.3. |
| R5 | SPC error codes (`E0330` etc.) trigger device-specific behaviors | Low–Medium → **CLOSED** | 0b | `E0330 = "NextSyncToken timeout"` confirmed in `FileErrorCodeEnum`; full enum set decompiled. Device session showed no error-driven misbehavior. |
| R6 | Path encoding (Unicode normalization) | Low → **CLOSED** | 0b | Observed: device emits **non-normalized paths with double slashes** (`Personal//IMG_…`). UB path handling must tolerate (noted in `docs/spc-protocol.md` §6/§11). |
| R7 | Multi-device push routing | Low | 1c | Engine.IO subscriptions keyed on `equipmentNo` from JWT. |
| R8 | `note/to/pdf` server-rendering endpoint required | Low → **resolved** | 5c | 0b showed the device does **not** hit `note/to/pdf` or `note/to/png` during normal sync. Phase 5c stays conditional/skippable. |
| R9 | Decompiled tree may be incomplete (controller exists in JAR but not in cfr/JADX output) | Meta | 0a | Endpoint enumeration from compiled mapping table. Soak-test against real device after each phase. |

---

## Persistent reference docs (built in Phase 0, read by all later phases)

- `docs/spc-protocol.md` — TLS posture, Engine.IO version, OSS HMAC scheme, JWT acceptance result, device-observed endpoint list, error-code mapping
- `internal/spcserver/CLAUDE.md` — package domain doc; conventions; pointers into `docs/spc-protocol.md`

## Critical files / paths (reference, not per-phase)

### Spec source (read-only)
- `/home/sysop/spc-rev/cfr-decrypted/` — primary spec ground truth (Phase 0a expands)
- `/home/sysop/spc-rev/decrypted-classes/` — raw .class files
- `/home/sysop/spc-rev/decompiled/` — JADX dump (structural only)
- `/home/sysop/spc-rev/supernote-service.jar` — original JAR

### Reused unchanged across all phases
- `internal/notedb`, `internal/notestore`, `internal/search`, `internal/rag`, `internal/chat`
- `internal/taskdb`, `internal/taskstore`
- `internal/caldav`, `internal/booxpipeline`, `internal/booxnote`, `internal/booxrender`, `internal/webdav`, `internal/pdfrender`
- `internal/web`, `internal/auth`, `internal/logging`, `internal/mcpauth`
- `cmd/ub-mcp/`

---

## What this plan is not

- Not a task-by-task implementation plan. Each top-level phase's first task is to write its own implementation plan at `docs/implementation-plans/spc-phase-N.md`, internally laid out by sub-phase.
- Not a commitment to timelines. Single engineer; sub-phases are sized in "ships when it ships" units.
- Not final on dropped scope. If Phase 0b device observation shows the firmware aggressively pings sharing/summary/equipment endpoints and degrades when they fail, we'll stub more aggressively rather than 404'ing.
