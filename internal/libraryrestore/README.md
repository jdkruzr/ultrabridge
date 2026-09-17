# Candidate authoritative library restore

Mounted only by `cmd/assetlab --reader --reader-assets --reader-enrollment
--reader-restore-sync`. Not wired into the production service.

Assembly is now owned by `libraryhost`, not duplicated in the executable. Its
request admission and fresh-worker lifetime surround publication. Production
hosts can supply transactional derived-data invalidation and a post-commit cache
hook; tests verify failure rollback before cache invalidation. Closed worker
owners cannot be restarted by late publication calls.

A deliberate restore replaces the one author's shared library; it is not a merge
with newer offline edits. The generation fence remains the normal row/asset gate.

- `POST /sync/restore/v1/publish`: native Basic-admin authorization, exact request
  ID/expected generation/snapshot digest/publisher. Device keys cannot publish.
- `GET /sync/restore/v1/state`: authenticated device discovery, including stale keys.
- `GET /sync/restore/v1/publications/<request ID>`: the publishing device can read
  its own current receipt without storing or resubmitting the server login password.
  Another device receives 404; a superseded publication receives 409. Read-only.
- `GET /sync/restore/v1/assets/<current snapshot>` and chunk reads: only that baseline,
  with its exact `generation` query parameter. No old-key writes or general browsing.
- `POST /sync/restore/v1/adopt`: old device key plus generation, snapshot, fresh actor
  and already durably saved token hash. Exact retries return the same baseline.

Original bytes use the resumable Rhizome asset channel. The interchange ZIP holds
`manifest.json`, `rows.jsonl`, and complete `assets/<sha256>` book entries. Uploads
are never opened as SQLite. Validation constructs a private database from known
DDL, retains original operation provenance, and uses the normal reader domain
gate. Missing books remain incomplete. Limit: 64 GiB compressed/expanded total,
100,000 entries, bounded rows/manifest/chunks. Staging requires additional disk
space; I/O or validation failure leaves the live generation/content untouched.

The host must cancel and JOIN library-derived workers around publication, then
restart them against committed state. The content allowlist excludes unrelated
UB sources/settings/tasks/authentication. Original immutable asset bytes may remain
unreferenced after replacement; garbage collection is separate.

Clients must validate a fresh local stage and persist its new identity/key before
adoption, close the old owner, and atomically select the replacement before enabling
ordinary sync. Old outboxes must never be replayed. Native UI/startup orchestration
is qualified against this candidate; production rollout remains gated. The detailed plan and Kotlin/Android evidence
live in Alexandria's `docs/design-plans/2026-09-16-alexandria-authoritative-restore-sync.md`.
