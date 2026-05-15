# Phase 0 — De-risking and persistent reference docs

**Phase goal:** Convert open unknowns about the Supernote Private Cloud (SPC) protocol into written facts that later phases can read cold. Produce two persistent reference docs (`docs/spc-protocol.md`, `internal/spcserver/CLAUDE.md`) that are sufficient on their own to start Phase 1 from a fresh session. **No production code merges in this phase.**

This plan is self-contained. Read this file plus the design plan and you should be able to execute Phase 0 from a cold `/clear`.

---

## Cold-start reading list

Read these before doing anything else:

1. `/home/sysop/.claude/plans/okay-so-we-have-sunny-flame.md` — the overall UB-as-SPC design plan (Phase 0 section + the "Deployment topology" section)
2. This file
3. Memory `project_spc_classfinal_decrypted.md` — JAR decryption procedure, locations of CFR/JADX/decrypted-classes tree
4. Memory `reference_supernote_service_internals.md` — SPC runtime layout, JAR location, JDK/Node versions
5. Memory `reference_spc_dev_topology.md` — how supernote.broken.works reaches the SPC container; required for 0b/0c procedures
6. Memory `project_notebridge_feasibility.md` — strategic context for why this refactor exists
7. Memory `project_spc_pagination.md` — concrete example of the kind of SPC behavior the decompile exists to clarify

You should not need any other memory or any conversation history.

---

## Entry state (verifiable)

- `/home/sysop/spc-rev/cfr-decrypted/` contains exactly one `.java` file: `com/ratta/file/service/impl/ScheduleServiceImpl.java`.
- `/home/sysop/spc-rev/decrypted-classes/` contains 437 protected classes (`find … -name '*.class' | wc -l` ≈ 437).
- `/home/sysop/spc-rev/supernote-service.jar` is the original 72MB fat-JAR; **unprotected** classes (`Constant`, `BaseVO`, `ResubmitCheck`, `ResubmitCheckAspect`, etc.) live only inside this JAR, not under `decrypted-classes/`.
- `/home/sysop/spc-rev/cfr-0.152.jar` is the CFR decompiler.
- `/home/sysop/spc-rev/extracted/BOOT-INF/classes/` contains stub bodies for the protected classes (do not use for spec; use `decrypted-classes/`).
- `internal/spcserver/` does not exist in the UB repo.
- `docs/spc-protocol.md` does not exist.

If any of those are not true, stop and reconcile before proceeding.

---

## Working artifacts produced by this phase

- `docs/spc-protocol.md` — protocol facts: envelope shape, JWT, Engine.IO events/ports, OSS HMAC, error codes, observed device endpoint list, TLS posture
- `internal/spcserver/CLAUDE.md` — placeholder domain doc for the spcserver package; points at `docs/spc-protocol.md`
- `/home/sysop/spc-rev/cfr-decrypted/**` — expanded with every class listed in 0a
- `/home/sysop/spc-rev/cfr-decrypted/_endpoints.txt` — raw endpoint dump from `strings`

The UB git working tree changes are only the two `.md` files plus the parent `internal/spcserver/` directory.

---

## Sub-phases

### 0a — CFR decompile + endpoint enumeration

**Purpose:** Make every class the design plan references actually readable. Produce the raw endpoint list.

**Targets** (all under `com.ratta`):

