# UltraBridge Troubleshooting

This page covers current user-facing deployments. Historical design and test plans may mention older MariaDB-backed SPC integration; current UltraBridge releases use SQLite and the built-in SPC server.

## Auth And Access

### Web UI Login Fails

1. Check the health endpoint:

   ```bash
   curl http://localhost:8443/health
   ```

2. On a fresh install, visit `/setup` and create the first user.
3. If credentials are lost, seed a replacement user:

   ```bash
   docker run --rm -v ./ultrabridge-data:/data ultrabridge:latest \
     seed-user myusername "new-password"
   ```

4. Turn on **Settings -> System -> Verbose API Logging** to surface auth failure detail in the Logs tab and container logs.

### Claude.ai Authorization Fails

1. Claude.ai must be able to reach the public `/mcp` URL. `localhost` only works for clients running on the same machine.
2. Confirm the reverse proxy forwards to the main app listener, normally `:8443`.
3. Disconnect and reconnect the MCP server in Claude.ai after changing passwords or OAuth-related settings.
4. Check the Logs tab for OAuth or MCP auth failures.

### MCP Bearer Token Fails

1. Create a bearer token in **Settings -> Integrations -> MCP Tokens**.
2. Configure the MCP client to send `Authorization: Bearer <token>` to the main `/mcp` endpoint.
3. Confirm the reverse proxy forwards the `Authorization` header to UltraBridge.
4. Revoke and recreate the token if the token may have been copied with whitespace or shell quoting.

## CalDAV

### Client Shows An Empty Collection

1. Use the discovery URL when possible:

   ```text
   https://your-host/.well-known/caldav
   ```

2. Some clients require the direct collection URL with a trailing slash:

   ```text
   https://your-host/caldav/user/calendars/tasks/
   ```

3. Confirm credentials match your UltraBridge user or bearer-token flow.
4. If the collection exists but tasks are missing, check the Tasks tab and logs for soft-delete or sync errors.

### Client Keeps Re-Downloading Every Task

1. UltraBridge serves `CS:getctag` and `DAV:sync-token` on the task collection, and the `DAV:sync-collection` REPORT (RFC 6578). If a client still full-syncs every time, confirm your reverse proxy forwards the `REPORT` and `PROPFIND` verbs unaltered.
2. A `DAV:valid-sync-token` error is normal for a stale or unrecognized token — the client should discard it and re-enumerate once. A hard purge of deleted tasks deliberately invalidates all outstanding tokens.
3. The token advances on deletes as well as edits, so a client that never sees deletions is caching, not being under-served.

### Task Status Looks Wrong After A Device Sync

All four RFC 5545 statuses (`NEEDS-ACTION`, `IN-PROCESS`, `COMPLETED`, `CANCELLED`) round-trip natively since v1.6.0; before that, `IN-PROCESS` and `CANCELLED` collapsed to `NEEDS-ACTION`. The Supernote itself only understands two states, so on-device an in-process task shows as open and a cancelled one as completed — the real state is preserved server-side and in CalDAV.

### Attachments Do Not Appear In A Client

1. Verify the task has an attachment in UltraBridge or the MCP task output.
2. Inspect the served CalDAV object and look for an `ATTACH` URI.
3. Fetch the attachment URL directly while authenticated. It should return `200`, a content type, and a content length.
4. Some CalDAV clients ignore task attachments even when the server presents valid `ATTACH` properties.

## Sources

### Source Does Not Appear In Files

1. Confirm the source exists and is enabled in **Settings -> Devices**.
2. Check that the container can read/write the configured path.
3. Watch the **Pipeline Status** bar at the top of the page (present on every page, expandable to a per-source breakdown) while pressing Scan, Reprocess, or the source-specific action, and check the Logs tab if nothing moves.

### Supernote Sync Does Not Start

1. Tick **Settings -> Devices -> UB-as-SPC Device Sync Server -> Enable device sync server**, save, and restart the container. (There is no longer a "Mode" dropdown; internally the setting is still `spc_mode=server`, and `client` simply means the listener is off.)
2. Publish the SPC listener port, normally `8089`.
3. Use a dedicated reverse-proxy hostname for the Supernote device. Do not share the web UI hostname.
4. Preserve the `Host` header and WebSocket upgrade for `/socket.io/`.
5. Confirm the SPC file root is a dedicated, UltraBridge-owned directory (not an existing Supernote Private Cloud data dir), and that the Supernote source's notes path points inside it at `<file root>/NOTE/Note`.

