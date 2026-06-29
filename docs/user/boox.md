# Boox Setup

Boox devices export notes into UltraBridge over WebDAV. UltraBridge parses Boox `.note` files, renders pages, runs OCR when configured, indexes text, and can extract tasks from colored handwriting.

## Configure UltraBridge

1. Add a Boox source in **Settings -> Devices**.
2. Set a writable notes path that is mounted into the container.
3. Configure optional Boox maintenance and import settings.
4. Configure OCR in **Settings -> AI & Processing** if you want handwriting recognition.

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
- Bulk import notes and PDFs from a configured import path.

## Red-Ink Tasks

When enabled, UltraBridge runs a second OCR pass looking for colored handwriting and creates tasks from recognized items. The color prompt is configurable, so the same mechanism can be adapted to blue, red, or another marker convention.

Tasks created this way flow through the same task store as web, CalDAV, and MCP tasks.

## Limitations

Boox is receive-only. UltraBridge cannot push deletes or renames back to the Boox device.
