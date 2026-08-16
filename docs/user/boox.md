# Boox Setup

Boox devices export notes into UltraBridge over WebDAV. UltraBridge parses Boox `.note` files, renders pages, runs OCR when configured, indexes text, and can extract tasks from colored handwriting.

## Configure UltraBridge

1. Add a Boox source in **Settings -> Devices**.
2. Set a writable notes path that is mounted into the container.
3. Configure optional Boox maintenance and import settings.
4. Configure OCR in **Settings -> AI & Processing** if you want handwriting recognition. The Boox-specific OCR prompt override also lives there — it is separate from the red-ink to-do prompt, which lives with the Boox source in **Settings -> Devices**.

## Configure The Device

Set the Boox WebDAV server URL to:

```text
http://<host>:8443/webdav/
```

Use your UltraBridge username and password. The trailing slash matters on some firmware.

Uploaded files are normally stored below an `onyx/` path that records model, note type, and folder metadata.

## Maintenance

The Devices settings page includes Boox maintenance actions:

- Scan disk and enqueue untracked files.
- Reconcile created dates from filename prefixes.
- Delete auto-named junk notebooks, including source files and version archives.

The import path and file-type toggles are configured in Settings, but the **Import** and **Migrate Imports** triggers are on the Boox **Files** tab, alongside **Retry Failed**. The Boox worker's start/stop controls live in the persistent pipeline status bar on every page.

## When A Note Fails

- **Empty notebook** (created on the device, never drawn in): recorded as **skipped**, not failed — there is nothing to render or index. Use Unskip on the file row to force a retry.
- **Transient OCR errors** (backend down, timeout, 5xx/429): retried automatically with backoff — 1 minute, doubling to a 30-minute cap, up to five attempts. Nothing to do but wait.
- **Permanent errors** (a 4xx from the OCR API, a truncated `.note` archive): marked **failed**. Fix the cause, then press **Retry Failed** on the Boox Files tab.
- **Stopping the worker mid-note** leaves the job in progress; it is reclaimed the next time the worker starts.

## Red-Ink Tasks

When enabled, UltraBridge runs a second OCR pass looking for colored handwriting and creates tasks from recognized items. The color prompt is configurable, so the same mechanism can be adapted to blue, red, or another marker convention.

Tasks created this way flow through the same task store as web, CalDAV, and MCP tasks. Each task's description carries an "Open:" link back to the source page in UltraBridge — set the externally reachable base URL in **Settings -> Devices** (Boox section) or CalDAV clients will render it as plain text instead of a clickable link.

## Limitations

Boox is receive-only. UltraBridge cannot push deletes or renames back to the Boox device.