### Boox Uploads Do Not Arrive

1. Configure the Boox WebDAV URL with a trailing slash:

   ```text
   http://your-host:8443/webdav/
   ```

2. Confirm the Boox source path is mounted into the container.
3. Uploaded files should land under the configured path, typically below an `onyx/` tree.
4. Use the **Database Maintenance** actions in **Settings -> Devices -> Boox** to scan disk and enqueue untracked files, reconcile `created_at` with filename dates, or delete auto-named `Notebook-N` files. If you imported files outside the notes tree, **Migrate Imports** (on the Boox Files tab) copies them in.

### ForestNote Sync Is Not Moving Data

1. Confirm a ForestNote source exists and is enabled.
2. Confirm `/sync/v1` is reachable from the device.
3. Check **Settings -> Devices** for registered ForestNote devices.
4. Use the compaction and prune controls only after confirming stale devices are no longer active.

### reMarkable Pairing Or Sync Fails

1. Confirm the reMarkable source has a writable `data_path`.
2. Check the reMarkable source/device panel in **Settings -> Devices**.
3. Ensure your reverse proxy forwards the device-facing reMarkable API routes to the main app listener.
4. If search fails on the tablet, check `/search/v1/error` traffic and UltraBridge logs before changing token or device state.

### reMarkable Uploads, Downloads, Or Deletes Fail

1. Uploads accept `.pdf` and `.epub` only, up to 512 MiB (a larger file is rejected with `413`).
2. Deleting a non-empty folder is refused deliberately — empty it first.
3. A deleted item is not gone: it moves to the tablet's own trash (restorable on-device) and disappears from UltraBridge's listing and search. This also applies to items trashed on the device.
4. Server-authored changes push a sync notification; if the tablet doesn't show the change within seconds, it isn't currently connected — it catches up on its next sync.
5. File management requires a tablet that has completed at least one modern (`/sync/v3`) sync; until then mutations are refused with a conflict error.

## OCR, Search, And Chat

### OCR Jobs Are Stuck

1. Check **Settings -> AI & Processing** for OCR provider, URL, key, model, concurrency, and max-file size.
2. Confirm the OCR endpoint is reachable from inside the container.
3. Reprocess a single page or note first; broad backfills make failures noisier.
4. For reMarkable, PDF/EPUB files are not automatically OCRed; notebook documents are the normal automatic path.

### Search Misses Expected Handwriting

1. Confirm the page has OCR text in its Files detail view.
2. Confirm source-specific OCR is enabled or manually reprocess the note.
3. For ForestNote, client OCR is indexed for search/RAG while server OCR/native text remains the render-triggering body.
4. For reMarkable, native device HWR proxying is not the same as UltraBridge server-side searchable OCR.

### RAG Falls Back To Keyword Search

1. Confirm Ollama is reachable:

   ```bash
   curl http://your-ollama-host:11434/api/tags
   ```

2. Confirm the embedding model name in Settings exactly matches the pulled model, usually `nomic-embed-text:v1.5`.
3. Run the embedding backfill from **Settings -> AI & Processing** after restoring Ollama.

### Chat Fails Or Streams Forever

1. Confirm the configured OpenAI-compatible chat endpoint is reachable:

   ```bash
   curl http://your-chat-host:8000/v1/models
   ```

2. Match the configured chat model to the model ID returned by the endpoint.
3. If a local vLLM service exits under load, configure systemd restart behavior for that service and check GPU memory fragmentation settings.

## Operations

### Rebuild After Pulling Changes

```bash
./rebuild.sh
```

or:

```bash
docker compose up -d --build
```

### Logs

- Web UI: **Logs** tab.
- Container:

  ```bash
  docker logs ultrabridge
  ```

- Optional file log defaults to `/data/ultrabridge.log` when configured.

### Backups

Back up the data directory that contains:

- `ultrabridge.db`
- `ultrabridge-tasks.db`
- Source file roots for Supernote, Boox, reMarkable, and any rendered/OCR cache paths you configured.

Stop the container before copying SQLite databases if you need a simple crash-consistent filesystem backup.
