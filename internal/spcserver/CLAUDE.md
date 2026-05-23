# internal/spcserver

Last verified: 2026-05-23

Device-facing reimplementation of the Supernote Private Cloud (SPC) protocol so an unmodified Supernote device talks to UltraBridge as if it were the real SPC server.

**Status:** Phase 1 complete and validated on real hardware (Supernote Nomad SN078C10034074, 2026-05-23) — auth/login, the Engine.IO socket, and bidirectional task sync (incl. instant web→device push) all work. Phase 2 (file listing + capacity, **read path**) is code-complete and validated by a no-device login→curl loop (2026-05-23); device acceptance is the remaining 2d step. OSS download/upload/recycle/search are Phases 3–5 (not built). Gated by `UB_SPC_MODE` (default `client` = no listener; `server` = bind `:8089`).

## Layout

- `server.go` — HTTP + Engine.IO server on one listener; `registerRoutes` wires everything. `settingstore.go` — notedb-backed `auth.SettingStore`.
- `envelope/` — leaf package: `BaseVO` + `WriteJSON`/`WriteError`. **Lives here, not in package spcserver, to break the import cycle** (handlers/auth/server all import it; it imports nothing internal).
- `auth/` — HS256 JWT mint/verify + `x-access-token` middleware (+ userId harvest).
- `login/` — randomCode store, password recipe (`sha256Hex(md5Hex(raw)+code)`), `ResolveUserID`.
- `dto/` — request DTOs / response VOs, field names verbatim from decompiled source.
- `mapping/` — `taskstore.Task ↔ dto.SPCTask` at the controller boundary (no second store).
- `socketio/` — Engine.IO v3 codec, connection registry, websocket handler.
- `groups/` — `GroupProvider` seam; single synthesized group today (multi-collection deferred).
- `dedup/` — ResubmitCheck (1s TTL). `notify/` — STARTSYNC notifier over the socket registry.
- `fileids/` — leaf package owning the `spc_file_ids` table (path↔id Long registry + lazy MD5 cache); migrated package-locally in server mode (à la `mcpauth.Migrate`), **not** by `notedb.Open`.
- `capacity/` — leaf package: du-style usage Meter with a 60s cache. **Its own package (not `capacity.go` at the root) to avoid the handlers↔spcserver import cycle** — same reason as `envelope/`.
- `handlers/` — equipment, login/challenge/boot, schedule (group/task/sort), summary stubs, and `files.go` (Phase 2 read path: sync-session, list_folder/_v3, query_v3/by-path_v3, capacity/query, get_space_usage, create_folder/deleteApi stubs).

This package does **not** own storage (tasks → `taskdb`; files/notes → notestore) or human UI (`internal/web`).

## Contracts / invariants

- **Flat envelope.** VOs embed `envelope.BaseVO` so `success`/`errorCode`/`errorMsg` serialize at top level, never under `data`.
- **JWT secret = `Constant.SECRET`** (long ~280-char, `Constant.java:46`), not the 32-char `JWT_SECRET`. Verify is stateless (no Redis, no `exp` enforcement) — real device tokens have no `exp`.
- **`client` mode is regression-safe**: no listener, UB behaves exactly as before. Must stay true.
- **DTO/VO field names verbatim** from decompiled source; cite `<FQN.java>:<line>`. Gotchas: `nextPageTokens` (plural request) vs `nextPageToken` (singular response); `lastModify` (no `d`) in sort DTOs; String-in / Long-out task ids (§8); file EntriesVO uses snake_case wire keys (`path_display`/`content_hash`/`is_downloadable`) with no `@JsonProperty`.
- **File listing reads a dedicated `UB_SPC_FILE_ROOT`, not the OCR `NotesPath`.** The device browses the Supernote storage root (its native `Note/Document/EXPORT/SCREENSHOT/INBOX/MyStyle` layout) directly off the filesystem — `notestore` is not used (it knows only the `Note` subtree and stores SHA-256, while the device wants MD5). Empty `FileRoot` ⇒ listing inert.
- **The device sees only the Supernote source.** The Boox source and its WebDAV tree are invisible to the device — never listed, never merged. This is deliberate ("look exactly like real SPC to the device").
- **`content_hash`/`md5` is MD5**, computed lazily and cached in `spc_file_ids` keyed on (size, mtime). **Folder & file ids are persisted Longs** (the device addresses folders by parent id in `list_folder` and by id in `query_v3`); a null/0 id means the root. Page/listing reads never mutate the filesystem in Phase 2 (`create_folder`/`deleteApi` are canned-success stubs).

## Socket.IO gotchas (hard-won on hardware 2026-05-23 — see memory `project_spc_socketio_breakthrough`)

- **Server speaks first.** The device's `io.socket` client connects to the default namespace `/` and does NOT send a Socket.IO CONNECT — it waits to RECEIVE `40` from the server. UB sends `0{...}` (EIO open) **then `40` proactively**. Without it the client never fires `connect` and reconnect-loops forever (looks healthy server-side).
- **Heartbeat is client-driven.** The device sends Engine.IO ping `2`; UB replies pong `3`. UB does **not** send server pings. (Device does NOT send `ratta_ping` — earlier 0b note was wrong.)
- **Event name routes the push, not msgType.** Task sync nudge = emit the **`to-do`** event (device's `TaskService` binds it; `onReceive` → `startTaskSync` unconditionally). `ServerMessage` is the FILE channel; `digest` is digests.
- **`taskListId` must be numeric** (device parses Long); page tokens must be `omitempty` (empty string → device pagination loop).
- **`PUT /schedule/task/list`** body is the `UpdateScheduleTaskListDTO` wrapper (`{taskListId, updateScheduleTaskList:[...]}`), not a bare array.

## Spec source

Reverse-engineered from `/home/sysop/spc-rev/cfr-decrypted/` (CFR-decompiled SPC `supernote-service.jar` v2.1.4) and, for client-side behavior, jadx-decompiled device apps (`SupernoteScreencast.apk` socket lib, `SupernoteTask.apk` task sync). Protocol summary: `docs/spc-protocol.md`. Phased plan: `docs/design-plans/2026-05-15-ub-as-spc-refactor.md`, `docs/implementation-plans/spc-phase-1.md`. When SPC behavior surprises you, read the `.java` (server) or decompiled client first — do not guess.
