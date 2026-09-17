# Alexandria shared-library production host

2026-09-17: implemented and qualified against disposable databases. **Not deployed
or activated on the live server by this checkpoint.** Return to the
[Alexandria integration plan](../../../AragoniteAlexandria/docs/design-plans/2026-09-16-alexandria-server-integration.md).

## Configuration and compatibility

The existing `forestnote` source row owns the library. `config_json` gains a
restart-required, default-off `shared_library` boolean. Example configuration:

```json
{"batch_limit":256,"shared_library":true,"compaction":false}
```

Merge these fields with the source's existing configuration; do not discard other
settings. Only one enabled ForestNote/Alexandria source may own these tables.
This is not a global `sync_enabled` setting or an automatic deployment migration.

- Off: existing account-authenticated writer-only `/sync/v1` remains unchanged.
- On: one `libraryhost.Host` mounts enrolled `/sync/v1`, capabilities, device
  enrollment, assets, reader search and authoritative restore together. There is
  no alternate old row endpoint. Device keys are site-bound; normal account/MCP
  credentials cannot bypass generation admission. Enrollment and publication
  accept the existing account credentials, not general-purpose MCP bearer tokens.
- An existing legacy site ID cannot silently enroll. Explicit legacy adoption is
  a distinct existing identity operation; a fresh Alexandria replica enrolls with
  its own site ID. Ordinary enrollment still merges, never publishes a restore.
- Writer-only periodic and manual compaction are disabled in shared mode. Startup
  rejects `shared_library:true` with `compaction:true` rather than sweeping mixed
  receipts using the old compactor.
- Successful activation records a sticky local marker (also retained if subsequent
  startup fails). This version refuses to resume legacy mode against that DB.
  **An older binary cannot enforce a marker it does not know about.** Do not use
  an old executable or flip the flag as an in-place rollback procedure. Preserve
  both databases and use matched software/pre-cutover backups for deliberate rollback.

## Replacement lifetime

Device requests, source manual operations, whole web-service notebook deletion
(including derived cleanup), and global ForestNote embedding backfill all join
the host's admission boundary. Non-ForestNote backfill continues independently.
Publication joins old work before replacing rows. Fresh writer bridge, projection
and search workers start after success **or rollback**, without reusing old queues.

`forestnote://` search/embedding rows are removed inside the replacement transaction.
The matching in-memory vector cache is invalidated only after commit. Other source
rows, vectors, settings, tasks and account credentials are outside this cleanup.
Saved writer text is rebuilt in bounded worker batches from typed/client/server
transcriptions, without rendering, OCR upload or newly authored sync operations.
Ordinary subsequent processing retains the configured server OCR behavior.
The old startup transcription-authoring backfill does not run in shared mode.

## Executable and source qualification

`TestProductionBinaryLibraryCutoverAndRestore` builds and launches the actual
`cmd/ultrabridge` executable over loopback TCP with isolated notedb/taskdb paths and
no inherited UB environment. It checks legacy sync with capability off, explicit
cutover, refused legacy identity reuse, account/device separation, mixed notebook
and reader-row exchange, asset stage/chunk/complete, reader search routing,
publication, server restart, receipt recovery, stale-key rejection, peer adoption,
and unrelated-source preservation. No external OCR endpoint is contacted.

Source-level tests additionally force replacement failure after transactional
cleanup, check unchanged cached vectors, worker replacement after rollback, admitted
backfill joining, saved-transcription rebuild without OCR/authorship, stopped-writer
rejection, and legacy-mode downgrade refusal. Service tests ensure deletion admission
extends through search/embedding cleanup. Existing host tests cover supersession,
interrupted responses and shutdown. Run:

```sh
go test ./...
go vet ./...
go test -race ./cmd/ultrabridge ./internal/source/forestnote ./internal/service ./internal/libraryhost ./internal/libraryrestore ./internal/rag ./internal/syncbridge
```

## Live rollout is still separate

Before activation, take fresh consistent backups of production notedb and taskdb;
the Sep16 backups are historical safeguards, not a substitute for fresh ones.
Deploy with the capability off first and verify legacy behavior. Coordinate explicit
cutover with the remaining device fleet; unchanged old clients are intentionally
not accepted after the enrolled-generation boundary is active. Do not exercise a
destructive restore against the user's live database for qualification.

Real Go/Mini interoperability, tiny Go 6 II acceptance and release signing remain
later gates. A web reader, server rebranding and README polish are not part of this
runtime checkpoint.
