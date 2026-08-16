# Supernote Setup

UltraBridge can be the cloud endpoint your Supernote syncs against. It implements the Supernote Private Cloud protocol directly and stores state in SQLite and configured file roots.

## Enable The SPC Server

1. Add or enable a Supernote source in **Settings -> Devices**.
2. Under **UB-as-SPC Device Sync Server**, tick **Enable device sync server**.
3. Set the SPC file root to a **dedicated, UltraBridge-owned** directory (e.g. `/mnt/supernote/ub_sn_files`). Do not point it at an existing Supernote Private Cloud installation's data directory. UltraBridge creates the device's bucket layout (`NOTE/Note`, `DOCUMENT/Document`, `EXPORT`, ...) inside it as the device syncs.
4. Point your Supernote **source's** notes path at `<file root>/NOTE/Note` so uploaded notes are OCR-indexed.
5. Configure the device account/password fields used by the Supernote login flow. Optional fields in the same panel: reported capacity/quota, TLS cert/key (leave empty when TLS terminates at your proxy), JWT secret, and the OSS signing secret (auto-generated on first boot).
6. Restart UltraBridge — every field in this panel takes effect only at startup.

The default SPC listener is `:8089`. The main app remains on `:8443`. The JIIX text-injection toggle lives with the Supernote source in **Settings -> Devices**; the Supernote OCR prompt editor lives in **Settings -> AI & Processing**.

## Reverse Proxy

Use a dedicated hostname for the Supernote device:

| Hostname | Backend |
| --- | --- |
| `supernote.example.com` | `http://ultrabridge:8089` |
| `ub.example.com` | `http://ultrabridge:8443` |

The Supernote hostname must:

- Preserve the `Host` header.
- Forward WebSocket upgrades for `/socket.io/`.
- Terminate TLS at the proxy unless you intentionally configure TLS inside UltraBridge.

## What Syncs

- Device files and folders.
- Task changes through the SPC schedule/task endpoints.
- Digest and metadata surfaces needed by the Files UI.

UltraBridge's task database is authoritative, but a device write is merged into the stored task rather than replacing it — fields the Supernote doesn't understand (recurrence, alarms, hierarchy, ForestNote provenance) survive a device edit. The Supernote itself only has two task states, so `IN-PROCESS` shows as open and `CANCELLED` shows as completed on-device while the real state is preserved server-side. Task changes from CalDAV, MCP, web UI, and the device converge through the same SQLite-backed task store.

## Pipeline Controls

The Supernote OCR worker's start/stop controls live in the persistent pipeline status bar at the top of every page (they were previously a per-tab panel). Device management for Supernote is currently registry-less; ForestNote and reMarkable have per-device registries, and a Supernote equivalent is planned.

## Common Checks

- `curl http://localhost:8443/health` for the main app.
- Check the Logs tab for SPC listener startup.
- Confirm port `8089` is published in Docker.
- Confirm the device hostname reaches `:8089`, not `:8443`.
