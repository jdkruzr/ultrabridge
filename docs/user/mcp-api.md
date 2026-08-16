# MCP And JSON API

UltraBridge exposes both MCP tools and a JSON API.

## Built-In MCP Endpoint

The main UltraBridge process serves MCP at:

```text
https://your-public-host/mcp
```

Use this for Claude.ai web or any MCP client that speaks Streamable HTTP. Both `/mcp` and `/mcp/` work. Claude.ai must be able to reach the URL publicly.

The endpoint accepts an `Authorization: Bearer <token>` header (MCP tokens) or the UltraBridge username/password. Create tokens in **Settings -> Integrations -> MCP Tokens**.

## Note Tools

- `search_notes`
- `get_note_pages`
- `get_note_image`

These tools read indexed note text and rendered page images through UltraBridge's API.
`search_notes` accepts `query` plus optional `source` or `sources`
(`supernote`, `boox`, `forestnote`, `remarkable`, `digest`), `folder`,
`location`, `device_model`, `created_from`, `created_to`, `modified_from`,
`modified_to`, `sort`, `mode`, and `limit`. Deprecated aliases `device` and
`date_from`/`date_to` are still accepted for older clients. `sort` is
`date_desc` (default), `date_asc`, or `relevance`; `mode` is `hybrid`
(default) or `keyword`. `location` takes the opaque folder token emitted by
the web UI's folder facet, not a plain path. `limit` defaults to 10 from the
tool and is clamped server-side to a maximum of 100. Note tools return
structured MCP results with a concise text fallback, including canonical
UltraBridge detail links and native-opener links where a source has them.

reMarkable file management (upload/download/delete/folders) is available
through the web UI and JSON API only — there are no MCP tools for it.

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

Tasks carry four native statuses — `needs_action`, `in_process`, `completed`,
`cancelled` — and all four can be filtered on and returned.

- `list_tasks` filters: `status`, `due_before`/`due_after`, `notebook_id`,
  `notebook_name`, `source`, `category`, `priority`, `include_deleted`.
- `create_task` and `update_task` accept `url`, `priority`, `categories`, and
  `comment` beyond title/due/detail. `update_task` has `clear_due_at`,
  `clear_url`, `clear_priority`, and `clear_comment` sentinels; `categories`
  is replaced wholesale (send `[]` to clear, omit to leave unchanged).
- `purge_completed_tasks` soft-deletes every completed task and returns the
  count. `purge_deleted_tasks` takes `older_than_days` (default 30, must be
  positive), returns `{deleted, skipped}`, and is the only operation that
  permanently frees rows.

Task tools include ForestNote provenance, categories, priority, URLs, details, and attachment summaries when present.
They also return structured MCP results with the same task fields exposed by
the JSON API.

## JSON API

The API lives under `/api/v1/*`. Highlights:

- `/api/v1/tasks` — CRUD, bulk actions, `purge-completed`, `purge-deleted`
- `/api/v1/files` — Supernote/Boox file listing
- `/api/v1/search`
- `/api/v1/chat/ask`
- `/api/v1/status`
- `/api/v1/config`
- `/api/v1/sync/devices` — ForestNote device registry (list, rename, prune) plus `/api/v1/sync/compact`
- `/api/v1/remarkable/devices` — reMarkable device registry (list, rename)
- `/api/v1/remarkable/documents` — list/detail, plus file management: multipart upload (`POST`), original-payload download (`GET .../{id}/file`), rename/move (`PATCH .../{id}`), delete-to-trash (`DELETE .../{id}`), and `/api/v1/remarkable/folders`
- `/api/v1/attachments/{id}` and `/api/v1/attachments/fn-render` — signed task-attachment URLs (deliberately outside auth; the signature is the guard)

See [API reference](../api-spec.md) for request and response shapes.
