# Supernote Setup

UltraBridge can be the cloud endpoint your Supernote syncs against. It implements the Supernote Private Cloud protocol directly and stores state in SQLite and configured file roots.

## Enable The SPC Server

1. Add or enable a Supernote source in **Settings -> Devices**.
2. In **UB-as-SPC Device Sync Server**, set mode to `server`.
3. Set the SPC file root to the full Supernote storage root. It should contain folders such as `Note`, `Document`, `EXPORT`, `SCREENSHOT`, `INBOX`, and `MyStyle`.
4. Configure the device account/password fields used by the Supernote login flow.
5. Restart UltraBridge if Settings shows a restart-required banner.

The default SPC listener is `:8089`. The main app remains on `:8443`.

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

UltraBridge's task database wins conflicts. Task changes from CalDAV, MCP, web UI, and the device converge through the same SQLite-backed task store.

## Common Checks

- `curl http://localhost:8443/health` for the main app.
- Check the Logs tab for SPC listener startup.
- Confirm port `8089` is published in Docker.
- Confirm the device hostname reaches `:8089`, not `:8443`.
