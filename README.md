<p align="center">
  <img src="https://github.com/jdkruzr/ultrabridge/blob/main/docs/erbwidesmall.png" alt="minimalistic depiction of Einstein-Rosen bridge"/>
</p>

<p align="center">
 <h1>UltraBridge</h1>
</p>

UltraBridge is a self-hosted bridge for e-ink notes, tasks, search, and AI tools. It can receive notes from Supernote, Boox, ForestNote, and reMarkable devices; render and OCR handwritten pages; expose tasks through CalDAV and MCP; and search or chat across indexed notebook content.

**This software was developed using Claude Code, which was trained on open source software, and will therefore always be open-source software.**

## What It Does

- **Device sources:** Supernote via UltraBridge's built-in SPC server, Boox via WebDAV, ForestNote via `/sync/v1`, and reMarkable via a hosted protocol-compatible source — two-way, including putting files ON the tablet.
- **Tasks:** Web UI task management, CalDAV sync with RFC 6578 incremental change detection and all four VTODO statuses, MCP task tools, ForestNote task provenance, and signed task attachments.
- **Notes:** Per-source Files tabs, rendered page previews, OCR/reprocess controls, trash/recovery flows, digest/detail views, and reMarkable file management (upload PDF/EPUB to the tablet, download originals, delete to its trash, manage folders).
- **Search and AI:** FTS5 keyword search, optional Ollama embeddings, RAG retrieval, local chat through an OpenAI-compatible endpoint, and MCP note tools.
- **Operations:** SQLite-backed storage, Docker-first deployment, grouped Settings pages, a persistent pipeline status bar on every page, operator-assigned device names, live logs, bearer tokens, and source-specific sync/device management.

## Quick Start

### Installer

```bash
./install.sh
```

The installer builds UltraBridge, writes a Docker Compose configuration, starts the service, and seeds your first username/password. Open the displayed URL, usually `http://localhost:8443`, then finish setup in the web UI.

### Docker Compose

```bash
docker compose up -d --build
```

The checked-in compose file publishes the main app on `8443` and the Supernote SPC listener on `8089`. MCP is served by the main app at `/mcp`.

### Local Development

```bash
go build -o /tmp/ultrabridge ./cmd/ultrabridge/

UB_DB_PATH=/tmp/ub-notes.db \
UB_TASK_DB_PATH=/tmp/ub-tasks.db \
UB_LISTEN_ADDR=:8443 \
/tmp/ultrabridge
```

On a fresh database, UltraBridge opens the setup page without auth. After the first user is created, web, API, CalDAV, and MCP access require credentials or a bearer token.

## User Documentation

- [Installation and upgrade](docs/user/installation.md)
- [Sources and sync models](docs/user/sources.md)
- [Supernote setup](docs/user/supernote.md)
- [Boox setup](docs/user/boox.md)
- [ForestNote sync](docs/user/forestnote.md)
- [reMarkable setup](docs/user/remarkable.md)
- [Tasks, CalDAV, and attachments](docs/user/tasks-caldav.md)
- [Search, RAG, OCR, and chat](docs/user/search-rag-chat.md)
- [MCP and JSON API](docs/user/mcp-api.md)
- [Operations and troubleshooting](docs/user/operations.md)

Additional references:

- [Architecture](docs/ARCHITECTURE.md)
- [API reference](docs/api-spec.md)
- [Troubleshooting](docs/TROUBLESHOOTING.md)
- [Standalone Supernote OCR injection](docs/OCR_INJECTION.md)

## Main Endpoints

