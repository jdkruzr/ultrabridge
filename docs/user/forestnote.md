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

UltraBridge speaks ForestNote sync schema **v5**. ForestNote 1.8's v4 schema remains accepted for one release; v3 and older are rejected with HTTP 409 — update the app. During the grace window UltraBridge fills v4 strokes as Fountain Pen v1 and uses their legacy page aspect, so an old device never produces malformed v5 rows.

## What Syncs

- Notebooks, folders, pages, strokes, and tombstones.
- Text boxes and page templates.
- Exact creator-page width and height, so the usable canvas stays identical across differently shaped devices.
- Portable brush identity, renderer version, deterministic texture seed, and optional pen tilt/orientation. Viwoods and Boox firmware styles are only live previews; ForestNote's committed renderer is the cross-device authority.
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