| FQN | Source | Why |
|---|---|---|
| `constants.Constant` | JAR (unprotected) | `SECRET`, `JWT_SECRET`, TTLs, header names |
| `vo.BaseVO` | JAR (unprotected) | Envelope shape `{success, errorCode, errorMsg}` |
| `check.ResubmitCheck` | JAR (unprotected) | Annotation declaration |
| `check.ResubmitCheckAspect` | JAR (unprotected) | Defaults: `interval=1`, `timeUnit=SECONDS` |
| `user.dto.LoginDTO` | decrypted-classes | Login request shape |
| `user.vo.LoginVO` | decrypted-classes | Login response shape (`token` field) |
| `user.info.JwtTokenUserUtil` | decrypted-classes | Issue/verify; confirm `secret = Constant.SECRET` |
| `controller.U_LoginController` | decrypted-classes | Login endpoint wiring |
| `controller.F_ScheduleController` | decrypted-classes | Task endpoints (Phase 1d) |
| `controller.F_FileLocalController` | decrypted-classes | File list/download/upload endpoints (Phases 2–4) |
| `file.service.impl.FileLocalServiceImpl` | decrypted-classes | File handler bodies |
| `controller.O_OssLocalController` | decrypted-classes | OSS presigned URL endpoints (Phases 3–4) |
| `oss.local.LocalFileUtil` | decrypted-classes | **OSS HMAC algorithm — blocks Phase 4** |
| `controller.E_EquipmentController` | decrypted-classes | Equipment binding flow |
| `controller.F_TerminalFileUploadController` | decrypted-classes | Terminal upload variants (Phase 4) |
| `controller.F_FileV2Controller` | decrypted-classes | v2 file endpoints (path query) |
| `controller.F_FileUploadController` | decrypted-classes | Non-terminal upload variant |
| `controller.F_FileSearchController` | decrypted-classes | File search endpoint (Phase 5b) |
| `socket.io.SocketIoConstant` | decrypted-classes | Event names, msgTypes, ops |
| `socket.io.SocketIoConfiguration` | decrypted-classes | Port wiring, namespace registration |
| `socket.io.SocketIOEventHandler` | decrypted-classes | File channel handler |
| `socket.io.SocketIOTaskEventHandler` | decrypted-classes | Task channel handler |
| `socket.io.SetocketIODigestEventHandler` | decrypted-classes | Digest channel handler (sic, that's the real filename) |
| `socket.io.JwtTokenUtil` | decrypted-classes | Socket-side JWT verify (cross-check with UserUtil) |
| `socket.io.SignVerifierSocketIO` | decrypted-classes | Signature verification on socket events |
| `file.dto.ScheduleTaskDTO` | decrypted-classes | `nextPageTokens` (plural) request field |
| `file.vo.ScheduleTaskAllVO` | decrypted-classes | `nextPageToken` (singular) response field |
| `file.dto.ScheduleSortDTO` | decrypted-classes | `lastModify` (no 'd') quirk |
| `file.dto.FileUploadFinishLocalDTO` | decrypted-classes | `content_hash` snake_case quirk |
| `enum.FileErrorCodeEnum` | decrypted-classes | `E0330` and the full error set |

**Commands** (run from `/home/sysop/spc-rev/`):

For **unprotected** classes (Constant, BaseVO, ResubmitCheck, ResubmitCheckAspect) — CFR reads the JAR directly:
```bash
java -jar cfr-0.152.jar supernote-service.jar com.ratta.constants.Constant --outputdir cfr-decrypted
java -jar cfr-0.152.jar supernote-service.jar com.ratta.vo.BaseVO --outputdir cfr-decrypted
java -jar cfr-0.152.jar supernote-service.jar com.ratta.check.ResubmitCheck --outputdir cfr-decrypted
java -jar cfr-0.152.jar supernote-service.jar com.ratta.check.ResubmitCheckAspect --outputdir cfr-decrypted
```

For **decrypted** classes — CFR reads from the decrypted-classes tree but needs the JAR on the extraclasspath for unprotected dependencies:
```bash
CFR="java -jar cfr-0.152.jar"
DC=/home/sysop/spc-rev/decrypted-classes
JAR=/home/sysop/spc-rev/supernote-service.jar
OUT=/home/sysop/spc-rev/cfr-decrypted

$CFR $DC/com/ratta/user/dto/LoginDTO.class --extraclasspath $JAR:$DC --outputdir $OUT
$CFR $DC/com/ratta/user/vo/LoginVO.class --extraclasspath $JAR:$DC --outputdir $OUT
$CFR $DC/com/ratta/user/info/JwtTokenUserUtil.class --extraclasspath $JAR:$DC --outputdir $OUT
$CFR $DC/com/ratta/controller/U_LoginController.class --extraclasspath $JAR:$DC --outputdir $OUT
$CFR $DC/com/ratta/controller/F_ScheduleController.class --extraclasspath $JAR:$DC --outputdir $OUT
$CFR $DC/com/ratta/controller/F_FileLocalController.class --extraclasspath $JAR:$DC --outputdir $OUT
$CFR $DC/com/ratta/file/service/impl/FileLocalServiceImpl.class --extraclasspath $JAR:$DC --outputdir $OUT
$CFR $DC/com/ratta/controller/O_OssLocalController.class --extraclasspath $JAR:$DC --outputdir $OUT
$CFR $DC/com/ratta/oss/local/LocalFileUtil.class --extraclasspath $JAR:$DC --outputdir $OUT
$CFR $DC/com/ratta/controller/E_EquipmentController.class --extraclasspath $JAR:$DC --outputdir $OUT
$CFR $DC/com/ratta/controller/F_TerminalFileUploadController.class --extraclasspath $JAR:$DC --outputdir $OUT
$CFR $DC/com/ratta/controller/F_FileV2Controller.class --extraclasspath $JAR:$DC --outputdir $OUT
$CFR $DC/com/ratta/controller/F_FileUploadController.class --extraclasspath $JAR:$DC --outputdir $OUT
$CFR $DC/com/ratta/controller/F_FileSearchController.class --extraclasspath $JAR:$DC --outputdir $OUT
# socket.io family — decompile all 22 classes in one shot:
for f in $DC/com/ratta/socket/io/*.class; do
  $CFR "$f" --extraclasspath $JAR:$DC --outputdir $OUT
done
# DTOs/VOs/enums:
$CFR $DC/com/ratta/file/dto/ScheduleTaskDTO.class --extraclasspath $JAR:$DC --outputdir $OUT
$CFR $DC/com/ratta/file/vo/ScheduleTaskAllVO.class --extraclasspath $JAR:$DC --outputdir $OUT
$CFR $DC/com/ratta/file/dto/ScheduleSortDTO.class --extraclasspath $JAR:$DC --outputdir $OUT
$CFR $DC/com/ratta/file/dto/FileUploadFinishLocalDTO.class --extraclasspath $JAR:$DC --outputdir $OUT
# Find FileErrorCodeEnum (location varies; grep first):
find $DC -name 'FileErrorCodeEnum.class'
# then $CFR that path
```

If any class file isn't where the table claims: `find /home/sysop/spc-rev/decrypted-classes /home/sysop/spc-rev/extracted -name '<Name>.class'`. The `decrypted-classes` tree puts everything under the package directory matching the FQN; if it's missing there, check the JAR via `unzip -l supernote-service.jar | grep <Name>`.

**Endpoint enumeration:**
```bash
cd /home/sysop/spc-rev
unzip -p supernote-service.jar 'BOOT-INF/classes/com/ratta/controller/*.class' | strings | grep -E '^/api/' | sort -u > cfr-decrypted/_endpoints.txt
wc -l cfr-decrypted/_endpoints.txt   # expect many dozens
```

**Acceptance for 0a:**
1. Every class in the target table has a corresponding `.java` file in `cfr-decrypted/` with **real method bodies** (open `JwtTokenUserUtil.java` and verify the `createToken`/`parseToken` methods are not stubs).
2. `cfr-decrypted/_endpoints.txt` exists and contains the SPC URL surface.
3. Draft a "Verified facts" section in `docs/spc-protocol.md` capturing what 0a confirmed (envelope shape, JWT secret identity, Engine.IO event names and ports, ResubmitCheck default, error codes, DTO field-name gotchas). Cite each fact with `<FQN.java>:<line>` so future-you can re-verify.

---

### 0b — Device boot-trace (in-place capture on the cleartext leg)

**Purpose:** Capture what the device actually does on the wire — endpoint URLs, payloads, Engine.IO frame content, error-code reactions, path encoding, which upload variant the device prefers.

**R3 (TLS pinning) is already closed in this dev install.** NPM serves a Let's Encrypt cert for `supernote.broken.works` and the device accepts it. No cert install on the device is needed. See memory `reference_spc_dev_topology.md` for the layout.

**Capture approach** — pick A or B (B is preferred):

**Approach A — nginx body-logging flip (lightest touch, REST only):**
1. The SPC container's internal nginx already JSON-logs `/api/*` to `/mnt/supernote/sndata/logs/web/access.log` (host-readable on neptune). The `req_body` field is in the log format but empty because `proxy_request_buffering off` is global.
2. Flip it for the capture window: `docker exec supernote-service` → edit `/etc/nginx/nginx.conf` to set `proxy_request_buffering on;` inside `location ^~ /api/`, then `nginx -s reload`. Revert after capture.
3. Drive a full session on the device (see Capture targets below).
4. Pull access log, sanitize, commit to `docs/spc-protocol.md`.

**Limitations of A:** captures REST only. Engine.IO frames are not in nginx logs past the upgrade handshake (`status:101`).

**Approach B — Go tap proxy on the cleartext leg (captures REST + Engine.IO frames):**
1. Write a small Go reverse proxy (`/home/sysop/spc-rev/tap/main.go`, ~60 LOC) that listens on a temporary port (e.g. neptune:19082) and proxies to `127.0.0.1:19072`. Logs every HTTP request/response body and every WebSocket frame in both directions to a JSON file.
2. Temporarily flip the NPM upstream for `supernote.broken.works` from `192.168.9.52:19072` to `192.168.9.52:19082`. NPM config lives at `/data/nginx/proxy_host/13.conf` inside the `sysop-app-1` container on hydrae (`ssh 192.168.9.30`). Edit via NPM admin UI or directly + `docker exec sysop-app-1 nginx -s reload`.
3. Drive a full session on the device.
4. Flip NPM upstream back. Sanitize and commit.

**Approach B preferred** because the same tap code is reusable as a Phase 1c Engine.IO inspector when validating UB's server matches SPC's frame-for-frame.

**Capture targets** — one full session containing each of:
- Cold boot login flow.
- Task creation/edit on device → sync.
- File creation on device (write a single .note page, sync).
- File browse via cloud-files UI (one folder navigation).
- File download from cloud to device (any small note).
- File delete from device.
- Idle period ≥30 minutes (to observe `ratta_ping` / Engine.IO keepalive cadence).

**Document in `docs/spc-protocol.md`:**
- Engine.IO confirmation: access log already confirms `EIO=3&transport=websocket`. Capture an initial handshake to record any additional query params the device sends.
- Socket.IO namespace usage: does the device open one socket.io connection or multiple? Does it ever try to reach a separate task-channel URL (would imply :18073 should be exposed in some configurations)? **Load-bearing for Phase 1c.**
- Path encoding: NFC vs NFD for filenames with combining accents; URL-encoded Chinese filenames byte-for-byte.
- Full ordered list of endpoint URLs hit by the device, with method + sample payload sketch (sanitize any credentials before committing).
- Which upload variant the device uses: `/api/file/3/files/upload/apply` + `.../finish`, or `/api/file/terminal/upload/apply` + `.../finish`. **Load-bearing for Phase 4b.**
- Whether device hits `/api/file/note/to/pdf` or `/api/file/note/to/png` during normal use (load-bearing for Phase 5c).
- Whether device hits sharing/summary/equipment endpoints during normal sync (informs Phase 1a stubs).
- Error-code behavior: when does the device send `nextSyncToken`? What happens on E0330 reply — full re-pull?

**Acceptance for 0b:**
- `docs/spc-protocol.md` has a "Device wire observations" section that covers all of the above.
- Risks R4 (EIO version), R5 (error code mapping), R6 (path encoding) are closed: each row in the design plan's risk table can be flipped from "still open" to "closed" with a sentence citing the observation. R3 was closed pre-0b by the deployment-topology walk (Let's Encrypt cert accepted by device).

