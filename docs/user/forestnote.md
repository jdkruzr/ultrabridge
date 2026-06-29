# ForestNote Sync

ForestNote uses UltraBridge's `/sync/v1` endpoint and Rhizome-style sync primitives to mirror notebook data with the server.

## Configure UltraBridge

1. Enable a ForestNote source in **Settings -> Devices**.
2. Confirm the main app URL is reachable from the device.
3. Use the ForestNote app's sync setup to point at UltraBridge's `/sync/v1` endpoint.

The route is served from the main app listener, usually:

```text
https://ub.example.com/sync/v1
```

## What Syncs

- Notebooks, folders, pages, strokes, and tombstones.
- Text boxes and page templates.
- Client OCR rows from ForestNote.
- Task provenance and page/native links for tasks created from notebook content.

## Device Management

**Settings -> Devices** lists registered ForestNote devices. From there you can:

- Prune inactive devices.
- Compact the relay log.
- Inspect device watermark state.

Use pruning conservatively. Compacting while stale devices remain registered can keep more relay history than expected; pruning active devices can cause unnecessary resync.

## OCR And Search

ForestNote client OCR is indexed for search and RAG. UltraBridge keeps server-owned page text behavior separate so device OCR can enrich search without turning every client OCR update into a render-triggering server edit.

## Files UI

The ForestNote Files tab supports notebook navigation, page rendering, PDF export, delete/recovery flows, and Re-OCR controls where the source has the required data.
