# UB-as-SPC Phase 1 — Skeleton + Auth + Tasks — Implementation Plan

**Goal:** An unmodified Supernote device logs into UltraBridge and syncs its task list end-to-end against the new `internal/spcserver/` package, exercising auth/JWT, the Engine.IO server, ResubmitCheck dedup, and SPC-shape DTOs on the cheapest subsystem (tasks). Files come in Phase 2+.

**Architecture:** Greenfield `internal/spcserver/` package holds all device-facing handlers, JWT auth, the Engine.IO server, and SPC-shape DTOs. Handlers translate SPC-shaped JSON ↔ UB's existing `internal/taskdb` store at the controller boundary — no second store, no MariaDB. A separate HTTP listener (default `:8089`), gated by `UB_SPC_MODE` (default `client` = not started), coexists with UB's existing web server and the real SPC during Phase 1–4. Routing uses `net/http.ServeMux` with Go 1.22 method+path patterns (the pattern already used in `cmd/ultrabridge/main.go`) — no new router dependency.

**Tech stack:** Go stdlib `net/http`; `crypto/hmac` + `crypto/sha256` + `encoding/base64` for HS256 JWT (hand-rolled, matching the claim shape in `docs/spc-protocol.md` §2 — no JWT library needed); `github.com/gorilla/websocket` (already a dependency) for the Engine.IO server; `modernc.org/sqlite` via `internal/taskdb` for the task store.

**Scope:** Phase 1 of the design plan `docs/design-plans/2026-05-15-ub-as-spc-refactor.md`, sub-phases 1a–1d (4 sub-phases, within the 8-phase limit).

**Codebase verified:** 2026-05-22.

