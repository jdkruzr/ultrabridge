# internal/spcserver

Device-facing implementation of the Supernote Private Cloud (SPC) protocol so an unmodified Supernote device can talk to UltraBridge as if it were the real SPC server.

**Status:** Stub. Phase 0 of the UB-as-SPC refactor created this directory and protocol notes; no Go source yet. First source files (`server.go`, `envelope.go`) land in Phase 1a.

## Scope

This package owns:

- HTTP listener for `/api/*` SPC endpoints (REST)
- Engine.IO v3 server on the file+digest channel (and optionally the task channel — pending Phase 0b confirmation)
- JWT auth middleware (`x-access-token` header, HS256 with the long `Constant.SECRET` value)
- DTO/VO types matching SPC's Java DTOs verbatim
- ResubmitCheck-style in-memory dedup (sync.Map keyed on `userId+endpoint+sha256(body)`)
- OSS signature primitive (`/api/oss/upload`, `/api/oss/download` URLs)

This package does **not** own:

- Authoritative storage. Files live in the existing notestore; tasks live in the existing taskdb; embeddings/search/RAG/chat untouched.
- Human-facing UI. UB's `internal/web` is the human surface; SPC's Vue UI is dropped (see design plan's "Scope dropped").

## Conventions

1. **DTO/VO field names match the decompiled SPC source verbatim**, snake_case included where present (`content_hash`, plural `nextPageTokens` in requests vs singular `nextPageToken` in responses, `lastModify` without trailing `d` in ScheduleSortDTO, etc.). See `docs/spc-protocol.md` §8 for the full gotcha list.

2. **Envelope is flat, not nested.** `BaseVO` has three fields: `success` (bool), `errorCode` (string), `errorMsg` (string). Payload fields sit alongside these, **not** under a `data` key. VOs extend `BaseVO`.

3. **JWT signing secret is `Constant.SECRET`** (the long ~280-char string from `Constant.java:46`). The 32-char `Constant.JWT_SECRET` exists but is not the signing secret. Terminal tokens (those with `equipmentNo` set) are non-expiring at the JWT level — effective TTL comes from server-side state, not `exp` claim.

4. **All handlers behind JWT middleware** except the explicit allow-list: `/api/official/user/account/login/equipment`, `/api/official/user/account/login/new`, `/api/equipment/bind/status`, `/api/file/query/server`, `ratta_ping` keepalive. Other endpoints require a valid `x-access-token`.

5. **ResubmitCheck dedup TTL defaults to 1 second** per the annotation default; per-endpoint overrides come from the method-level annotation in `cfr-decrypted/`.

6. **Error codes match the SPC enum.** `E0330` ("NextSyncToken timeout") in particular is a contract: the device falls back to a full pull when it sees it.

## Spec source

Everything in this package is reverse-engineered from `/home/sysop/spc-rev/cfr-decrypted/` (CFR-decompiled SPC `supernote-service.jar` v2.1.4.RELEASE). When implementing a new endpoint or DTO, cite `<FQN.java>:<line>` in the code comment that pins the spec to the source. Read `docs/spc-protocol.md` for the protocol-level summary.

When SPC's behavior surprises you, read the corresponding `.java` first — do not guess.
