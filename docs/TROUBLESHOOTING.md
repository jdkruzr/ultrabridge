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

### MCP Sidecar Cannot Authenticate

1. Create a bearer token in **Settings -> Integrations -> MCP Tokens**.
2. Put it in an untracked `.env`:

   ```bash
   UB_MCP_API_TOKEN=...
   ```

3. Start the sidecar profile:

   ```bash
   docker compose --profile mcp up -d --build ub-mcp
   ```

## CalDAV

### Client Shows An Empty Collection

1. Use the discovery URL when possible:

   ```text
   https://your-host/.well-known/caldav
   ```

2. Some clients require the direct collection URL with a trailing slash:

   ```text
   https://your-host/caldav/tasks/
   ```

3. Confirm credentials match your UltraBridge user or bearer-token flow.
4. If the collection exists but tasks are missing, check the Tasks tab and logs for soft-delete or sync errors.

### Attachments Do Not Appear In A Client

1. Verify the task has an attachment in UltraBridge or the MCP task output.
2. Inspect the served CalDAV object and look for an `ATTACH` URI.
3. Fetch the attachment URL directly while authenticated. It should return `200`, a content type, and a content length.
4. Some CalDAV clients ignore task attachments even when the server presents valid `ATTACH` properties.

## Sources

### Source Does Not Appear In Files

1. Confirm the source exists and is enabled in **Settings -> Devices**.
2. Check that the container can read/write the configured path.
3. Watch the Logs tab while pressing Scan, Reprocess, or the source-specific action.

### Supernote Sync Does Not Start

1. Enable **Settings -> Devices -> UB-as-SPC Device Sync Server -> Mode: server** and restart if prompted.
2. Publish the SPC listener port, normally `8089`.
3. Use a dedicated reverse-proxy hostname for the Supernote device. Do not share the web UI hostname.
4. Preserve the `Host` header and WebSocket upgrade for `/socket.io/`.
5. Confirm the SPC file root points at the full Supernote storage root, not only the `Note/` folder.

### Boox Uploads Do Not Arrive

1. Configure the Boox WebDAV URL with a trailing slash:

   ```text
   http://your-host:8443/webdav/
   ```

2. Confirm the Boox source path is mounted into the container.
3. Uploaded files should land under the configured path, typically below an `onyx/` tree.
4. Use the Boox maintenance actions in **Settings -> Devices** to scan disk, reconcile dates, or remove auto-named junk notebooks.

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