**Spec source of truth:** `docs/spc-protocol.md` (Phase 0 output) for all wire facts, and `/home/sysop/spc-rev/cfr-decrypted/` for DTO/VO field names and handler behavior. Every DTO/VO and endpoint task cites `<FQN.java>:<line>`. When the design plan and `docs/spc-protocol.md` disagree, **`docs/spc-protocol.md` wins** — it is the Phase-0-verified record (e.g. the `bind/status` response is the flat `BindStatusVO`, not the design plan's pre-Phase-0 `{"data":{"bound":true}}`).

---

## Validation approach (all sub-phases)

Two tiers; most tasks use tier 1, each sub-phase's acceptance uses tier 2.

1. **No-device fast loop (per task):** Run UB in `server` mode on `:8089` *alongside* the real SPC container. Verify with `curl` against `localhost:8089/api/...` and `go test`. No device, no NPM change. This is the default verification for every task's "Verify" step.
2. **Device integration (per sub-phase acceptance):** Flip the Nginx Proxy Manager `supernote.broken.works` proxy-host upstream from `neptune:19072` (real SPC) to UB's SPC listener `:8089` (which serves both `/api/*` and, from 1c on, `/socket.io/*` — single port, see 1c). The device needs no change — same front-door flip used in Phase 0c. Flipping the upstream back to `neptune:19072` is an instant rollback; real SPC stays untouched on its own port throughout Phase 1. Device under test: Supernote Nomad `SN078C10034074`.

**Regression invariant:** with `UB_SPC_MODE=client` (the default), no SPC listener is started and UB behaves exactly as it does today. Every sub-phase must keep this true.

---

## Acceptance Criteria Coverage

This phase implements and tests the following. AC identifiers are scoped `spc-phase-1.ACx.y`.

### spc-phase-1.AC1: Skeleton + envelope + config (1a)
- **spc-phase-1.AC1.1 Success:** `POST /api/equipment/bind/status` returns a flat `BindStatusVO`: `{"success":true,"errorCode":"","errorMsg":"","bindStatus":true}`.
- **spc-phase-1.AC1.2 Regression:** `UB_SPC_MODE=client` (default) starts no SPC listener; UB runtime behavior is identical to today.
- **spc-phase-1.AC1.3 Success:** `UB_SPC_MODE=server` binds the SPC listener on `UB_SPC_LISTEN_ADDR`; serves TLS when `UB_SPC_TLS_CERT`+`UB_SPC_TLS_KEY` are both set, plain HTTP otherwise.
- **spc-phase-1.AC1.4 Invariant:** VO payload fields serialize alongside `success`/`errorCode`/`errorMsg` at the top level, never nested under a `data` key.

### spc-phase-1.AC2: JWT auth + login (1b)
- **spc-phase-1.AC2.1 Success:** `POST /api/official/user/account/login/equipment` with a valid account+password → flat `LoginVO` with a non-empty `token`.
- **spc-phase-1.AC2.2 Auth:** a protected endpoint with a valid `x-access-token` passes; missing/invalid token → SPC error envelope (`success:false`).
- **spc-phase-1.AC2.3 Crypto:** a UB-minted token round-trips through verify; tampered signature and wrong-secret are rejected.
- **spc-phase-1.AC2.4 Challenge + boot stubs:** `query/random/code` → `RandomCodeVO{randomCode,timestamp}`; `check/exists/server` → well-formed `UserCheckVO`; `user/logout` → bare `BaseVO` success; the login-handshake/boot stubs `bindEquipment`, `terminal/equipment/unlink`, `GET /api/file/query/server` → well-formed `success:true`.
- **spc-phase-1.AC2.5 Device (tier 2):** real device flipped to UB logs in and makes authenticated calls.
- **spc-phase-1.AC2.6 Password:** `sha256Hex(md5Hex(rawPassword) + randomCode) == webPassword.trim()` accepts; wrong `webPassword` and a reused/expired code reject. (`md5Hex`/`sha256Hex` = lowercase zero-padded hex; recipe RESOLVED in `docs/spc-protocol.md` §2.1.)

### spc-phase-1.AC3: Engine.IO v3 server, connection-only (1c)
- **spc-phase-1.AC3.1 Handshake:** a WS client completes the EIO v3 handshake — server sends open `0{"sid":…,"upgrades":[],"pingInterval":5000,"pingTimeout":25000}` and the socket.io `40` connect.
- **spc-phase-1.AC3.2 Ping:** native Engine.IO ping `2` → server pong `3`.
- **spc-phase-1.AC3.3 ratta_ping:** `42["ratta_ping"]` → server `42["ratta_ping","Received"]`.
- **spc-phase-1.AC3.4 Auth:** connect with a missing/invalid `token` query param → connection closed.
- **spc-phase-1.AC3.5 Device (tier 2):** real device connects and stays connected ≥30 min (ping/pong + ratta_ping), no disconnect.
- **spc-phase-1.AC3.6 Registry:** `Emit(userId, event, payloadJSON)` writes a frame to that user's live connection(s); no-ops when none.

### spc-phase-1.AC4: Task endpoints + mapping + dedup + STARTSYNC (1d)
- **spc-phase-1.AC4.1 Groups:** `/schedule/group/all` returns the single synthesized group (`ScheduleTaskGroupVO`); group create/update/delete/clear/get return well-formed success.
- **spc-phase-1.AC4.2 Task list:** `/schedule/task/all` returns tasks mapped from `taskdb`, paginated 20/page with `nextPageToken` (singular) when more remain and a `nextSyncToken`; request `nextPageTokens` (plural) advances pages.
- **spc-phase-1.AC4.3 Task CRUD:** create / update / put-list / delete / get map `taskstore.Task ↔ SPCTask` correctly, preserving the `completedTime`=creation / `lastModified`=completion quirk and soft-delete.
- **spc-phase-1.AC4.4 Dedup:** an identical create POST repeated within 1 s is deduplicated (single effect).
- **spc-phase-1.AC4.5 STARTSYNC:** a task write via web/CalDAV emits a FILE-SYN `ServerMessage` STARTSYNC over the device's socket (via the 1c registry).
- **spc-phase-1.AC4.6 Device (tier 2):** device sees its tasks in its task UI; device edit → web UI ≤5 s; web/CalDAV edit → device receives STARTSYNC and pulls ≤5 s.
- **spc-phase-1.AC4.7 Summary stubs:** `query/summary/{hash,group,id}` return well-formed success (device hits these every sync; `docs/spc-protocol.md` §5).

---

## Sub-phase 1a — HTTP listener skeleton + envelope + config wiring

**Type:** mixed — config parsing and the envelope/handler are functionality (tested); the listener wiring is infrastructure (verified operationally).

**Entry state:** No `internal/spcserver/*.go` source files exist (only the stub `internal/spcserver/CLAUDE.md`). `internal/appconfig` has no SPC keys. `cmd/ultrabridge/main.go` does not reference `spcserver`.

<!-- START_SUBCOMPONENT_A (tasks 1-4) -->
<!-- START_TASK_1 -->
### Task 1: Add SPC server config keys to appconfig

**Verifies:** spc-phase-1.AC1.2 (default mode = client)

**Files:**
- Modify: `internal/appconfig/keys.go`
- Modify: `internal/appconfig/config.go`
- Test: `internal/appconfig/config_test.go`

**Implementation:**
`internal/appconfig` requires a setting key to be added in lockstep across several maps/structs (verified 2026-05-22). Add all of:

- `keys.go` const block (new "SPC server" group):
  - `KeySPCMode = "spc_mode"`
  - `KeySPCListenAddr = "spc_listen_addr"`
  - `KeySPCTLSCert = "spc_tls_cert"`
  - `KeySPCTLSKey = "spc_tls_key"`
- `keys.go` `envVarForKey` map: `KeySPCMode→"UB_SPC_MODE"`, `KeySPCListenAddr→"UB_SPC_LISTEN_ADDR"`, `KeySPCTLSCert→"UB_SPC_TLS_CERT"`, `KeySPCTLSKey→"UB_SPC_TLS_KEY"`.
- `keys.go` `defaultValues` map: `KeySPCMode→"client"`, `KeySPCListenAddr→":8089"`. (Cert/key default empty → omit.)
- `keys.go` `restartRequired` map: all four → `true`.
- `config.go` `Config` struct: new "SPC server" group with `SPCMode string`, `SPCListenAddr string`, `SPCTLSCert string`, `SPCTLSKey string`.
- `config.go` `loadConfigFromDB`: populate the four fields from `dbVals[...]`.
- `config.go` `configToMap`: emit the four fields.

**Testing:**
Follow the existing table/roundtrip patterns in `config_test.go` (real in-memory SQLite, no mocks):
- spc-phase-1.AC1.2: a freshly-loaded config (nothing in DB, no env) has `SPCMode == "client"` and `SPCListenAddr == ":8089"`.
- Save→Load roundtrip preserves all four SPC fields.
- `UB_SPC_MODE=server` env override beats a DB value of `client`.

**Verification:**
Run: `go test -C /home/sysop/src/ultrabridge ./internal/appconfig/`
Expected: all tests pass.

**Commit:** `feat(spcserver): add SPC server config keys to appconfig`
<!-- END_TASK_1 -->

<!-- START_TASK_2 -->
### Task 2: Flat BaseVO envelope helper

**Verifies:** spc-phase-1.AC1.4

**Files:**
- Create: `internal/spcserver/envelope.go`
- Test: `internal/spcserver/envelope_test.go`

**Implementation:**
Define the wire envelope matching `com/ratta/vo/BaseVO.java` (cite it in a comment) and helpers. VOs embed `BaseVO` anonymously so promoted fields serialize flat — this is what guarantees AC1.4 structurally, not by convention.

```go
package spcserver

// BaseVO is the SPC response envelope. Every VO embeds it anonymously so
// payload fields serialize at the top level alongside these three, never under
// a "data" key. Mirrors com/ratta/vo/BaseVO.java.
type BaseVO struct {
	Success   bool   `json:"success"`
	ErrorCode string `json:"errorCode"`
	ErrorMsg  string `json:"errorMsg"`
}

func OK() BaseVO { return BaseVO{Success: true} }

func WriteJSON(w http.ResponseWriter, v any) { /* set Content-Type: application/json;charset=UTF-8; json.NewEncoder(w).Encode(v) */ }

func WriteError(w http.ResponseWriter, code, msg string) { /* WriteJSON(w, BaseVO{false, code, msg}) */ }
```

**Testing:**
- spc-phase-1.AC1.4: marshal a small `struct{ BaseVO; Foo string `json:"foo"` }{OK(), "bar"}` and assert the JSON has top-level keys `success`, `errorCode`, `errorMsg`, `foo` and **no** `data` key.

**Verification:**
Run: `go test -C /home/sysop/src/ultrabridge ./internal/spcserver/`
Expected: pass.

**Commit:** `feat(spcserver): flat BaseVO envelope helper`
<!-- END_TASK_2 -->

<!-- START_TASK_3 -->
### Task 3: Server skeleton + bind/status stub handler

**Verifies:** spc-phase-1.AC1.1, spc-phase-1.AC1.4

**Files:**
- Create: `internal/spcserver/server.go`
- Create: `internal/spcserver/handlers/equipment.go`
- Test: `internal/spcserver/handlers/equipment_test.go`

**Implementation:**
`server.go`:
```go
type Config struct {
	Mode       string // "client" | "server"
	ListenAddr string
	TLSCert    string
	TLSKey     string
	Logger     *slog.Logger
}

type Server struct { cfg Config; mux *http.ServeMux }

func New(cfg Config) *Server // builds mux, calls registerRoutes
func (s *Server) Handler() http.Handler { return s.mux } // exported for tests
func (s *Server) Run() error {
	if s.cfg.TLSCert != "" && s.cfg.TLSKey != "" {
		return http.ListenAndServeTLS(s.cfg.ListenAddr, s.cfg.TLSCert, s.cfg.TLSKey, s.mux)
	}
	return http.ListenAndServe(s.cfg.ListenAddr, s.mux)
}
```
`registerRoutes` wires `mux.HandleFunc("POST /api/equipment/bind/status", handlers.BindStatus)`.

`handlers/equipment.go` — the response VO matches `BindStatusVO.java` (extends BaseVO, single Boolean `bindStatus`). Cite `E_EquipmentController.java:101` and `BindStatusVO.java`:
```go
type bindStatusVO struct {
	spcserver.BaseVO
	BindStatus bool `json:"bindStatus"`
}
func BindStatus(w http.ResponseWriter, r *http.Request) {
	spcserver.WriteJSON(w, bindStatusVO{spcserver.OK(), true})
}
```
(Note: in 1a this stub is unauthenticated. SPC reads `x-access-token` here to log the device; auth-protecting it is deferred to 1b. The device polls it ~4×/session and only needs `success:true` — confirmed `docs/spc-protocol.md` §5/§11.)

> Import-cycle check: `handlers` importing the parent `spcserver` package for `BaseVO`/`WriteJSON` is one-directional (`server.go` imports `handlers`, not vice versa for types) — confirm no cycle at build time; if one arises, move `BaseVO`/`WriteJSON` into a leaf `internal/spcserver/envelope` subpackage. Resolve during implementation, do not leave ambiguous.

**Testing:**
- spc-phase-1.AC1.1 + AC1.4: `httptest` POST to `BindStatus` → assert body equals `{"success":true,"errorCode":"","errorMsg":"","bindStatus":true}` (exact, flat).

**Verification:**
Run: `go test -C /home/sysop/src/ultrabridge ./internal/spcserver/...`
Expected: pass.

**Commit:** `feat(spcserver): HTTP server skeleton + bind/status stub`
<!-- END_TASK_3 -->

<!-- START_TASK_4 -->
### Task 4: Spawn the SPC listener from main.go in server mode

**Verifies:** spc-phase-1.AC1.2, spc-phase-1.AC1.3

**Files:**
- Modify: `cmd/ultrabridge/main.go`

**Implementation:**
After `appconfig.Load` succeeds (`cmd/ultrabridge/main.go:256`, where `cfg` is available), add a guarded spawn before the main HTTP server block:
```go
if cfg.SPCMode == "server" {
	spcSrv := spcserver.New(spcserver.Config{
		Mode:       cfg.SPCMode,
		ListenAddr: cfg.SPCListenAddr,
		TLSCert:    cfg.SPCTLSCert,
		TLSKey:     cfg.SPCTLSKey,
		Logger:     logger,
	})
	go func() {
		logger.Info("spc server starting", "addr", cfg.SPCListenAddr, "tls", cfg.SPCTLSCert != "")
		if err := spcSrv.Run(); err != nil {
			logger.Error("spc server error", "error", err)
		}
	}()
}
```
Add the `spcserver` import. Default `client` mode spawns nothing — zero behavioral change.

**Verification (operational):**
- `go build -C /home/sysop/src/ultrabridge ./...` succeeds.
- `go vet -C /home/sysop/src/ultrabridge ./...` clean.
- Run UB with `UB_SPC_MODE=server UB_SPC_LISTEN_ADDR=:8089` (other env as usual): `curl -s localhost:8089/api/equipment/bind/status` returns the flat envelope (AC1.3, AC1.1).
- Run UB with defaults (no `UB_SPC_MODE`): port `:8089` is closed; existing UB endpoints behave as before (AC1.2).

**Commit:** `feat(spcserver): spawn SPC listener in server mode`
<!-- END_TASK_4 -->
<!-- END_SUBCOMPONENT_A -->

**1a exit state:** `go build -C /home/sysop/src/ultrabridge ./...` and `go test -C /home/sysop/src/ultrabridge ./internal/spcserver/... ./internal/appconfig/` are green. `client` mode = today's behavior (regression-safe). One real endpoint (`bind/status`) answers correctly in `server` mode. This becomes the entry state for 1b.

---

## Sub-phase 1b — JWT auth + login endpoint

**Type:** functionality (JWT, password verification, handlers are all tested), with one tier-2 device task (T12) for the live device-login acceptance. The `servicePassword` recipe is already fully known (see below) — no capture needed.

**Entry state:** 1a exit state holds — `internal/spcserver/{server.go,envelope.go}` and `handlers/equipment.go` exist; the listener spawns in `server` mode; appconfig has the 1a SPC keys.

**Key spec facts (from `docs/spc-protocol.md` §2 + §2.1, cite in code):**
- Auth header `x-access-token` (`Constant.AUTHORIZE_TOKEN`). HS256, signing secret = `Constant.SECRET` (the long ~280-char string, **not** the 32-char `JWT_SECRET`).
- UB auth is **stateless**: verify the HMAC signature + trust the `userId` claim. Do **not** replicate SPC's Redis token-existence check (`JwtTokenUserUtil.userId():249`) — UB has no Redis, is single-user, and the device does no client-side JWT validation (proven 0c). This is the design plan's intent.
- Token shape to mint (the exp-bearing flavor observed on the wire and accepted by the live device in 0c — see the reference stub `/home/sysop/spc-rev/jwt-test/main.go`): claims `{userId, createTime:<unix_sec>, key:"<userId>_<sec>_<ms>", exp:<far-future>}`.
- Terminal-login password check (recipe RESOLVED, `docs/spc-protocol.md` §2.1; confirmed by UB's own working SPC client `internal/tasksync/supernote/client.go:61-66`): `webPassword == sha256Hex( md5Hex(rawPassword) + randomCode )`. `servicePassword = md5Hex(rawPassword)`; `randomCode` is the one-time value issued at `/query/random/code` (looked up by account, deleted after use). Both `md5Hex` and `sha256Hex` are lowercase zero-padded hex over UTF-8. **UB stores the raw password** (`UB_SPC_DEVICE_PASSWORD`) and computes `md5Hex(raw)` at validation time — no capture/brute needed.

<!-- START_SUBCOMPONENT_B (tasks 5-12) -->
<!-- START_TASK_5 -->
### Task 5: 1b config keys + DB handle into spcserver

**Verifies:** (infrastructure for AC2; no AC of its own)

**Files:**
- Modify: `internal/appconfig/keys.go`, `internal/appconfig/config.go`, `internal/appconfig/config_test.go`
- Modify: `internal/spcserver/server.go`

**Implementation:**
- appconfig (same six-spot pattern as Task 1): consts `KeySPCJWTSecret="spc_jwt_secret"`, `KeySPCDeviceAccount="spc_device_account"`, `KeySPCDevicePassword="spc_device_password"` (the **raw** account password; UB computes `md5Hex(raw)` internally); `envVarForKey` → `UB_SPC_JWT_SECRET`, `UB_SPC_DEVICE_ACCOUNT`, `UB_SPC_DEVICE_PASSWORD`; `defaultValues[KeySPCJWTSecret]` = the long `Constant.SECRET` string (cite `Constant.java:46` / `JwtTokenUserUtil.java:57`); `restartRequired` all three → true; `Config` fields `SPCJWTSecret/SPCDeviceAccount/SPCDevicePassword`; wire `loadConfigFromDB` + `configToMap`.
- `spcserver.Config` gains `DB *sql.DB` (the notedb handle). `server.go` stores it for handlers that persist/read settings via `internal/notedb` (`spc_user_id`, etc.). `spc_user_id` is **runtime-managed**, not an appconfig key — accessed directly via `notedb.GetSetting/SetSetting`.

**Testing:** extend `config_test.go` — roundtrip preserves the three keys; `UB_SPC_JWT_SECRET` env override; default JWT secret equals `Constant.SECRET`.

**Verification:** `go test -C /home/sysop/src/ultrabridge ./internal/appconfig/`

**Commit:** `feat(spcserver): JWT/login config keys + notedb handle`
<!-- END_TASK_5 -->

<!-- START_TASK_6 -->
### Task 6: HS256 JWT mint/verify

**Verifies:** spc-phase-1.AC2.3

**Files:**
- Create: `internal/spcserver/auth/jwt.go`
- Test: `internal/spcserver/auth/jwt_test.go`

**Implementation:** Hand-rolled HS256 (no JWT lib), mirroring the proven 0c stub:
```go
func Mint(userID, secret string) string // header {"typ":"JWT","alg":"HS256"}; claims {userId, createTime:<sec>, key:"<userId>_<sec>_<ms>", exp:<now+~60yr>}; base64.RawURLEncoding; HMAC-SHA256
func Verify(token, secret string) (userID string, err error) // split 3 parts, recompute sig, hmac.Equal (constant-time), parse claims, return userId
```

**Testing:**
- AC2.3: `Verify(Mint(uid, s), s)` returns `uid`; flipping a byte in the signature → error; `Verify(token, wrongSecret)` → error.
- minted token decodes to claims with keys `userId`, `createTime`, `key`, `exp` (matches the 0c-accepted shape).

**Verification:** `go test -C /home/sysop/src/ultrabridge ./internal/spcserver/auth/`

**Commit:** `feat(spcserver): HS256 JWT mint/verify`
<!-- END_TASK_6 -->

<!-- START_TASK_7 -->
### Task 7: Auth middleware + opportunistic userId harvest

**Verifies:** spc-phase-1.AC2.2

**Files:**
- Create: `internal/spcserver/auth/middleware.go`
- Test: `internal/spcserver/auth/middleware_test.go`

**Implementation:**
- `Middleware(secret string, store SettingStore, next http.Handler) http.Handler`: read `x-access-token`; `Verify`; on failure `spcserver.WriteError` with the SPC-expected envelope (`success:false`); on success put `userId` in request context (`auth.UserID(ctx) string`).
- **userId harvest:** on a successful verify, if `store` has no `spc_user_id` yet, persist the verified `userId`. This adopts the device's real-SPC userId when it presents its old token during the NPM flip. (`SettingStore` is a tiny interface — `Get(ctx,key)`, `Set(ctx,key,val)` — implemented over `notedb` in main wiring; keeps `auth` import-light and unit-testable with a fake.)

**Testing:** valid token → next called, `UserID(ctx)` set; missing/garbage token → `success:false` envelope, next not called; harvest persists userId on first valid token, no-ops once set (fake store).

**Verification:** `go test -C /home/sysop/src/ultrabridge ./internal/spcserver/auth/`

**Commit:** `feat(spcserver): x-access-token middleware + userId harvest`
<!-- END_TASK_7 -->

<!-- START_TASK_8 -->
### Task 8: Login DTOs/VOs

**Verifies:** (types for AC2.1/AC2.4; no test of its own)

**Files:**
- Create: `internal/spcserver/dto/login.go`

**Implementation:** Match decompiled field names verbatim, cite sources:
- `LoginDTO` — `account, password, equipment(int), equipmentNo, loginMethod, countryCode, browser, language, timestamp` (`LoginDTO.java`).
- `LoginVO` (embeds `spcserver.BaseVO`) — `token, counts, userName, avatarsUrl, lastUpdateTime, isBind, isBindEquipment, soldOutCount` (`LoginVO.java`).
- `RandomCodeVO` (embeds BaseVO) — `randomCode, timestamp` (`RandomCodeVO.java`).
- `UserCheckVO` (embeds BaseVO) — minimal existence response (fields per `UserCheckVO` — verify; a `success:true` + existence flag is sufficient since the device proceeds on well-formed success, per 0c).

**Verification:** `go build -C /home/sysop/src/ultrabridge ./internal/spcserver/...`

**Commit:** `feat(spcserver): login DTOs/VOs`
<!-- END_TASK_8 -->

<!-- START_TASK_9 -->
### Task 9: Verification core — randomCode store, sha256Hex, userId resolver

**Verifies:** spc-phase-1.AC2.6

**Files:**
- Create: `internal/spcserver/login/randomcode.go` (in-memory one-time code store)
- Create: `internal/spcserver/login/password.go` (`sha256Hex`)
- Create: `internal/spcserver/login/userid.go` (`ResolveUserID`)
- Test: `internal/spcserver/login/login_test.go`

**Implementation:**
- `randomcode`: `Store` with `Issue(account) (code string)` (random, e.g. 6–8 digits or a short token — exact format is cosmetic; the device echoes it back inside the hash), `Consume(account) (code string, ok bool)` (returns + deletes), TTL ~5 min via stored issue-time, background GC ticker. `sync.Mutex`-guarded map keyed by account.
- `sha256Hex(s string) string` and `md5Hex(s string) string`: `hex.EncodeToString(sha256.Sum256(...)/ md5.Sum(...))` (Go's hex is lowercase zero-padded — matches `SHA256Util.byte2Hex` / `MD5StrUtil`). `ServicePassword(raw string) string { return md5Hex(raw) }`.
- `ResolveUserID(ctx, store SettingStore) (string, error)`: return persisted `spc_user_id` if set; else generate a stable 19-digit numeric id, persist, return.

**Testing:**
- AC2.6: assert `md5Hex("ehh1701jqb")` and `sha256Hex(md5Hex("ehh1701jqb")+"Y")` each equal a precomputed golden hex (lock the recipe end-to-end); a verify helper `sha256Hex(md5Hex(raw)+code) == webPassword` accepts the matching `webPassword`, rejects a wrong one.
- randomCode: `Consume` returns the issued code once, then `ok=false` (one-time); after TTL, `ok=false` (expired).
- `ResolveUserID`: first call generates+persists; second returns the same value (fake store).

**Verification:** `go test -C /home/sysop/src/ultrabridge ./internal/spcserver/login/`

**Commit:** `feat(spcserver): randomCode store, sha256Hex, userId resolver`
<!-- END_TASK_9 -->

<!-- START_TASK_10 -->
### Task 10: Login + challenge handlers

**Verifies:** spc-phase-1.AC2.1, spc-phase-1.AC2.4, spc-phase-1.AC2.6

**Files:**
- Create: `internal/spcserver/handlers/login.go`
- Test: `internal/spcserver/handlers/login_test.go`

**Implementation:** handlers take their deps (config values, randomCode store, setting store, jwt secret) via a small handler struct constructed in `server.go`.
- `POST /api/official/user/query/random/code`: `code := store.Issue(dto.account)`; return `RandomCodeVO{OK(), code, nowMs}`.
- `POST /api/official/user/check/exists/server`: return `UserCheckVO` indicating the account exists (well-formed success).
- `POST /api/official/user/account/login/equipment` (+ `/login/new` alias): if `UB_SPC_DEVICE_ACCOUNT` set, require `dto.account == it`; `code, ok := store.Consume(dto.account)` (fail → error envelope); require `sha256Hex(md5Hex(UB_SPC_DEVICE_PASSWORD) + code) == strings.TrimSpace(dto.password)` (fail → error envelope, AC2.6); `userID := ResolveUserID(...)`; `token := auth.Mint(userID, secret)`; return `LoginVO{OK(), token, …, isBind:"1", isBindEquipment:"1"}`.
- `POST /api/user/query/token`: return `QueryTokenVO` (echo token if present).
- `POST /api/user/logout`: bare `BaseVO` success.
- **Login-handshake/boot stubs (required for the device to complete login/boot — `docs/spc-protocol.md` §2 handshake + §5):**
  - `POST /api/terminal/user/bindEquipment` → bare `BaseVO` success (handshake step 4; device sends `{account,equipmentNo,flag,label[],name,totalCapacity}` — read loosely, return success). Cite `E_EquipmentController.java:88`.
  - `POST /api/terminal/equipment/unlink` → bare `BaseVO` success (device logout). Cite `E_EquipmentController.java:95`.
  - `GET /api/file/query/server` → bare `BaseVO` success (boot reachability check). Cite `F_FileLocalController.java:235`.
  - (`bind/status` already served by 1a Task 3.)

**Testing:** issue→login happy path returns non-empty token (AC2.1); wrong password → `success:false` (AC2.6); reused code → reject; random/code returns a code (AC2.4); logout → bare success; bindEquipment/unlink/query/server → well-formed `success:true` (AC2.4).

**Verification:** `go test -C /home/sysop/src/ultrabridge ./internal/spcserver/handlers/`

**Commit:** `feat(spcserver): login + challenge handlers`
<!-- END_TASK_10 -->

<!-- START_TASK_11 -->
### Task 11: Wire routes + protect a stub; full no-device loop

**Verifies:** spc-phase-1.AC2.1, spc-phase-1.AC2.2 (operational)

**Files:**
- Modify: `internal/spcserver/server.go`
- Modify: `cmd/ultrabridge/main.go` (pass `noteDB` into `spcserver.Config.DB`; provide the `SettingStore` adapter over `notedb`)

**Implementation:** register login + challenge routes (unauthenticated) and wrap `POST /api/user/query` (returns a minimal `UserQueryByIdVO`-shaped success) with `auth.Middleware`. Construct the randomCode store + handler struct in `New`.

**Verification (operational):**
- `go build` / `go vet` / `go test ./...` green.
- Run UB `server` mode with `UB_SPC_DEVICE_PASSWORD=<raw>`: `curl` `query/random/code` → get `code`; compute `webPassword = sha256Hex(md5Hex(<raw>)+code)` locally; `curl` login → non-empty `token` (AC2.1); `curl` `user/query` with `x-access-token: <token>` → success; without → `success:false` (AC2.2); wrong `webPassword` → `success:false`.

**Commit:** `feat(spcserver): wire auth routes + protected probe`
<!-- END_TASK_11 -->

<!-- START_TASK_12 -->
### Task 12: Device login (tier-2 acceptance)

**Verifies:** spc-phase-1.AC2.5

**Steps (config + device, no code):**
1. Set `UB_SPC_DEVICE_ACCOUNT` to the account (`starkruzr@gmail.com`) and `UB_SPC_DEVICE_PASSWORD` to the raw password (`ehh1701jqb`). (servicePassword recipe is already known — §2.1 — so no capture/brute step.)
2. Flip NPM `supernote.broken.works` upstream to UB; log the device out and back in.

**Acceptance:** device logs into UB (no client-side change) — the device computes `sha256Hex(md5Hex(raw)+code)`, UB validates it, mints the token — then makes authenticated calls (`/api/user/query`, `bind/status`) carrying the UB-minted token. Flip NPM back to real SPC to roll back. If login fails, watch the tap to compare the device's `webPassword` against UB's computed value (sanity-check the recipe against this firmware build).

**Commit:** docs-only if no code change (`docs(spcserver): record 1b device-login result`).
<!-- END_TASK_12 -->
<!-- END_SUBCOMPONENT_B -->

**1b exit state:** `go build`/`go vet`/`go test ./...` green. Device logs into UB and makes authenticated calls in `server` mode; `client` mode still regression-safe. `internal/spcserver/{auth,dto,login,handlers}` exist. This becomes the entry state for 1c.

---

## Sub-phase 1c — Engine.IO v3 server (connection-only)

**Type:** functionality — the Engine.IO framing, handshake, and registry are unit/integration-tested with a `gorilla/websocket` client against `httptest`; one tier-2 device task (T17) for the live soak.

**Entry state:** 1b exit state holds — `internal/spcserver/{server.go,auth,dto,login,handlers}` exist; JWT `auth.Verify` is available; the listener spawns in `server` mode.

**Key spec facts (from `docs/spc-protocol.md` §3 + `SocketIOEventHandler.java`, cite in code):**
- **Protocol:** Engine.IO v3 / Socket.IO v1 (`netty-socketio` 2.0.3). The device connects **directly over websocket** — its WS URL carries no `sid` (no prior polling phase): `/socket.io/?token=<JWT>&type=ANDROID<uuid>&random=<unix_ms>&sign=<sig>&EIO=3&transport=websocket`.
- **Handshake:** server sends Engine.IO open packet `0` + JSON `{"sid":"<id>","upgrades":[],"pingInterval":5000,"pingTimeout":25000}`; client sends socket.io connect `40`; server replies `40`. (Reference: `internal/sync/notifier.go` is the client side of this exact exchange.)
- **Keepalive:** the device sends, every 5 s, **both** the native ping `2` (server → `3`) **and** `42["ratta_ping"]` (server → `42["ratta_ping","Received"]`, `SocketIOEventHandler.java:172,198`). The native ping is client-initiated in EIO v3; the server does not send pings, only pongs.
- **Auth at connect:** SPC reads `token` from the handshake query and disconnects on invalid (`userSuccess`→`disconnect`). Connection identity is `userId` (from token) + `type` + `random`. The `sign` param (`SignVerifierSocketIO`, §3) is **accepted-and-ignored** in 1c (token verification is the gate); signature validation is deferrable.
- **No business events in 1c:** other `42[...]` frames are logged only. STARTSYNC/pending-message push (which SPC drives off `ratta_ping`) is 1d.

**Decision — single listener, no separate socket port.** UB serves `/socket.io/` on the **same `:8089` listener** as `/api/`. SPC's `:18072`/`:18073` are container-internal ports the device never sees (it only ever connects to `host:443`, paths `/api/*` and `/socket.io/*`; demux is server-side). NPM routes both paths for the host to the one UB upstream. No new config key. The task channel is dropped entirely (device never opens it, §3).

**Decision — permessage-deflate not negotiated.** The upgrade response omits the extension, so a compliant client falls back to uncompressed frames (our notifier client already speaks this protocol to SPC uncompressed). **Residual risk:** if the firmware hard-requires the negotiated compression and drops the link, T17 surfaces it; the fallback is a minimal permessage-deflate (no context-takeover). Tracked, not pre-built.

<!-- START_SUBCOMPONENT_C (tasks 13-17) -->
<!-- START_TASK_13 -->
### Task 13: Engine.IO v3 packet codec

**Verifies:** spc-phase-1.AC3.1 (encoding correctness)

**Files:**
- Create: `internal/spcserver/socketio/engineio.go`
- Test: `internal/spcserver/socketio/engineio_test.go`

**Implementation:** packet-type constants (`0` open, `2` ping, `3` pong, `4` message; socket.io sub-types `40` connect, `42` event). `EncodeOpen(sid string) []byte` → `0{json}` with `upgrades:[]`, `pingInterval:5000`, `pingTimeout:25000`. `ClassifyFrame([]byte) (kind, eventName, payload)` distinguishing native ping, socket.io connect, and `42["<event>",<payload>]` events (tolerant parse — extract event name; payload may be absent as in `42["ratta_ping"]`).

**Testing:** `EncodeOpen` produces valid JSON with the four fields; classifier identifies `2`, `40`, `42["ratta_ping"]`, and `42["ClientMessage",{…}]` correctly.

**Verification:** `go test -C /home/sysop/src/ultrabridge ./internal/spcserver/socketio/`

**Commit:** `feat(spcserver): Engine.IO v3 packet codec`
<!-- END_TASK_13 -->

<!-- START_TASK_14 -->
### Task 14: Connection registry

**Verifies:** spc-phase-1.AC3.6

**Files:**
- Create: `internal/spcserver/socketio/registry.go`
- Test: `internal/spcserver/socketio/registry_test.go`

**Implementation:** `Registry` — thread-safe map from `userId` to a set of live connections. `Add(userId, *conn)`, `Remove(*conn)`, `Emit(userId, event string, payload any) (delivered int)` marshals `42["<event>",<payload>]` and writes to each of that user's conns (per-conn write mutex, mirroring `notifier.go`'s `writeMu`). A `conn` wraps `*websocket.Conn` + write mutex + identity. `Emit` to an absent userId returns 0 (no-op).

**Testing:** `Add` then `Emit` writes the framed event to a fake/loopback conn; `Emit` to unknown userId returns 0; `Remove` stops delivery.

**Verification:** `go test -C /home/sysop/src/ultrabridge ./internal/spcserver/socketio/`

**Commit:** `feat(spcserver): Engine.IO connection registry`
<!-- END_TASK_14 -->

<!-- START_TASK_15 -->
### Task 15: WebSocket handler (handshake + keepalive)

**Verifies:** spc-phase-1.AC3.1, AC3.2, AC3.3, AC3.4

**Files:**
- Create: `internal/spcserver/socketio/server.go`
- Test: `internal/spcserver/socketio/server_test.go`

**Implementation:** `Handler{secret string, reg *Registry, logger}` with `ServeHTTP`:
- `websocket.Upgrader` with **no** `EnableCompression` (decision above); permissive `CheckOrigin` (device sends no Origin).
- Extract `token`/`type`/`random`/`sign` from the query. `userID, err := auth.Verify(token, secret)`; on error, close immediately (AC3.4).
- Send open `0{…}` (random `sid`); on receiving `40`, reply `40`; `reg.Add(userID, conn)` (defer `reg.Remove`).
- Read loop: `2`→write `3` (AC3.2); `42["ratta_ping"]`→write `42["ratta_ping","Received"]` (AC3.3); other `42[...]`→log only. Set a read deadline ~`pingTimeout` (25 s) refreshed on every frame so dead clients are reaped.

**Testing:** drive with a `gorilla/websocket` *client* against `httptest.NewServer(handler)`:
- valid minted token → receive open `0{…}` with the four fields, send `40`, receive `40` (AC3.1).
- send `2` → receive `3` (AC3.2); send `42["ratta_ping"]` → receive `42["ratta_ping","Received"]` (AC3.3).
- bad/empty token → handshake closed (AC3.4).

**Verification:** `go test -C /home/sysop/src/ultrabridge ./internal/spcserver/socketio/`

**Commit:** `feat(spcserver): Engine.IO v3 websocket handler`
<!-- END_TASK_15 -->

<!-- START_TASK_16 -->
### Task 16: Wire /socket.io/ into the SPC server

**Verifies:** spc-phase-1.AC3.1 (operational, same listener)

**Files:**
- Modify: `internal/spcserver/server.go`

**Implementation:** construct the `socketio.Registry` + `socketio.Handler` in `spcserver.New` (sharing the JWT secret) and register `mux.Handle("/socket.io/", h)` on the **same** `:8089` mux. Expose the registry on `*Server` (e.g. `Server.SocketRegistry()`) so 1d can `Emit` STARTSYNC. No `cmd/ultrabridge/main.go` change (same listener) and no new config key.

**Verification (operational):** `go build`/`go vet`/`go test ./...` green; a local `gorilla` client script connects to `ws://localhost:8089/socket.io/?...&EIO=3&transport=websocket` with a UB-minted token, completes handshake, exchanges several `2`/`3` and `ratta_ping` cycles, stays connected.

**Commit:** `feat(spcserver): serve /socket.io/ on the SPC listener`
<!-- END_TASK_16 -->

<!-- START_TASK_17 -->
### Task 17: Device soak (tier-2 acceptance)

**Verifies:** spc-phase-1.AC3.5

**Steps (config + device, no code):**
1. Flip NPM so both `/api/*` and `/socket.io/*` for `supernote.broken.works` route to UB (`:8089`).
2. Log the device in (1b flow); confirm it opens the Engine.IO socket and exchanges `2`/`3` + `ratta_ping` (watch UB logs / the tap).
3. Leave idle ≥30 min; confirm no disconnect/reconnect storm.
4. **If** the device fails to keep the socket up specifically due to missing compression, implement the minimal no-context-takeover permessage-deflate fallback (new task) and retry. Flip NPM back to roll back.

**Acceptance:** device holds a live Engine.IO connection to UB for ≥30 min.

**Commit:** docs-only if no fallback needed (`docs(spcserver): record 1c device soak result`).
<!-- END_TASK_17 -->
<!-- END_SUBCOMPONENT_C -->

**1c exit state:** build/test green; device maintains a live Engine.IO connection to UB; `Server.SocketRegistry()` is ready for 1d STARTSYNC pushes; `client` mode unaffected. This becomes the entry state for 1d.

---

## Sub-phase 1d — Task endpoints + mapping + dedup + STARTSYNC

**Type:** functionality — DTO mapping, dedup, and handlers are unit-tested against in-memory `taskdb` (real SQLite, no mocks, per `internal/taskdb` test conventions); one tier-2 device task (T24) for the end-to-end sync.

**Entry state:** 1c exit state holds — `internal/spcserver/{server.go,envelope.go,auth,dto,login,handlers,socketio}` exist; JWT middleware works; the socket registry is exposed via `Server.SocketRegistry()`.

**Key spec facts (cite in code):**
- **Task wire shape** = the proven `SPCTask` field set from `internal/tasksync/supernote/client.go:232-255` (it reads/writes exactly what real SPC emits): `taskId,taskListId,title,detail,status,importance,dueTime,completedTime,lastModified,recurrence,isReminderOn,links,isDeleted` + sort fields (`sort,sortCompleted,sortTime,planerSort,planerSortTime,allSort,allSortCompleted,allSortTime`). Quirk: `completedTime` holds creation time, `lastModified` holds completion time.
- **`/schedule/group/all`** → `ScheduleTaskGroupVO{pageToken, scheduleTaskGroup:[ScheduleTaskGroupDO{taskListId,userId,title,lastModified,isDeleted}]}` (`F_ScheduleController.java:116`, `ScheduleTaskGroupVO.java`).
- **`/schedule/task/all`** → `ScheduleTaskAllVO{nextPageToken, nextSyncToken, scheduleTask:[<flat task>]}`; request `ScheduleTaskDTO` carries **`nextPageTokens` (plural)** + `maxResults` (default 20). Asymmetry is load-bearing (§8).
- **`/schedule/sort`** ↔ `ScheduleSortDTO{taskListId,title,lastModify,content}` (note `lastModify`, no trailing 'd', §8); `getScheduleSort` → `GetScheduleSortVO{…,nextIndexNumber}`.
- **ResubmitCheck** on `addScheduleTaskGroup`/`addScheduleTask` uses the default 1 s interval (no override).
- **STARTSYNC delivery:** UB already nudges the device with a FILE-SYN `ServerMessage` STARTSYNC (the existing `internal/sync/notifier.go:Notify`, invoked by `caldav.Backend` on task writes). In `server` mode, re-emit that same payload via the 1c registry.
- **Task IDs:** `String` in request DTOs, `Long` in response VOs (§8) — convert at the mapping boundary; do not unify.

**Decision — single synthesized task group (Option A).** UB exposes exactly one task list: `taskListId` = a stable constant (e.g. `"default"`), `title` = the CalDAV collection name (`cfg.CalDAVCollectionName`). `/group/all` returns just it; every task maps to it (`Task.TaskListID` defaults to `"default"`). Device group create/update/delete/clear are accepted as well-formed success **no-ops** (UB has one collection). This matches UB's single-collection CalDAV model and needs no schema change. **Future work (deliberately deferred):** multi-collection support — a `task_lists` table in `taskdb`, persisted device group CRUD, and multiple CalDAV collections — is its own future build session, scoped in `docs/future-work/multi-collection-task-lists.md` and memory `project_ub_multicollection_future`. 1d must not bake in assumptions that block it: keep group handling behind a small `groups` seam (a `GroupProvider` interface with a single-group impl) so the future table swaps in without touching task handlers.

<!-- START_SUBCOMPONENT_D (tasks 18-24) -->
<!-- START_TASK_18 -->
### Task 18: (covered) 1b servicePassword simplification

This task is folded into 1b (Tasks 5–12): `UB_SPC_DEVICE_PASSWORD` (raw) replaces `UB_SPC_SERVICE_PASSWORD`; UB computes `md5Hex(raw)`. No separate work here — listed so the numbering matches the design-plan narrative. **Skip.**
<!-- END_TASK_18 -->

<!-- START_TASK_19 -->
### Task 19: ResubmitCheck dedup

**Verifies:** spc-phase-1.AC4.4

**Files:**
- Create: `internal/spcserver/dedup/dedup.go`
- Test: `internal/spcserver/dedup/dedup_test.go`

**Implementation:** `Checker` with `Seen(userID, endpoint string, body []byte) bool` — key = `userID + "|" + endpoint + "|" + sha256hex(body)`; `sync.Map` (or mutex+map) storing insert time; returns true if an unexpired identical key exists, else records and returns false. Default TTL 1 s (`docs/spc-protocol.md` §4). Background GC ticker purges expired keys. Cite `ResubmitCheck.java`.

**Testing:** first call false, immediate repeat true; after TTL, false again; different body/endpoint/user → independent.

**Verification:** `go test -C /home/sysop/src/ultrabridge ./internal/spcserver/dedup/`

**Commit:** `feat(spcserver): ResubmitCheck in-memory dedup`
<!-- END_TASK_19 -->

<!-- START_TASK_20 -->
### Task 20: Schedule DTOs/VOs

**Verifies:** (types for AC4.1–AC4.3; no test of its own)

**Files:**
- Create: `internal/spcserver/dto/schedule.go`

**Implementation:** define, with verbatim field names + source citations:
- `SPCTask` (flat task; mirror `client.go:232-255` field tags exactly).
- `ScheduleTaskAllVO` (embeds BaseVO; `nextPageToken string`, `nextSyncToken int64`, `scheduleTask []SPCTask`); `ScheduleTaskDTO` request (`nextPageTokens string` plural, `maxResults string`).
- `ScheduleTaskGroupVO` (embeds BaseVO; `pageToken string`, `scheduleTaskGroup []ScheduleGroup`); `ScheduleGroup{taskListId,userId(int64),title,lastModified(int64),isDeleted}`.
- Group create/update/clear DTOs (`AddScheduleTaskGroupDTO{taskListId,title,createTime,lastModified}` etc. — verify field names against cfr), `ScheduleSortDTO{taskListId,title,lastModify(int64),content}`, `GetScheduleSortVO{…,nextIndexNumber int}`.
- Single-task VOs return Long `taskId`/`taskListId` (§8 String-in/Long-out).

**Verification:** `go build -C /home/sysop/src/ultrabridge ./internal/spcserver/...`

**Commit:** `feat(spcserver): schedule DTOs/VOs`
<!-- END_TASK_20 -->

<!-- START_TASK_21 -->
### Task 21: Task mapping (taskstore.Task ↔ SPCTask)

**Verifies:** spc-phase-1.AC4.3

**Files:**
- Create: `internal/spcserver/mapping/task.go`
- Test: `internal/spcserver/mapping/task_test.go`

**Implementation:** `TaskToSPC(t taskstore.Task) dto.SPCTask` and `SPCToTask(s dto.SPCTask) taskstore.Task`, using existing `taskstore` helpers (`MsToTime`/`TimeToMs`, `SupernoteStatus`/`CalDAVStatus`, `GenerateTaskID`, null handling). Independent of `internal/tasksync/supernote` (Phase 5 deletes that package) — replicate the small mapping here, citing it as the reference. Preserve: `completedTime` = creation time (set to now on create if unset), `lastModified` = completion time; `isDeleted` "Y"/"N"; sort defaults as in `RemoteToSPCTask`. New tasks lacking an ID get an MD5 id (title+timestamp).

**Testing:** round-trip `TaskToSPC(SPCToTask(s))` preserves fields; status casing maps both ways; completed task sets `completedTime`/`lastModified` per the quirk; soft-deleted task carries `isDeleted="Y"`.

**Verification:** `go test -C /home/sysop/src/ultrabridge ./internal/spcserver/mapping/`

**Commit:** `feat(spcserver): task DTO mapping`
<!-- END_TASK_21 -->

<!-- START_TASK_22 -->
### Task 22: Schedule handlers (groups, tasks, sort, summary stubs)

**Verifies:** spc-phase-1.AC4.1, AC4.2, AC4.3, AC4.7

**Files:**
- Create: `internal/spcserver/handlers/schedule.go`
- Create: `internal/spcserver/groups/groups.go` (`GroupProvider` interface + single-group impl — the future-multi-collection seam)
- Test: `internal/spcserver/handlers/schedule_test.go`

**Implementation:** handler struct holds the `taskstore.Store` (the `caldav.TaskStore`/`taskdb` store), `GroupProvider`, dedup `Checker`, and socket registry. All routes JWT-protected (1b middleware); `userId` from context.
- Groups: `POST /group/all` → `ScheduleTaskGroupVO` with the one synthesized group; `POST/PUT/DELETE /group`, `POST /group/clear`, `GET /group/{taskListId}` → well-formed success no-ops (Option A).
- Tasks: `POST /task/all` → list from `store.List`, map via T21, paginate 20 by `lastModified` ASC, set `nextPageToken` when more remain + `nextSyncToken` (max `lastModified`); request `nextPageTokens` advances. `POST /task` (create; dedup via T19), `PUT /task` (update), `PUT /task/list` (bulk), `DELETE /task/{taskId}` (soft delete), `GET /task/{taskId}`.
- Sort: `POST/PUT /sort`, `DELETE /sort/{taskListId}`, `POST /api/file/query/schedule/sort` → `GetScheduleSortVO` (sort state can be a stored/echoed stub since UB doesn't reorder; return well-formed values).
- Summary stubs: `POST /api/file/query/summary/{hash,group,id}` → well-formed empty success (AC4.7).

**Testing (in-memory taskdb):** seed tasks → `/task/all` returns them mapped + paginated (21 tasks → page1 has token, page2 empties it); create → row appears + dedup blocks immediate dup; update/delete reflected; `/group/all` returns the one group; summary stubs return success.

**Verification:** `go test -C /home/sysop/src/ultrabridge ./internal/spcserver/handlers/`

**Commit:** `feat(spcserver): schedule (group/task/sort) handlers + summary stubs`
<!-- END_TASK_22 -->

<!-- START_TASK_23 -->
### Task 23: STARTSYNC notifier over the socket registry

**Verifies:** spc-phase-1.AC4.5

**Files:**
- Create: `internal/spcserver/notify/notifier.go`
- Modify: `cmd/ultrabridge/main.go`
- Test: `internal/spcserver/notify/notifier_test.go`

**Implementation:** `SocketNotifier` implementing the same interface the CalDAV backend expects (`caldav.SyncNotifier` — `Notify(ctx) error`, plus any others on that interface; verify its method set). `Notify` resolves `spc_user_id` and calls `registry.Emit(userID, "ServerMessage", <FILE-SYN STARTSYNC payload>)` matching `internal/sync/notifier.go:144-151`'s JSON. In `cmd/ultrabridge/main.go`, when `cfg.SPCMode == "server"`, construct the SPC server first, then pass its `SocketNotifier` (wrapping `Server.SocketRegistry()`) as the `notifier` into the CalDAV backend / task service **instead of** the `sync.Notifier` client. In `client` mode, wiring is unchanged (regression-safe).

**Testing:** `Notify` Emits a well-formed `42["ServerMessage",{…STARTSYNC…}]` to a fake registry; no-op/no-error when no connection.

**Verification:** `go test -C /home/sysop/src/ultrabridge ./internal/spcserver/notify/`; `go build`/`go vet ./...`.

**Commit:** `feat(spcserver): STARTSYNC notifier over socket registry`
<!-- END_TASK_23 -->

<!-- START_TASK_24 -->
### Task 24: Full task-sync device acceptance (tier-2)

**Verifies:** spc-phase-1.AC4.6

**Steps (config + device, no code):** with UB in `server` mode and NPM flipped to UB (`/api/*` + `/socket.io/*`):
1. Device sync → device's task UI shows the existing UB tasks under the one list.
2. Edit a task on the device → confirm the change lands in UB (web UI / CalDAV) within ~5 s.
3. Edit/add a task in UB's web UI (or via a CalDAV client) → confirm the device receives STARTSYNC and pulls the change within ~5 s.
4. Flip NPM back to roll back.

**Acceptance:** all four behaviors hold; `client` mode unaffected when flipped back.

**Phase-1 caveat (watch for, don't pre-build):** Phase 1 implements tasks only — the device's full sync also hits file endpoints (`/api/file/2/files/synchronous/start`/`end`, `list_folder`, `query_v3`, `capacity/query`, etc.) which return 404 until Phase 2. Expect file-sync errors in the device/UB logs. **If** a file-sync failure aborts the *whole* sync and blocks task sync (unknown — 0b showed file+task ops interleaved), pull forward minimal file stubs as a new task: `synchronous/start`/`end` → success, `list_folder`/`list_folder_v3` → empty list, `capacity/query` → a canned figure. Decide based on observed device behavior; do not build speculatively.

**Commit:** docs-only (`docs(spcserver): record 1d task-sync result`).
<!-- END_TASK_24 -->
<!-- END_SUBCOMPONENT_D -->

**1d exit state (= top-level Phase 1 exit state):** `go build -C /home/sysop/src/ultrabridge ./...` and `go test -C /home/sysop/src/ultrabridge ./...` green; `go vet` clean. Full end-to-end task sync works in `server` mode (device ↔ UB ↔ web/CalDAV); `client` mode is regression-safe; `internal/tasksync/supernote/` and `internal/sync/notifier.go` are untouched (still used in `client` mode). This satisfies the design plan's Phase 1 exit state; Phase 2 (file listing) starts from here after a `/clear`.

---

## Out of scope for Phase 1 (forward notes, no implementation here)

- **Multi-collection / multiple task lists.** 1d uses a single synthesized group. Future build session: `task_lists` table in `taskdb`, persisted device group CRUD, and multiple CalDAV collections. Seam: `internal/spcserver/groups.GroupProvider`. Tracked in `docs/future-work/multi-collection-task-lists.md` + memory `project_ub_multicollection_future`.
- **Engine.IO permessage-deflate.** Skipped in 1c (uncompressed fallback); minimal no-context-takeover impl only if T17 shows the device requires it.
- **Socket `sign` validation** (`SignVerifierSocketIO`) — accepted-and-ignored in 1c; token verification is the gate. Validate later only if needed.
- Files, OSS, recycle, search, catalog cutover — Phases 2–5.