| Surface | Default URL | Notes |
| --- | --- | --- |
| Web UI | `http://<host>:8443/` | Tasks, Search, per-source Files tabs, Digests (Supernote), Chat, Logs, and the four grouped Settings pages. |
| Health check | `http://<host>:8443/health` | Returns service status and config-dirty state. |
| CalDAV | `http://<host>:8443/.well-known/caldav` | Task collection for DAVx5, Evolution, 2Do, and similar clients. |
| Boox WebDAV | `http://<host>:8443/webdav/` | Boox upload target. |
| MCP | `https://<public-host>/mcp` | Streamable HTTP MCP endpoint built into the main service (Claude.ai and other MCP clients). |
| JSON API | `http://<host>:8443/api/v1/*` | Tasks, files, search, chat, status, config, sync devices, and reMarkable device + file-management routes. |
| Supernote SPC | `https://<supernote-host>/` -> `:8089` | Device-facing listener. Use a dedicated hostname behind your reverse proxy. |
| ForestNote sync | `http://<host>:8443/sync/v1` | Device sync endpoint, enabled by a ForestNote source. |

## Configuration

Most configuration lives in the SQLite settings database and is edited from the web UI:

- **Settings -> Devices:** source rows, Supernote SPC server settings, ForestNote device registry, Boox maintenance, and reMarkable source/device data.
- **Settings -> AI & Processing:** OCR provider/model, source OCR prompts, embeddings, and chat.
- **Settings -> Integrations:** CalDAV and MCP tokens.
- **Settings -> System:** web credentials and verbose API logging. (Log level, format, and file destination remain bootstrap `UB_LOG_*` env vars.)

Bootstrap environment variables are still supported for automation and overrides. The most common are:

| Variable | Default | Purpose |
| --- | --- | --- |
| `UB_DB_PATH` | `/data/ultrabridge.db` | Notes, settings, source state, and search data. |
| `UB_TASK_DB_PATH` | `/data/ultrabridge-tasks.db` | CalDAV/task database. |
| `UB_LISTEN_ADDR` | `:8443` | Main app listener. |
| `UB_SPC_MODE` | `client` | Whether UltraBridge runs the built-in Supernote SPC listener. `client` (the default) means the listener is off — UltraBridge no longer has an SPC *client*; the name is historical. Set `server`, or tick **Settings -> Devices -> UB-as-SPC Device Sync Server -> Enable device sync server**, then restart. |
| `UB_SPC_LISTEN_ADDR` | `:8089` | Supernote SPC listener address. |
| `UB_SYNC_ENABLED` | `false` | Legacy gate. `/sync/v1` is now served whenever an enabled ForestNote source exists; a legacy `true` value is promoted into a ForestNote source row once, at first boot after upgrade. |

Secrets such as API keys, MCP bearer tokens, task attachment signing keys, and SPC credentials should be set through Settings or an untracked `.env`.

## Build And Test

```bash
go build ./cmd/ultrabridge/
go test ./...
```

For Docker:

```bash
./rebuild.sh
```

`rebuild.sh` force-recreates the container and waits up to 180 seconds for `/health`; `--fresh` clears both SQLite databases and `--nuke` deletes all data (both confirmed unless `-y`). Or:

```bash
docker compose up -d --build
```

## Known Limitations

- UltraBridge's own UI and API model title, description, URL, priority, categories, status (all four VTODO states), due date, completion time, and signed attachments. Properties it does not model — recurrence rules, alarms, parent/child links — are preserved verbatim through the stored iCal blob and round-trip through CalDAV, but UltraBridge does not interpret or expand them.
- Boox is receive-only. Deletes and renames on the device do not propagate back to UltraBridge.
- reMarkable device handwriting recognition proxying and UltraBridge server-side OCR/search are separate paths. The native device HWR proxy returns MyScript JIIX to the tablet; search/RAG text comes from UltraBridge's render-to-OCR pipeline.
- Some legacy design and test documents describe earlier MariaDB-backed SPC integration. User-facing setup should follow the docs linked from this README.

## Release Notes

The current release train is documented in [CHANGELOG.md](CHANGELOG.md). This documentation set is prepared for `v1.6.0`.

## License

[Apache 2.0](LICENSE) (C) 2026 jdkruzr. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

## Credits

This project owes a bunch to two self-hosted Supernote Private Cloud reimplementation projects: [Supernote Knowledge Hub](https://github.com/allenporter/supernote) and [OpenNoteCloud](https://github.com/k4z4n0v4/opennotecloud), both of which helped shape how UltraBridge evolved.
