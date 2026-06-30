# MCP And JSON API

UltraBridge exposes both MCP tools and a JSON API.

## Built-In MCP SSE

The main UltraBridge process serves MCP at:

```text
https://your-public-host/mcp
```

Use this for Claude.ai web or other MCP clients that support HTTP/SSE. Claude.ai must be able to reach the URL publicly.

Create tokens in **Settings -> Integrations -> MCP Tokens**.

## Note Tools

- `search_notes`
- `get_note_pages`
- `get_note_image`

These tools read indexed note text and rendered page images through UltraBridge's API.
`search_notes` accepts `query` plus optional `source` or `sources`
(`supernote`, `boox`, `forestnote`, `remarkable`, `digest`), `folder`,
`location`, `device_model`, `created_from`, `created_to`, `modified_from`,
`modified_to`, `sort`, `mode`, and `limit`. Deprecated aliases `device` and
`date_from`/`date_to` are still accepted for older clients. Note tools return
structured MCP results with a concise text fallback.

## ForestNote Text Box Tools

- `list_text_boxes`
- `edit_text_box`

These tools discover and edit synced ForestNote text boxes. Server-authored edits relay back to devices on their next sync.

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
They also return structured MCP results with the same task fields exposed by
the JSON API.

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
