# MCP Server (ub-mcp)

Last verified: 2026-05-30 (search_notes deep-link host: UB_MCP_PUBLIC_URL env + displayBaseURL() helper mirrored across both MCP surfaces; task tools: ForestNote provenance + URL/Priority/Categories/Comment write surface + purge_deleted_tasks)

## Purpose
Model Context Protocol server that exposes UltraBridge note search and retrieval
as MCP tools for AI agents (Claude Desktop, Cursor, etc.).

## Contracts
- **Exposes**: Thirteen MCP tools across three categories.
  - Notes: `search_notes` (hybrid search), `get_note_pages` (page content), `get_note_image` (JPEG rendering).
  - ForestNote text boxes: `list_text_boxes` (discovery — boxes in a notebook with id/page/text), `edit_text_box` (server-authored edit of a box's text; relayed to devices on next sync, re-indexed; LWW so a newer device edit can override). Both require an active ForestNote source (404 otherwise).
  - Tasks: `list_tasks`, `get_task`, `create_task`, `update_task`, `complete_task`, `delete_task`, `purge_completed_tasks`, `purge_deleted_tasks` (new — hard-purges soft-tombstoned rows older than `older_than_days`, default 30; pair with `list_tasks { include_deleted: true }` to confirm targets first; the only operation that frees rows from the store). `list_tasks` gained provenance/metadata filters: `notebook_id`, `notebook_name`, `source`, `category` (case-sensitive equality), `priority` ("1".."9"), `include_deleted`. `get_task` / `list_tasks` outputs surface URL, Priority, Categories, ForestNote provenance (notebook id+name, page id, source, native URL), Comment, and a `deleted` flag. `create_task` / `update_task` accept `url`, `priority`, `categories`, `comment`; update additionally accepts `clear_url`, `clear_priority`, `clear_comment` sentinels (Clear wins over value when both set; mirrors existing `clear_due_at`). `categories` on update is wholesale (send `[]` to clear, omit to leave unchanged). All task mutations propagate to configured CalDAV devices on the next sync cycle (UB-wins). Dates are RFC3339.
  - Two transport modes: stdio (default) and HTTP SSE (`--http` flag).
- **Guarantees**: All tools delegate to UltraBridge JSON API via HTTP, authenticating with a Bearer token (`UB_MCP_API_TOKEN`) when set, else Basic Auth. Image data returned as base64-encoded embedded images. Error responses use MCP error format.
- **Expects**: Running UltraBridge instance with JSON API endpoints enabled (requires retriever). Environment variables for API connection.

## Two MCP surfaces — keep in sync

Task tools live in **both** `cmd/ub-mcp/tasks.go` (the standalone sidecar
binary) and `cmd/ultrabridge/mcptools.go` (the in-process MCP registered
at `cmd/ultrabridge/main.go:753`, mounted at `/mcp/`). When adding or
changing a tool, both surfaces must be updated; the input/output JSON
shapes are mirrored intentionally (the in-process file keeps local
`mcpTask` / `mcpTaskForestNote` / `mcpTaskLink` types so it doesn't
import `internal/service`). Drift between the two is a contract bug,
not a refactor opportunity.

This applies to deep-link formatting too: both `apiClient` (sidecar) and
`mcpAPIClient` (in-process) carry a `publicBaseURL` field plus a
`displayBaseURL()` helper that returns `publicBaseURL` when set and the
loopback `baseURL` otherwise. `search_notes` (and any future tool that
emits a clickable URL into the response stream) must build its
`detailURL` from `client.displayBaseURL()` — never `client.baseURL` —
or remote LLM consumers will see a loopback link they can't follow.
The in-process surface populates `publicBaseURL` from the
`KeyBooxExternalBaseURL` setting (same value the Boox red-ink-TODO
creator uses); the standalone surface populates it from
`UB_MCP_PUBLIC_URL`.

## Dependencies
- **Uses**: `github.com/modelcontextprotocol/go-sdk/mcp` (MCP server framework), UltraBridge JSON API (notes: `/api/search`, `/api/notes/pages`, `/api/notes/pages/image`; forestnote: `GET /api/forestnote/text-boxes?notebook=`, `POST /api/forestnote/text-boxes/edit`; tasks: `/api/v1/tasks`, `/api/v1/tasks/{id}`, `/api/v1/tasks/{id}/complete`, `/api/v1/tasks/purge-completed`).
- **Used by**: AI agents via MCP protocol
- **Boundary**: Separate binary. Imports `internal/mcpauth` and `internal/notedb` for direct bearer token validation against shared SQLite. All note data access still via HTTP API.

## Key Decisions
- Separate binary (not embedded in ultrabridge): allows independent deployment, different lifecycle
- HTTP API client: avoids importing internal packages, keeps MCP server loosely coupled
- Dual transport: stdio for Claude Desktop integration, HTTP SSE for network-accessible deployment

## Config
- `UB_MCP_API_URL` -- UltraBridge API base URL (default http://localhost:8443). What the binary actually talks to over HTTP.
- `UB_MCP_PUBLIC_URL` -- Externally-reachable URL of the UltraBridge deployment (e.g. `https://ub.example.com`). Used only for deep-link formatting in tool output (currently `search_notes`); empty falls back to `UB_MCP_API_URL`, which works for same-host clicks but emits a loopback URL that remote LLM consumers can't follow. Set this when the sidecar talks to a loopback or container-internal `UB_MCP_API_URL`. The in-process MCP (`cmd/ultrabridge/main.go`) reads the same value from the `boox_external_base_url` setting instead of an env var — the setting is shared with the Boox red-ink-TODO task creator.
- `UB_MCP_API_TOKEN` -- DB-backed MCP bearer token (created in UB Settings → MCP Tokens). When set, the API client sends `Authorization: Bearer <token>` and takes precedence over Basic Auth; lets the sidecar run without a plaintext password.
- `UB_MCP_API_USER` -- Basic Auth username (fallback when no token set)
- `UB_MCP_API_PASS` -- Basic Auth password (fallback when no token set)
- `UB_DB_PATH` -- Path to shared notedb SQLite file (enables DB-backed bearer tokens)

## Key Files
- `main.go` -- Entry point, transport selection, API client (GET / POST / PATCH / DELETE with JSON body support) and Bearer/Basic auth middleware.
- `tools.go` -- Note-oriented MCP tools (`search_notes`, `get_note_pages`, `get_note_image`) and top-level `registerTools`.
- `tasks.go` -- Task-oriented MCP tools (`list_tasks` / `get_task` / `create_task` / `update_task` / `complete_task` / `delete_task` / `purge_completed_tasks`) plus the local `task` / `taskLink` JSON-decode types mirroring `service.Task`.
- `tools_test.go` -- Tests for note tools against mock HTTP servers.
- `tasks_test.go` -- Tests for task tools with a shared `callTaskTool` helper (in-process MCP client-server transport).

## Gotchas
- MCP SDK uses generics for tool input types (SearchNotesInput, GetNotePagesInput, GetNoteImageInput)
- Image responses encode JPEG as base64 with embedded image content type
- Opens shared notedb for bearer token validation only — all note data access remains via HTTP API
