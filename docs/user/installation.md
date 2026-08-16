# Installation And Upgrade

UltraBridge is Docker-first, but the main binary also runs directly for development.

## Prerequisites

- Go, only for local development or source builds.
- Docker with Compose v2 for the normal deployment path.
- Writable storage for `/data` and any source-specific file roots.
- A reverse proxy with TLS for devices or cloud clients that require public HTTPS.
- Optional services: Ollama for embeddings and an OpenAI-compatible chat/OCR endpoint for AI features.

UltraBridge does not require an external Supernote Private Cloud stack or MariaDB. The built-in SPC server is enabled from Settings when you want a Supernote device to sync directly to UltraBridge.

## Installer

```bash
./install.sh
```

The installer prompts for a username, password, and the two host ports to publish (defaults `8443` and `8089`), builds the image, **overwrites `docker-compose.yml`** with the generated configuration, starts the container, waits for `/health`, and seeds the first web user. It bind-mounts `/mnt/supernote` and `/mnt/remarkable` into the container only if those directories already exist on the host — create them before running the installer if you plan to use those sources.

Note: the checked-in `docker-compose.yml` also enables file logging (`UB_LOG_FILE=/data/ultrabridge.log`) and joins an external `supernote_default` network. `install.sh` regenerates the file without those, so file logging is off on installer-created deployments — add `UB_LOG_FILE` back if you want it.

## Docker Compose

```bash
docker compose up -d --build
```

The default compose file publishes:

| Port | Service |
| --- | --- |
| `8443` | Web UI, JSON API, CalDAV, Boox WebDAV, MCP, ForestNote sync, and reMarkable routes. |
| `8089` | Supernote SPC device listener. Only bound after you tick **Settings -> Devices -> UB-as-SPC Device Sync Server -> Enable device sync server** and restart. |

## Local Development

```bash
go build -o /tmp/ultrabridge ./cmd/ultrabridge/

UB_DB_PATH=/tmp/ub-notes.db \
UB_TASK_DB_PATH=/tmp/ub-tasks.db \
UB_LISTEN_ADDR=:8443 \
/tmp/ultrabridge
```

## First Boot

On an empty settings database, `/setup` is public. Create the first user, then configure sources and integrations from Settings.

## Upgrades

1. Back up `/data` and source roots.
2. Pull the new code.
3. Rebuild:

   ```bash
   ./rebuild.sh
   ```

   `rebuild.sh` rebuilds the image, force-recreates the container, and waits up to 180 seconds for `/health`. `--fresh` additionally deletes both SQLite databases and `--nuke` deletes the whole data directory; both prompt for confirmation unless you pass `-y`.

4. Open Settings and resolve any restart banner or newly available source settings.
5. Check the Logs tab for migration or source startup errors.

Upgrading from v1.2.x: pipeline status is now a single persistent bar at the top of every page rather than a per-tab panel, and the SPC "Mode" selector is now an "Enable device sync server" checkbox.

Legacy `UB_*` environment variables are still honored as bootstrap overrides, but most installs should prefer DB-backed Settings plus untracked `.env` secrets.

## Reverse Proxy Basics

Use separate hostnames for surfaces that have different protocol expectations:

| Hostname | Backend | Use |
| --- | --- | --- |
| `ub.example.com` | `:8443` | Web UI, CalDAV, Boox WebDAV, MCP, API, ForestNote, reMarkable. |
| `supernote.example.com` | `:8089` | Supernote SPC device and Partner App traffic. |

For the Supernote hostname, preserve the `Host` header and WebSocket upgrade for `/socket.io/`.
