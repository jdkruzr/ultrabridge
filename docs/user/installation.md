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

The installer builds the image, writes Compose configuration, starts the container, and seeds the first web user. After it finishes, open the displayed web URL and finish configuration in Settings.

## Docker Compose

```bash
docker compose up -d --build
```

The default compose file publishes:

| Port | Service |
| --- | --- |
| `8443` | Web UI, JSON API, CalDAV, Boox WebDAV, MCP SSE, ForestNote sync, and reMarkable routes. |
| `8089` | Supernote SPC device listener when SPC server mode is enabled. |

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

4. Open Settings and resolve any restart banner or newly available source settings.
5. Check the Logs tab for migration or source startup errors.

Legacy `UB_*` environment variables are still honored as bootstrap overrides, but most installs should prefer DB-backed Settings plus untracked `.env` secrets.

## Reverse Proxy Basics

Use separate hostnames for surfaces that have different protocol expectations:

| Hostname | Backend | Use |
| --- | --- | --- |
| `ub.example.com` | `:8443` | Web UI, CalDAV, Boox WebDAV, MCP, API, ForestNote, reMarkable. |
| `supernote.example.com` | `:8089` | Supernote SPC device and Partner App traffic. |

For the Supernote hostname, preserve the `Host` header and WebSocket upgrade for `/socket.io/`.
