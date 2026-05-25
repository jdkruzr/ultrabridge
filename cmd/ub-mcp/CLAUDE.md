# MCP Server (ub-mcp)

Last verified: 2026-05-25 (UB_MCP_API_TOKEN bearer auth added)

## Purpose
Model Context Protocol server that exposes UltraBridge note search and retrieval
as MCP tools for AI agents (Claude Desktop, Cursor, etc.).

## Contracts
- **Exposes**: Ten MCP tools across two categories.
  - Notes: `search_notes` (hybrid search), `get_note_pages` (page content), `get_note_image` (JPEG rendering).
  - Tasks: `list_tasks`, `get_task`, `create_task`, `update_task`, `complete_task`, `delete_task`, `purge_completed_tasks`. All task mutations propagate to configured CalDAV devices on the next sync cycle (UB-wins). Dates are RFC3339; `update_task.clear_due_at=true` removes an existing due date (wins over `due_at` when both are set).
  - Two transport modes: stdio (default) and HTTP SSE (`--http` flag).
- **Guarantees**: All tools delegate to UltraBridge JSON API via HTTP, authenticating with a Bearer token (`UB_MCP_API_TOKEN`) when set, else Basic Auth. Image data returned as base64-encoded embedded images. Error responses use MCP error format.
- **Expects**: Running UltraBridge instance with JSON API endpoints enabled (requires retriever). Environment variables for API connection.

## Dependencies
- **Uses**: `github.com/modelcontextprotocol/go-sdk/mcp` (MCP server framework), UltraBridge JSON API (notes: `/api/search`, `/api/notes/pages`, `/api/notes/pages/image`; tasks: `/api/v1/tasks`, `/api/v1/tasks/{id}`, `/api/v1/tasks/{id}/complete`, `/api/v1/tasks/purge-completed`).
- **Used by**: AI agents via MCP protocol
- **Boundary**: Separate binary. Imports `internal/mcpauth` and `internal/notedb` for direct bearer token validation against shared SQLite. All note data access still via HTTP API.

## Key Decisions
- Separate binary (not embedded in ultrabridge): allows independent deployment, different lifecycle
- HTTP API client: avoids importing internal packages, keeps MCP server loosely coupled
- Dual transport: stdio for Claude Desktop integration, HTTP SSE for network-accessible deployment

## Config
- `UB_MCP_API_URL` -- UltraBridge API base URL (default http://localhost:8443)
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