---

### 0c — JWT acceptance test

**Purpose:** Confirm that a JWT signed with `Constant.SECRET` (the ~280-char string surfaced in 0a) is accepted by an unmodified device. If not, this phase generates a new sub-phase (extract real secret from device firmware) before Phase 1 can proceed.

**Setup:**
- Tiny standalone HTTP server (Go, throwaway under `/home/sysop/spc-rev/jwt-test/`), listening on neptune:9090.
- Serves `POST /api/official/user/account/login/equipment` (and whatever other path 0b showed is the device's actual login endpoint).
- Returns a `LoginVO`-shaped JSON envelope: `{"success":true,"errorCode":"","errorMsg":"","token":"<JWT>","userId":1,"userInfo":{...minimum fields…}}`. Exact fields confirmed by reading `LoginVO.java` from 0a.
- The JWT is HS256-signed with the literal `Constant.SECRET` string as bytes (UTF-8). Claims include the fields `JwtTokenUserUtil` populates (verify from 0a: typically `userId`, `account`, `equipmentNo`, `iat`, `exp`). Use `JWT_TTL_8 = 28800000` (8h) for terminal flow.
- Stub a couple of follow-up endpoints (`/api/equipment/bind/status`, `/api/file/query/server`) to return success, so the device proceeds past login long enough to demonstrate the JWT was accepted.

**NPM front-door procedure (preferred — no DNS gymnastics, no cert install):**
1. In NPM admin UI on hydrae, create a new proxy host: `supernote-test.broken.works`, upstream `192.168.9.52:9090`, request a Let's Encrypt cert (must be a hostname that resolves publicly for ACME). Or if a wildcard cert is in place, just add the hostname.
2. On the device's WiFi, set DNS to a local resolver (router or dnsmasq) that returns `192.168.9.30` for `supernote-test.broken.works`. (Or just set a hosts override if the device permits it — most do not, so plan on a router-level override.)
3. Either: temporarily flip the device's `cloud.host` setting if accessible, OR test by manually pointing the device at `supernote-test.broken.works` if the firmware supports it. If `cloud.host` is baked in and not user-configurable, fall back to the **drop-in flip** below.

**Drop-in flip alternative (if `cloud.host` is fixed):**
1. Stop the SPC container (`docker compose -f /mnt/supernote/docker-compose.yml stop supernote-service`) to free port 19072. SPC is unreachable during the test — schedule a short window.
2. Run the stub Go server bound to neptune:19072 directly (matching the NPM upstream the device already knows).
3. Device reboots, attempts login, hits our stub through `supernote.broken.works`.
4. Restore SPC after the test: `docker compose -f /mnt/supernote/docker-compose.yml start supernote-service`.

**Procedure:**
1. Bring up the stub (either via NPM front-door or drop-in flip).
2. Reboot the device.
3. Observe: does the device proceed past login (hits a follow-up endpoint with `x-access-token: <our JWT>`), or reject?
4. If it sends the token but a follow-up rejects: the JWT was accepted by the device but our stub was wrong about something else — note that in `docs/spc-protocol.md`.
5. Tear down. Verify real SPC is reachable again.

**Acceptance for 0c:**
- `docs/spc-protocol.md` has a "JWT acceptance" section with the binary verdict: accepted (with claim shape) or rejected.
- Risk R2 closed.
- If rejected: add a follow-up sub-phase 0f to the design plan (extract real signing material from device firmware) and stop. Phase 1 cannot start.

---

### 0d — Decode OSS HMAC signing scheme

**Purpose:** Specify the algorithm well enough that Phase 3a can implement and unit-test it from this doc alone. **Blocks all of Phase 4.**

**Primary approach — CFR reading:**
After 0a runs, `cfr-decrypted/com/ratta/oss/local/LocalFileUtil.java` and `cfr-decrypted/com/ratta/controller/O_OssLocalController.java` will be readable. The signing function will be inside one of them (or in a referenced helper — follow imports).

Document in `docs/spc-protocol.md`:
- Inputs: what fields go into the signed string? Path? Timestamp? Nonce? UserId? EquipmentNo? Method (GET/PUT)? Headers? Body hash?
- Concatenation order and separator(s) — match byte-for-byte.
- Hash function: HMAC-SHA1? HMAC-SHA256? Plain SHA256(key + payload)?
- Output encoding: hex (lowercase? uppercase?), base64 (standard? URL-safe? padded?).
- Where does the signature appear: query string parameter (name?), or `Authorization` header?
- TTL: how is expiry enforced? `Expires=<unix-ts>` query param? Server-side issued+stored?
- Nonce/replay: are nonces tracked server-side, or does pure timestamp suffice?

**Fallback — mitmproxy capture from 0b:**
If CFR output for `LocalFileUtil` has any decompile gaps (heavy AspectJ weaving, Lombok-generated bits): take a real signed download URL from 0b's session, plus all observed inputs (userId, file id, timestamp, etc.), and reverse-engineer the signing function from known inputs/outputs. Brute-force candidate algorithms (HMAC-SHA1, HMAC-SHA256 with various concat orderings) until one matches.

**Acceptance for 0d:**
- `docs/spc-protocol.md` "OSS HMAC" section has a fully specified algorithm: pseudo-code or actual code snippet, plus a worked example (here are these inputs, here is the resulting signature byte-for-byte).
- Risk R1 closed.
- Phase 4a's unit tests can be written against this spec without re-reading the JAR.

---

### 0e — Consolidate reference docs

**Purpose:** Polish the running notes from 0a–0d into two coherent docs that serve as the only inputs Phase 1+ needs.

**`docs/spc-protocol.md` final structure:**
1. Wire envelope (`BaseVO`) — field names, "VOs extend BaseVO directly, no nested data"
2. Auth header (`x-access-token`), JWT algorithm (HS256), signing secret identity, TTLs, claim shape
3. Engine.IO — protocol version, ports (`18072` file+digest, `18073` task), namespaces, event names per channel, msgType registry, op registry, ping cadence
4. Endpoints, grouped by phase:
   - Phase 1 (auth + tasks)
   - Phase 2 (file listing, capacity)
   - Phase 3 (download)
   - Phase 4 (upload, mutation)
   - Phase 5 (recycle, search, conditional rendering)
   - Stubbed (equipment, summary, share, dictionary)
5. OSS HMAC algorithm (from 0d)
6. DTO field-name gotchas (full list including any 0a discovered beyond the ones already in the design plan)
7. Error codes (full enum from `FileErrorCodeEnum`, especially E0330)
8. Path/Unicode normalization (from 0b)
9. TLS posture (from 0b)
10. Device wire observations (from 0b) — endpoint hit list, idle behavior, error-code reactions
11. JWT acceptance verdict (from 0c)
12. Storage paths and timing constants from `Constant.java`

**`internal/spcserver/CLAUDE.md`:**
- Purpose of the package (device-facing handlers; SPC-shape DTOs/VOs; JWT auth; Engine.IO server).
- Conventions:
  - DTO/VO field names match `cfr-decrypted/` verbatim, snake_case included where present
  - Envelope shape `{success, errorCode, errorMsg}` flat; payload fields alongside, not nested under `data`
  - All handlers behind JWT middleware unless explicitly listed (login, equipment/bind/status, ratta_ping)
  - ResubmitCheck dedup applied per cfr-decrypted's method-level annotations
- Pointer to `docs/spc-protocol.md` as the spec source.
- Note: package is empty at end of Phase 0; Phase 1a creates `server.go` and `envelope.go`.

**Acceptance for 0e:**
- A new engineer (or post-`/clear` you) reading only `docs/spc-protocol.md` + `internal/spcserver/CLAUDE.md` + the design plan can begin Phase 1 without re-reading the JAR or running any tools.
- Self-check: pick three concrete questions Phase 1 will face ("What does the login response look like?" / "What header does the JWT go in?" / "What event name and msgType does a STARTSYNC push use?") and confirm each is answered explicitly in the two docs.

---

## Exit state (top-level Phase 0)

- `cfr-decrypted/` populated with every class in 0a's target table; method bodies real, not stubs.
- `cfr-decrypted/_endpoints.txt` exists.
- `docs/spc-protocol.md` exists in the UB repo, fully populated.
- `internal/spcserver/CLAUDE.md` exists.
- Design plan's risk table updated: R1, R2, R3, R4, R5, R6 closed (or follow-up sub-phase added to design plan if 0c rejected the JWT).
- **No** Go source code added under `internal/spcserver/`.
- Build/test state of the UB repo is unchanged from entry.

## Files this phase touches

**Created:**
- `docs/implementation-plans/spc-phase-0.md` (this file)
- `docs/spc-protocol.md`
- `internal/spcserver/CLAUDE.md`
- `/home/sysop/spc-rev/cfr-decrypted/**` (outside repo)

**Modified:**
- `/home/sysop/.claude/plans/okay-so-we-have-sunny-flame.md` (risk table updates at phase exit)

**Not touched:**
- Any UB runtime package
- `cmd/ultrabridge/main.go`
- Go module files

## Out of scope for Phase 0

- Any code that runs in `cmd/ultrabridge`.
- Anything under `internal/spcserver/` except `CLAUDE.md`.
- Editing the design plan beyond the risk-table updates.
- Phase 1 implementation plan (`spc-phase-1.md`) — that is the first task of Phase 1.

## Compact/clear checkpoint

After this phase exits cleanly: commit the two docs, `/clear`, and start Phase 1 by reading the design plan + `docs/spc-protocol.md` + `internal/spcserver/CLAUDE.md`.
