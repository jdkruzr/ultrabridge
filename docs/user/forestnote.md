# ForestNote Sync

ForestNote uses UltraBridge's `/sync/v1` endpoint and Rhizome-style sync primitives to mirror notebook data with the server.

## Configure UltraBridge

1. Enable a ForestNote source in **Settings -> Devices**. Sync is off by default, and enabling it requires a restart — `/sync/v1` is only served while a ForestNote source is enabled.
2. Confirm the main app URL is reachable from the device. The endpoint sits behind the same authentication as the web UI, so the device needs your UltraBridge credentials.
3. Use the ForestNote app's sync setup to point at UltraBridge's `/sync/v1` endpoint.

The route is served from the main app listener, usually:

```text
https://ub.example.com/sync/v1
```

UltraBridge speaks sync schema **v4**. A v3 client is still accepted during the rollout grace window; v2 and older are rejected with HTTP 409 — update the app.

## What Syncs

- Notebooks, folders, pages, strokes, and tombstones.
- Text boxes and page templates.
- Per-notebook page aspect, so a note keeps its native shape across devices instead of being distorted to fit.
- Client OCR rows from ForestNote.
- Server-authored OCR text, which flows back down to devices.

Tasks created in ForestNote reach UltraBridge over **CalDAV**, not `/sync/v1`. The `X-FORESTNOTE-*` properties on those tasks are stored as structured provenance (notebook, page, native `forestnote://` link) and surfaced through the web UI, REST, and MCP.

## Device Management

**Settings -> Devices** lists registered ForestNote devices. From there you can:

- Name each device — the name is stored separately from the device-reported name and is never overwritten by a sync.
- Prune inactive devices.
- Compact the relay log, either on demand or automatically on a schedule (off by default; interval and stale-device horizon are configurable on the source).
- Inspect device watermark state.

Use pruning conservatively. Compacting while stale devices remain registered can keep more relay history than expected; pruning active devices can cause unnecessary resync (they simply re-register on their next sync).

## OCR And Search

ForestNote client OCR is indexed for search and RAG. UltraBridge keeps server-owned page text behavior separate so device OCR can enrich search without turning every client OCR update into a render-triggering server edit.

The pipeline status bar shows ForestNote's durable **indexed** count (live pages carrying OCR text, surviving restarts) alongside a this-session processed figure. ForestNote has no global start/stop controls — Re-OCR is per-notebook from the Files tab.

## Files UI

The ForestNote Files tab supports notebook navigation, page rendering, PDF export (**Download**), per-notebook **Re-OCR**, and **Delete**. Deleting pushes tombstones to your devices on their next sync — it is a real delete, not a local hide. Last write wins: if the notebook still exists on a device and you edit it there afterwards, it comes back. To delete permanently everywhere, delete it on the device too.
