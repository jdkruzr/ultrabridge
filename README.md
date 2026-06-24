<p align="center">
  <img src="https://github.com/jdkruzr/ultrabridge/blob/main/docs/erbwidesmall.png" alt="minimalistic depiction of Einstein-Rosen bridge"/>
</p>

<p align="center">
 <h1>UltraBridge</h1>
</p>

UltraBridge is a note management and task synchronization platform supporting multiple e-ink devices, including Supernote (via [Supernote Private Cloud](https://support.supernote.com/article/75/set-up-supernote-partner-cloud)) and Onyx Boox. It provides:

1. **CalDAV task sync** — synchronise Supernote tasks with any CalDAV client (DAVx5, GNOME Evolution, 2Do, etc.)
2. **Supernote notes pipeline** — automatically discover `.note` files, extract handwritten text, index it for full-text search, and optionally run vision-API OCR
3. **Boox notes pipeline** — accept Boox `.note` file uploads via WebDAV or bulk import from filesystem (including PDFs), parse ZIP/protobuf format, render pages, OCR, extract TODOs via color coding, and index for unified search alongside Supernote notes
4. **RAG search** — generate vector embeddings via Ollama, then combine them with FTS5 keyword search using reciprocal rank fusion for hybrid retrieval. Exposed as a JSON API and an integrated MCP server with full OAuth2 support for Claude.ai
5. **Local chat** — ask questions about your notes in a browser-based chat tab, powered by a local text generation model (vLLM) with streaming responses and clickable citations back to source pages
6. **Unified search** — full-text search across both Supernote and Boox notes with source indicators and flexible sorting (name, size, date)

**This software was developed using Claude Code, which was trained on open source software, and will therefore always be open-source software.**

## Prerequisites

- **Docker** and **Docker Compose v2**
- For Supernote features: **Supernote Private Cloud** running with Docker Compose
- For Boox integration: a Boox device with WebDAV export support (Tab Ultra C Pro, NoteAir, Note Air5C, etc.)
- For CalDAV sync: a CalDAV client on your device
- For OCR: an API key for Anthropic or OpenRouter, or an API endpoint from a local inference API server like vLLM
- For RAG search *(optional)*: **Ollama** with the `nomic-embed-text:v1.5` model
- For local chat *(optional)*: **vLLM** (or any OpenAI-compatible API) with a text generation model

> **Supernote Users:** The installer auto-detects your Supernote Private Cloud network and volume mounts to ensure seamless task synchronization and note discovery.

## Quick Start

### Option A: Use the installer (recommended)

```bash
./install.sh
```

The installer prompts for a username, a password, and three host ports: the web UI (default 8443), the MCP server (default 8081), and the Supernote SPC device-sync server (default 8089). It builds the images, generates a `docker-compose.yml` that publishes all three, starts the containers, and seeds your credentials.

Open the URL shown (e.g., `http://localhost:8443`), log in, and configure everything else via the **Settings** tab: device sources, OCR, RAG search, chat.

The SPC port (8089) is published but idle until you set **Settings > UB-as-SPC Device Sync Server > Mode** to `server` and restart. To sync a Supernote you'll also need a reverse proxy with a dedicated hostname for that port — see [Reverse Proxy & Device Hostnames](#reverse-proxy--device-hostnames).

To rebuild after pulling changes:

```bash
./rebuild.sh
```

### Option B: Docker Compose (manual)

```bash
docker build -t ultrabridge:latest .

docker run -d --name ultrabridge \
  -p 8443:8443 \
  -e UB_DB_PATH=/data/ultrabridge.db \
  -e UB_LISTEN_ADDR=:8443 \
  -e UB_TASK_DB_PATH=/data/ultrabridge-tasks.db \
  -v ./ultrabridge-data:/data \
  ultrabridge:latest
```

Open `http://localhost:8443` — the **setup page** appears (no auth required on first boot). Enter a username and password, then configure sources and services via **Settings**.

Verify with: `curl http://localhost:8443/health` → `{"status":"ok","config_dirty":false}`

### Option C: Local development (no Docker)

```bash
go build -o /tmp/ultrabridge ./cmd/ultrabridge/

UB_DB_PATH=/tmp/ub-test.db \
UB_TASK_DB_PATH=/tmp/ub-tasks.db \
UB_LISTEN_ADDR=:8443 \
/tmp/ultrabridge
```

Opens on `:8443` with the setup page. Configure from there.

### Upgrading from env-var-based installs

Existing deployments using `UB_USERNAME`, `UB_PASSWORD_HASH`, and other `UB_*` env vars continue to work — `appconfig.Load()` falls back to env vars when the database has no stored values. The one change: `UB_NOTES_PATH` and `UB_BOOX_NOTES_PATH` no longer auto-create source entries. After upgrading, add your sources via **Settings > Sources** or the `/api/sources` API.

## Web UI

Navigate to `http://<host>:<port>/` after starting the service.

| Tab | What it does |
|-----|-------------|
| **Tasks** | View, create, and complete tasks; bulk actions; purge all completed tasks |
| **Supernote Files** | Directory-tree browser of Supernote `.note` files; sort by name, size, or date; view rendered pages, OCR text, and processing status; queue/skip/force per row; Scan Now. |
| **Boox Files** | Flat catalog of Boox notes with Title, Folder, Device, NoteType, and Pages columns; bulk delete; Import and Migrate Imports for bulk filesystem-to-catalog ingest; per-row queue/skip/force/details. |
| **Search** | Full-text keyword search across all indexed notes with source badges and folder filter |
| **Chat** | Ask questions about your notes with streaming AI responses and clickable `[filename, p.N]` citations |
| **Logs** | Live log stream with level filtering and remote IP tracking |
| **Settings** | Configuration for OCR prompts, red-ink to-dos, RAG embedding, local chat, MCP tokens, and verbose API logging |

### Red Ink To-Do Extraction

When enabled in Settings > Boox, a second OCR pass scans each Boox page for **red handwriting**. Any red text found is automatically created as a CalDAV task — visible in the Tasks tab, synced to your CalDAV client, and pushed to your Supernote device.

This lets you use red ink on your Boox device as a "to-do" marker: write in red, and UltraBridge picks it up. Duplicate detection prevents the same task from being created twice (checks both incomplete and completed tasks).

This feature is exposed in the Settings tab of the application and can be configured as needed (for example, changing the prompt from "red" to "blue.")

## CalDAV Client Setup

**(Note for all URLs: 8443 is the internally exposed port; you are expected to run a reverse proxy for TLS termination like Caddy or Nginx Proxy Manager.)**

UltraBridge exposes a single CalDAV collection at `https://your-host:8443/caldav/tasks/`.

### DAVx5 (Android)
1. **Add account** → DAVx5 settings → Add account → CalDAV
2. **Server URL:** `https://your-host:8443/.well-known/caldav`
3. **Login:** Your UltraBridge username and password
4. **Accept SSL:** If using a self-signed certificate, enable "SSL / TLS: Custom CA"
5. **Sync:** Select "Supernote Tasks" collection

### GNOME Evolution (Linux)
1. **Calendar** → **New Calendar** → Remote
2. **Type:** CalDAV, **URL:** `https://your-host:8443/caldav/tasks/`
3. **Login:** Your UltraBridge username and password

### OpenTasks / 2Do
Use `https://your-host:8443/.well-known/caldav` as the server URL with your credentials.

## Configuration

**All configuration beyond bootstrap happens via the web UI Settings tab.** The installer and first-boot setup walk you through enabling sources, configuring paths, and setting up OCR and RAG.

### Bootstrap Environment Variables (Optional)

Only these variables are read at startup. All other configuration is stored in the database and configured via the Settings UI.

| Variable | Default | Description |
|----------|---------|-------------|
| `UB_DB_PATH` | `/data/ultrabridge.db` | SQLite database for notes, tasks, and settings |
| `UB_TASK_DB_PATH` | `/data/ultrabridge-tasks.db` | SQLite database for CalDAV task sync |
| `UB_LISTEN_ADDR` | `:8443` | Listen address (port inside container) |

### Web UI Settings

After first boot, use the **Settings** tab to configure:

- **Auth:** Username and password for CalDAV and web access
- **Sources:** Add Supernote and/or Boox note sources with their respective paths
- **OCR:** Enable vision-API OCR, set API credentials and model
- **Embeddings:** Configure Ollama for RAG search (optional)
- **Chat:** Enable local chat with vLLM (optional)
- **MCP Tokens:** Generate bearer tokens for standalone MCP clients
- **Logging:** Set log level, format, file path, and toggle Verbose API Logging
- **CalDAV:** Collection name for task sync, due date handling mode

### Device Sources

Configure sources in Settings → Sources:

- **Supernote:** Path to `.note` files and optional backup directory
- **Boox:** Path for WebDAV note uploads; optionally enable red-ink to-do extraction

Each source can be enabled/disabled independently.

On the Boox device, configure WebDAV sync under Settings > Cloud Storage with `http://<host>:<port>/webdav/` as the server URL and your UltraBridge credentials. Uploaded notes appear in the **Boox Files** tab.

Re-uploaded notes are versioned automatically — the previous version is archived under `.versions/` before the new file is written.

### RAG search (embedding pipeline)

When enabled, UltraBridge generates vector embeddings for each OCR'd page using Ollama. These embeddings power hybrid search — combining FTS5 keyword matching with vector cosine similarity via reciprocal rank fusion (RRF). The result is search that finds both exact keyword matches and semantically related content.

Embeddings are stored in SQLite and loaded into an in-memory cache on startup. A background backfill runs at startup to embed any pages that were indexed before the feature was enabled. You can also trigger a backfill manually from the Settings page.

**Setup:** Install [Ollama](https://ollama.com), pull the model, and enable the feature via Settings:

```bash
ollama pull nomic-embed-text:v1.5
# Then enable embeddings in Settings > Embedding
```

If Ollama is unreachable, embedding silently skips — OCR indexing continues normally. You won't lose data, just vector search capability until Ollama comes back.

### JSON API

UltraBridge exposes a headless JSON API under `/api/v1/*` covering tasks,
files, search, chat, and system status. All routes require Basic Auth or a
bearer token (see **MCP Tokens** in Settings).

Highlights:

- `GET /api/v1/tasks` — list active tasks; optional `status`, `due_before`,
  `due_after` filters.
- `GET /api/v1/tasks/{id}` / `PATCH /api/v1/tasks/{id}` — fetch or
  partial-update a single task.
- `POST /api/v1/tasks` / `POST /api/v1/tasks/{id}/complete` /
  `DELETE /api/v1/tasks/{id}` / `POST /api/v1/tasks/purge-completed` —
  standard CRUD + housekeeping.
- `GET /api/v1/files`, `GET /api/v1/search`, `POST /api/v1/chat/ask`,
  `GET /api/v1/status`.

A lighter-weight compatibility surface also exists for legacy integrations:

- `GET /api/search`, `GET /api/notes/pages`, `GET /api/notes/pages/image` —
  used by the built-in MCP note tools.

Full endpoint reference, request/response shapes, and query-parameter rules
live in [`docs/api-spec.md`](docs/api-spec.md).

All task mutations (both via the API and via MCP tools) emit a structured
audit log line tagged with the auth method and the token label / username
that made the change, so "why did that task disappear" is answerable from
`docker logs`.

### MCP Server (Claude integration)

UltraBridge includes a built-in MCP server that exposes your notes as tools for AI agents.

#### Option 1: Integrated SSE Server (Claude.ai Web)

The main UltraBridge binary hosts an MCP-compliant SSE endpoint at `/mcp`. This is the easiest way to connect **Claude.ai** on the web.

1. In Claude.ai, go to **Settings > MCP**.
2. Click **Add Server** and select **SSE**.
3. **Name:** UltraBridge
4. **Endpoint URL:** `https://your-public-url/mcp`
5. **Authorization:** Select **OAuth** (UltraBridge supports a simplified OAuth2 flow for Claude).
6. Click **Connect**. You will be redirected to UltraBridge to authorize the connection.

Once connected, Claude can search your notes, read page text, and view rendered images.

#### Option 2: Standalone Binary (Claude Desktop / CLI)

For local use with Claude Desktop or command-line agents, use the `ub-mcp` standalone binary. It communicates with the main UltraBridge API via JSON.

**Tools:**

Note-oriented (read-only):

| Tool | Description |
|------|-------------|
| `search_notes` | Search notes by keyword with optional folder/device/date filters |
| `get_note_pages` | Get all page text for a specific note |
| `get_note_image` | Get a rendered page image (JPEG) |

Task-oriented (mutates the CalDAV-synced task list):

| Tool | Description |
|------|-------------|
| `list_tasks` | List active tasks, optionally filtered by status and/or due-date range |
| `get_task` | Fetch a single task by id, including any back-reference to the note it was auto-extracted from |
| `create_task` | Create a new task (title + optional RFC3339 due date) |
| `update_task` | Partial update — change the title, due date, or detail. `clear_due_at=true` removes an existing due date. |
| `complete_task` | Mark a task as completed |
| `delete_task` | Soft-delete a task |
| `purge_completed_tasks` | Drop every completed task in one call |

Task mutations flow through UltraBridge's normal sync path, so changes
made by the agent propagate to your configured CalDAV device on the next
sync cycle (UB wins on conflicts). Every mutation is audit-logged with
the token label, so you can see which agent did what.

**Build and configure Claude Desktop** (`claude_desktop_config.json`):

```bash
go build ./cmd/ub-mcp/
```

```json
{
  "mcpServers": {
    "ultrabridge": {
      "command": "/path/to/ub-mcp",
      "env": {
        "UB_MCP_API_URL": "http://localhost:8443",
        "UB_MCP_API_USER": "your-username",
        "UB_MCP_API_PASS": "your-password"
      }
    }
  }
}
```

### Local chat

When enabled, a Chat tab appears in the web UI. Type a question, and UltraBridge retrieves relevant note pages via hybrid search, assembles a prompt with the retrieved context, and streams the response from a local text generation model via vLLM's OpenAI-compatible API.

The model is instructed to cite sources using `[filename, p.N]` format. These citations render as clickable links back to the source note page. Chat history is persisted in SQLite — conversations survive page refreshes and restarts.

**Setup:** Run vLLM (or any OpenAI-compatible API) with your model of choice:

```bash
vllm serve Qwen/Qwen3-8B
# Then enable chat in Settings > Chat
```

If vLLM is unreachable when you send a message, the chat UI shows an error instead of crashing. Previous conversations remain accessible.

### Supernote Sync

UltraBridge *is* the cloud your Supernote syncs against. It implements the Supernote Private Cloud (SPC) protocol directly (`internal/spcserver`), so the device talks to UltraBridge as if it were Ratta's server — there's no `supernote-service` and no MariaDB. Tasks, files, and digests all sync over SPC, and UltraBridge's local SQLite task store wins on conflicts.

Enable it in **Settings > UB-as-SPC Device Sync Server** by setting **Mode** to `server`. The default mode, `client`, runs no listener. The server binds its own port — `:8089` by default — separate from the main app on `:8443`.

#### Reverse Proxy & Device Hostnames

The SPC server and the main app are **two listeners on different ports**: `:8089` for the Supernote, `:8443` for the web UI, CalDAV, Boox WebDAV, and MCP. A Supernote connects over plain HTTPS (port 443) addressed by hostname, so you put a reverse proxy in front — and you need **two hostnames**, one per backend port:

| Hostname (example) | Routes to | Serves |
|--------------------|-----------|--------|
| `supernote.example.com` | `:8089` | Supernote device/Partner App sync (SPC) |
| `ub.example.com`   | `:8443`   | Web UI, CalDAV, Boox/WebDAV, MCP |

Pick any names you like, but they must be **distinct**. A proxy routes by `Host` header to a single backend, so one hostname can't reach both ports. Point the Supernote at the `:8089` hostname; point browsers, CalDAV clients, and Boox devices at the `:8443` one.

In practice, keep using a Supernote-named domain for SPC traffic, such as `supernote.example.com` (or this install's `supernote.broken.works`). Do not multiplex SPC paths under the UltraBridge web hostname (`ub.example.com` / `ultrabridge.broken.works`); the web app has its own `/api/*` surface, and the Supernote device and Partner App expect the bog-standard SPC contract on their host.

Two requirements for the `:8089` proxy host:

- **Forward the WebSocket upgrade** for `/socket.io/`. The device holds a persistent Engine.IO socket and reconnect-loops without it.
- **Preserve the Host header** (`proxy_set_header Host $host`), so UltraBridge mints file download/upload URLs that resolve back through the proxy.

TLS is normally terminated at the proxy. Leave the SPC card's TLS fields blank in that case.

When running the SPC server in Docker, publish the extra port: add `-p 8089:8089` to the `docker run` in Option B (or the `ports:` list in your compose file).

## Architecture

UltraBridge is organised as four services (Task, Note, Search, Config)
sitting on top of a SQLite store, plus a pair of source-specific notes
pipelines (Supernote and Boox). Everything is SQLite-backed — there's
no MariaDB. Supernote devices sync against UltraBridge's built-in SPC
server (`internal/spcserver`, on its own `:8089` listener); Boox
devices push files over WebDAV on the main `:8443` listener. Both
pipelines feed the same search and RAG index.

For the full system diagram, the two pipeline flow diagrams
(Supernote and Boox), the task-mutation flow, and the service-layer
contracts, see [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

Per-package deep dives live in each `internal/*/CLAUDE.md`.

## Development

### Build

From the repo root:

```bash
go build ./cmd/ultrabridge/
go build ./cmd/ub-mcp/
```

### Test

```bash
go test ./...
```

Integration tests require a running MariaDB:

```bash
TEST_DBENV_PATH=/mnt/supernote/.dbenv go test -tags integration ./tests/ -v
```

### Rebuild Docker image

```bash
./rebuild.sh
```

Or manually:

```bash
docker build -t ultrabridge:dev .
```

### Admin subcommands

The `ultrabridge` binary ships two admin helpers for headless / automated setup:

```bash
# Generate a bcrypt hash for UB_PASSWORD_HASH (legacy env-var flow):
docker run --rm ultrabridge:dev hash-password "yourpassword"

# Pre-provision credentials directly into the settings DB so the container
# skips the web setup wizard on first boot:
docker run --rm -v ./ultrabridge-data:/data ultrabridge:dev \
  seed-user myusername "mypassword"
```

## Standalone OCR injection

For one-off OCR processing outside UltraBridge's pipeline (debugging
a specific file, backup processing, etc.), the
[go-sn](https://github.com/jdkruzr/go-sn) library ships a standalone
`sninject` tool. See [`docs/OCR_INJECTION.md`](docs/OCR_INJECTION.md)
for usage and the `-zero-recognfile` explanation.

## Known Limitations

1. **CalDAV — tier 3 fields dropped:** Only title, description, priority, due date, and status are synchronised. Recurrence, reminders, and other advanced fields are not mapped.

2. **TITLE block extraction is a stub:** `extractNoteTitle` returns an empty string. Heading text is captured indirectly when RECOGNTEXT is indexed. Full heading extraction is out of scope for the current release.

## Troubleshooting

Most common issues — can't log in, Claude.ai OAuth failures, OCR
jobs stuck, Boox WebDAV not syncing, RAG search falling back to
FTS-only — are covered in
[`docs/TROUBLESHOOTING.md`](docs/TROUBLESHOOTING.md).

If you hit something that isn't in that document: `docker logs
ultrabridge` almost always has a specific reason, and enabling
**Verbose API Logging** in Settings > General surfaces per-request
auth-failure detail.

## License

[Apache 2.0](LICENSE) © 2026 jdkruzr. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

## Credits
This project owes a bunch to two self-hosted Supernote Private Cloud reimplementation projects: [Supernote Knowledge Hub](https://github.com/allenporter/supernote) and [OpenNoteCloud](https://github.com/k4z4n0v4/opennotecloud), both of which helped shape how Ultrabridge evolved (and will continue to in the future).
