# MCP And JSON API

UltraBridge exposes both MCP tools and a JSON API.

## Built-In MCP SSE

The main UltraBridge process serves MCP at:

```text
https://your-public-host/mcp
```

Use this for Claude.ai web or other MCP clients that support HTTP/SSE. Claude.ai must be able to reach the URL publicly.

## Standalone `ub-mcp`

The standalone sidecar is useful for Claude Desktop, local agents, or deployments that prefer a separate MCP process.

Build:

```bash
go build ./cmd/ub-mcp/
```

Bearer-token environment:

```bash
UB_MCP_API_URL=http://localhost:8443
UB_MCP_API_TOKEN=...
```

Create tokens in **Settings -> Integrations -> MCP Tokens**.

## Note Tools

- `search_notes`
- `get_note_pages`
- `get_note_image`

These tools read indexed note text and rendered page images through UltraBridge's API.

## Task Tools

- `list_tasks`
- `get_task`
- `create_task`
- `update_task`
- `complete_task`
- `delete_task`
- `purge_completed_tasks`
- `purge_deleted_tasks`

Task tools include ForestNote provenance, categories, priority, URLs, details, and attachment summaries when present.

## JSON API

The API lives under `/api/v1/*`. Highlights:

- `/api/v1/tasks`
- `/api/v1/files`
- `/api/v1/search`
- `/api/v1/chat/ask`
- `/api/v1/status`
- `/api/v1/config`
- `/api/v1/sync/devices`
- `/api/v1/remarkable/devices`
- `/api/v1/remarkable/documents`

See [API reference](../api-spec.md) for request and response shapes.
