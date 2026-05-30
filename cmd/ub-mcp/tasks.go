package main // FCIS: Imperative Shell

// MCP tools for task manipulation. Each tool is a thin wrapper over the
// /api/v1/tasks/* endpoints; the real business logic lives there.
//
// All mutations flow through UltraBridge's existing sync path — a change
// made here propagates to the configured CalDAV device on the next sync
// cycle (UB-wins conflict resolution).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// taskLink mirrors service.TaskLink in the UltraBridge repo. Duplicated
// here rather than imported to keep ub-mcp loosely coupled to the internal
// service package — the API contract is the JSON shape, not the Go type.
type taskLink struct {
	AppName  string `json:"app_name"`
	FilePath string `json:"file_path"`
	Page     int    `json:"page"`
}

// nativeDeepLink is the shape Supernote/Viwoods devices stuff into the
// VTODO URL property when they create a task from a page — base64-encoded
// JSON with appName/fileId/filePath/page/pageId. Decoding it lets the MCP
// formatter render a friendly "Source: <filename> (page N)" label instead
// of dumping the raw base64 wall into list_tasks output.
type nativeDeepLink struct {
	AppName  string `json:"appName"`
	FileID   string `json:"fileId"`
	FilePath string `json:"filePath"`
	Page     int    `json:"page"`
	PageID   string `json:"pageId"`

	// Filename is the derived basename of FilePath, set by decodeNativeDeepLink
	// after parsing. Lifted out so callers don't redo the path-split.
	Filename string `json:"-"`
}

// decodeNativeDeepLink tries to parse a task URL as a base64-encoded native
// deep-link blob. Returns (decoded, true) on success; (zero, false) on any
// failure path (non-base64, non-JSON, missing AppName field) — the failure
// case is the common one (plain HTTP URLs, ForestNote NativeURLs, etc.)
// and never an error worth logging. The AppName presence check guards
// against incidentally-decodable URLs that happen to be valid base64+JSON
// but aren't really native deep-links.
func decodeNativeDeepLink(raw string) (nativeDeepLink, bool) {
	// Quick gate — native deep-link payloads are JSON objects whose first
	// key starts with an ASCII letter (`appName`, `fileId`, etc.), so the
	// base64-encoded form always begins with `eyJ` (the encoding of `{"`
	// followed by such a letter). Non-deep-link URLs that happen to start
	// with `eyJ` fall through to the JSON unmarshal which rejects them
	// safely; this gate just skips the round-trip on every plain URL.
	if !strings.HasPrefix(raw, "eyJ") {
		return nativeDeepLink{}, false
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nativeDeepLink{}, false
	}
	var dl nativeDeepLink
	if err := json.Unmarshal(decoded, &dl); err != nil {
		return nativeDeepLink{}, false
	}
	if dl.AppName == "" {
		return nativeDeepLink{}, false
	}
	if dl.FilePath != "" {
		// Inline last-segment extraction (these are device-side
		// forward-slash paths; mirrors decodeMCPNativeDeepLink's split
		// for cross-surface parity rather than importing path.Base just
		// here while the other surface inlines).
		if i := strings.LastIndex(dl.FilePath, "/"); i >= 0 {
			dl.Filename = dl.FilePath[i+1:]
		} else {
			dl.Filename = dl.FilePath
		}
	}
	return dl, true
}

// taskForestNote mirrors service.TaskForestNote — provenance for tasks
// auto-extracted from a ForestNote notebook page (via the lasso → to-do
// gesture or future paths). All fields optional.
type taskForestNote struct {
	NotebookID   string `json:"notebook_id,omitempty"`
	PageID       string `json:"page_id,omitempty"`
	NotebookName string `json:"notebook_name,omitempty"`
	Source       string `json:"source,omitempty"`
	NativeURL    string `json:"native_url,omitempty"`
}

// task mirrors service.Task's JSON shape. Kept local so changes to the
// internal type don't break ub-mcp's compilation.
type task struct {
	ID          string          `json:"id"`
	Title       string          `json:"title"`
	Status      string          `json:"status"`
	CreatedAt   time.Time       `json:"created_at"`
	DueAt       *time.Time      `json:"due_at,omitempty"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
	Detail      *string         `json:"detail,omitempty"`
	Links       *taskLink       `json:"links,omitempty"`
	URL         *string         `json:"url,omitempty"`
	Priority    *string         `json:"priority,omitempty"`
	Categories  []string        `json:"categories,omitempty"`
	ForestNote  *taskForestNote `json:"forestnote,omitempty"`
	Comment     string          `json:"comment,omitempty"`
	Deleted     bool            `json:"deleted,omitempty"`
}

// formatTask renders a single task as readable text for the agent.
func formatTask(t task) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Task: %s\n", t.Title))
	sb.WriteString(fmt.Sprintf("ID: %s\n", t.ID))
	sb.WriteString(fmt.Sprintf("Status: %s\n", t.Status))
	if t.Deleted {
		sb.WriteString("(deleted — soft-tombstoned, hidden from default views)\n")
	}
	if t.DueAt != nil {
		sb.WriteString(fmt.Sprintf("Due: %s\n", t.DueAt.Format(time.RFC3339)))
	}
	if t.CompletedAt != nil && t.Status == "completed" {
		sb.WriteString(fmt.Sprintf("Completed: %s\n", t.CompletedAt.Format(time.RFC3339)))
	}
	if t.Priority != nil && *t.Priority != "" {
		sb.WriteString(fmt.Sprintf("Priority: %s\n", *t.Priority))
	}
	if t.URL != nil && *t.URL != "" {
		// Supernote/Viwoods-native tasks land here with a base64-encoded JSON
		// blob in URL — render a friendly "Source: <filename> (p.N)" label
		// alongside (or instead of) the raw blob so list_tasks output isn't
		// dominated by a wall of base64. Falls through to the bare URL line
		// when the value isn't a recognized native-deep-link payload.
		if dl, ok := decodeNativeDeepLink(*t.URL); ok {
			if dl.Filename != "" {
				if dl.Page > 0 {
					sb.WriteString(fmt.Sprintf("Source: %s (page %d)\n", dl.Filename, dl.Page))
				} else {
					sb.WriteString(fmt.Sprintf("Source: %s\n", dl.Filename))
				}
				sb.WriteString(fmt.Sprintf("URL (native deep-link, base64): %s\n", *t.URL))
			} else {
				sb.WriteString(fmt.Sprintf("URL: %s\n", *t.URL))
			}
		} else {
			sb.WriteString(fmt.Sprintf("URL: %s\n", *t.URL))
		}
	}
	if len(t.Categories) > 0 {
		sb.WriteString(fmt.Sprintf("Categories: %s\n", strings.Join(t.Categories, ", ")))
	}
	if t.Detail != nil && *t.Detail != "" {
		sb.WriteString(fmt.Sprintf("Detail: %s\n", *t.Detail))
	}
	if t.Comment != "" {
		sb.WriteString(fmt.Sprintf("Comment: %s\n", t.Comment))
	}
	if t.ForestNote != nil {
		if t.ForestNote.NotebookName != "" {
			sb.WriteString(fmt.Sprintf("From ForestNote notebook: %s (id %s)\n",
				t.ForestNote.NotebookName, t.ForestNote.NotebookID))
		} else if t.ForestNote.NotebookID != "" {
			sb.WriteString(fmt.Sprintf("From ForestNote notebook id: %s\n", t.ForestNote.NotebookID))
		}
		if t.ForestNote.PageID != "" {
			sb.WriteString(fmt.Sprintf("ForestNote page id: %s\n", t.ForestNote.PageID))
		}
		if t.ForestNote.Source != "" {
			sb.WriteString(fmt.Sprintf("ForestNote source: %s\n", t.ForestNote.Source))
		}
		if t.ForestNote.NativeURL != "" {
			sb.WriteString(fmt.Sprintf("ForestNote native URL: %s\n", t.ForestNote.NativeURL))
		}
	}
	if t.Links != nil && t.Links.FilePath != "" {
		sb.WriteString(fmt.Sprintf("From note: %s (page %d)\n", t.Links.FilePath, t.Links.Page))
	}
	return sb.String()
}

// registerTaskTools wires the task-manipulation tools onto an MCP server
// instance.
func registerTaskTools(server *mcp.Server, client *apiClient) {
	registerListTasks(server, client)
	registerGetTask(server, client)
	registerCreateTask(server, client)
	registerUpdateTask(server, client)
	registerCompleteTask(server, client)
	registerDeleteTask(server, client)
	registerPurgeCompletedTasks(server, client)
	registerPurgeDeletedTasks(server, client)
}

// --- list_tasks ---

type ListTasksInput struct {
	Status    string `json:"status,omitempty"`     // needs_action | completed | all
	DueBefore string `json:"due_before,omitempty"` // RFC3339
	DueAfter  string `json:"due_after,omitempty"`  // RFC3339
	// ForestNote provenance filters — match tasks that came from a specific
	// notebook (by ULID or human name), or any task created by a specific
	// source (e.g. "lasso").
	NotebookID   string `json:"notebook_id,omitempty"`
	NotebookName string `json:"notebook_name,omitempty"`
	Source       string `json:"source,omitempty"`
	// Category matches a single VTODO CATEGORIES entry (case-sensitive).
	Category string `json:"category,omitempty"`
	// Priority matches the VTODO PRIORITY value verbatim ("1".."9").
	Priority string `json:"priority,omitempty"`
	// IncludeDeleted surfaces soft-tombstoned rows alongside live ones.
	// Useful for "what's in the trash" queries and pre-flighting the
	// purge_deleted_tasks tool. Defaults to false.
	IncludeDeleted bool `json:"include_deleted,omitempty"`
}

func registerListTasks(server *mcp.Server, client *apiClient) {
	mcp.AddTool[ListTasksInput, any](server, &mcp.Tool{
		Name: "list_tasks",
		Description: "List tasks from UltraBridge. Optional filters: " +
			"status (needs_action / completed / all, default all); " +
			"due_before / due_after as RFC3339 (tasks with no due date excluded when either is set); " +
			"notebook_id / notebook_name / source (ForestNote provenance — match tasks created from a specific notebook or input source); " +
			"category (single VTODO CATEGORIES entry, case-sensitive); " +
			"priority (VTODO PRIORITY value 1-9); " +
			"include_deleted=true to surface soft-tombstoned rows (default false). " +
			"Returns title, status, due/completed times, URL, priority, categories, ForestNote provenance, and detail when present.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input ListTasksInput) (*mcp.CallToolResult, any, error) {
		params := url.Values{}
		if input.Status != "" {
			params.Set("status", input.Status)
		}
		if input.DueBefore != "" {
			params.Set("due_before", input.DueBefore)
		}
		if input.DueAfter != "" {
			params.Set("due_after", input.DueAfter)
		}
		if input.NotebookID != "" {
			params.Set("notebook_id", input.NotebookID)
		}
		if input.NotebookName != "" {
			params.Set("notebook_name", input.NotebookName)
		}
		if input.Source != "" {
			params.Set("source", input.Source)
		}
		if input.Category != "" {
			params.Set("category", input.Category)
		}
		if input.Priority != "" {
			params.Set("priority", input.Priority)
		}
		if input.IncludeDeleted {
			params.Set("include_deleted", "true")
		}

		path := "/api/v1/tasks"
		if encoded := params.Encode(); encoded != "" {
			path += "?" + encoded
		}
		resp, err := client.get(ctx, path)
		if err != nil {
			return nil, nil, fmt.Errorf("API request failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			return nil, nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body))
		}

		var tasks []task
		if err := json.NewDecoder(resp.Body).Decode(&tasks); err != nil {
			return nil, nil, fmt.Errorf("decode response: %w", err)
		}

		if len(tasks) == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "No tasks match the filter.\n"}},
			}, nil, nil
		}

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("%d task(s):\n\n", len(tasks)))
		for i, t := range tasks {
			sb.WriteString(fmt.Sprintf("--- %d ---\n", i+1))
			sb.WriteString(formatTask(t))
			sb.WriteString("\n")
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: sb.String()}},
		}, nil, nil
	})
}

// --- get_task ---

type GetTaskInput struct {
	ID string `json:"id"`
}

func registerGetTask(server *mcp.Server, client *apiClient) {
	mcp.AddTool[GetTaskInput, any](server, &mcp.Tool{
		Name:        "get_task",
		Description: "Fetch a single task by id. Returns the full task surface: title, status, due/completed times, URL, priority, categories, detail, comment, and any ForestNote provenance (notebook id+name, page id, source, native URL) when the task came from a notebook page.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input GetTaskInput) (*mcp.CallToolResult, any, error) {
		if input.ID == "" {
			return nil, nil, fmt.Errorf("id is required")
		}
		resp, err := client.get(ctx, "/api/v1/tasks/"+url.PathEscape(input.ID))
		if err != nil {
			return nil, nil, fmt.Errorf("API request failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode == 404 {
			return nil, nil, fmt.Errorf("task not found: %s", input.ID)
		}
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			return nil, nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body))
		}

		var t task
		if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
			return nil, nil, fmt.Errorf("decode response: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: formatTask(t)}},
		}, nil, nil
	})
}

// --- create_task ---

type CreateTaskInput struct {
	Title      string   `json:"title"`
	DueAt      string   `json:"due_at,omitempty"` // RFC3339; optional
	Detail     string   `json:"detail,omitempty"`
	URL        string   `json:"url,omitempty"`
	Priority   string   `json:"priority,omitempty"`   // VTODO PRIORITY "1".."9"
	Categories []string `json:"categories,omitempty"` // VTODO CATEGORIES (list)
	Comment    string   `json:"comment,omitempty"`    // VTODO COMMENT (free-form)
}

func registerCreateTask(server *mcp.Server, client *apiClient) {
	mcp.AddTool[CreateTaskInput, any](server, &mcp.Tool{
		Name: "create_task",
		Description: "Create a new task. Requires a title; everything else is optional. " +
			"due_at must be RFC3339 when provided. " +
			"url and priority land in dedicated columns (priority is the VTODO PRIORITY value, \"1\"-\"9\"). " +
			"categories and comment ride in the iCal blob, so they're readable via get_task right after create. " +
			"The new task syncs to configured CalDAV devices on the next sync cycle.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input CreateTaskInput) (*mcp.CallToolResult, any, error) {
		if input.Title == "" {
			return nil, nil, fmt.Errorf("title is required")
		}
		body := map[string]interface{}{"title": input.Title}
		if input.DueAt != "" {
			t, err := time.Parse(time.RFC3339, input.DueAt)
			if err != nil {
				return nil, nil, fmt.Errorf("due_at must be RFC3339: %w", err)
			}
			body["due_at"] = t
		}
		if input.Detail != "" {
			body["detail"] = input.Detail
		}
		if input.URL != "" {
			body["url"] = input.URL
		}
		if input.Priority != "" {
			body["priority"] = input.Priority
		}
		if len(input.Categories) > 0 {
			body["categories"] = input.Categories
		}
		if input.Comment != "" {
			body["comment"] = input.Comment
		}

		resp, err := client.postJSON(ctx, "/api/v1/tasks", body)
		if err != nil {
			return nil, nil, fmt.Errorf("API request failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 201 {
			raw, _ := io.ReadAll(resp.Body)
			return nil, nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(raw))
		}

		var created task
		if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
			return nil, nil, fmt.Errorf("decode response: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Created:\n" + formatTask(created)}},
		}, nil, nil
	})
}

// --- update_task ---

// UpdateTaskInput holds the partial-update payload. Omitted pointer fields
// leave the task unchanged. ClearXxx flags win over the value pointer when
// both are set (allows null-ing a column without sending a value).
type UpdateTaskInput struct {
	ID            string    `json:"id"`
	Title         *string   `json:"title,omitempty"`
	DueAt         *string   `json:"due_at,omitempty"` // RFC3339
	ClearDueAt    bool      `json:"clear_due_at,omitempty"`
	Detail        *string   `json:"detail,omitempty"`
	URL           *string   `json:"url,omitempty"`
	ClearURL      bool      `json:"clear_url,omitempty"`
	Priority      *string   `json:"priority,omitempty"`
	ClearPriority bool      `json:"clear_priority,omitempty"`
	// Categories: nil = leave unchanged; non-nil (incl. empty slice) =
	// replace wholesale. Send "categories": [] to clear.
	Categories   *[]string `json:"categories,omitempty"`
	Comment      *string   `json:"comment,omitempty"`
	ClearComment bool      `json:"clear_comment,omitempty"`
}

func registerUpdateTask(server *mcp.Server, client *apiClient) {
	mcp.AddTool[UpdateTaskInput, any](server, &mcp.Tool{
		Name: "update_task",
		Description: "Partially update a task. Only supplied fields are changed. " +
			"Use clear_due_at / clear_url / clear_priority / clear_comment to null out a column (the Clear flag wins over the value pointer when both are set). " +
			"Categories is wholesale: send a list to replace the existing set, an empty list to clear, or omit to leave unchanged. " +
			"Detail and comment can be cleared by sending an empty string. Title cannot be empty.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input UpdateTaskInput) (*mcp.CallToolResult, any, error) {
		if input.ID == "" {
			return nil, nil, fmt.Errorf("id is required")
		}

		body := map[string]interface{}{}
		if input.Title != nil {
			body["title"] = *input.Title
		}
		if input.DueAt != nil {
			parsed, err := time.Parse(time.RFC3339, *input.DueAt)
			if err != nil {
				return nil, nil, fmt.Errorf("due_at must be RFC3339: %w", err)
			}
			body["due_at"] = parsed
		}
		if input.ClearDueAt {
			body["clear_due_at"] = true
		}
		if input.Detail != nil {
			body["detail"] = *input.Detail
		}
		if input.URL != nil {
			body["url"] = *input.URL
		}
		if input.ClearURL {
			body["clear_url"] = true
		}
		if input.Priority != nil {
			body["priority"] = *input.Priority
		}
		if input.ClearPriority {
			body["clear_priority"] = true
		}
		if input.Categories != nil {
			body["categories"] = *input.Categories
		}
		if input.Comment != nil {
			body["comment"] = *input.Comment
		}
		if input.ClearComment {
			body["clear_comment"] = true
		}
		if len(body) == 0 {
			return nil, nil, fmt.Errorf("no fields to update")
		}

		resp, err := client.patchJSON(ctx, "/api/v1/tasks/"+url.PathEscape(input.ID), body)
		if err != nil {
			return nil, nil, fmt.Errorf("API request failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode == 404 {
			return nil, nil, fmt.Errorf("task not found: %s", input.ID)
		}
		if resp.StatusCode != 200 {
			raw, _ := io.ReadAll(resp.Body)
			return nil, nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(raw))
		}

		var updated task
		if err := json.NewDecoder(resp.Body).Decode(&updated); err != nil {
			return nil, nil, fmt.Errorf("decode response: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Updated:\n" + formatTask(updated)}},
		}, nil, nil
	})
}

// --- complete_task ---

type CompleteTaskInput struct {
	ID string `json:"id"`
}

func registerCompleteTask(server *mcp.Server, client *apiClient) {
	mcp.AddTool[CompleteTaskInput, any](server, &mcp.Tool{
		Name:        "complete_task",
		Description: "Mark a task as completed. Idempotent — re-completing an already-completed task is a no-op.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input CompleteTaskInput) (*mcp.CallToolResult, any, error) {
		if input.ID == "" {
			return nil, nil, fmt.Errorf("id is required")
		}
		resp, err := client.postJSON(ctx, "/api/v1/tasks/"+url.PathEscape(input.ID)+"/complete", nil)
		if err != nil {
			return nil, nil, fmt.Errorf("API request failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode == 404 {
			return nil, nil, fmt.Errorf("task not found: %s", input.ID)
		}
		if resp.StatusCode != 204 {
			raw, _ := io.ReadAll(resp.Body)
			return nil, nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(raw))
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Task %s marked completed.\n", input.ID)}},
		}, nil, nil
	})
}

// --- delete_task ---

type DeleteTaskInput struct {
	ID string `json:"id"`
}

func registerDeleteTask(server *mcp.Server, client *apiClient) {
	mcp.AddTool[DeleteTaskInput, any](server, &mcp.Tool{
		Name:        "delete_task",
		Description: "Soft-delete a task. The task is hidden from all views and removed from device sync, but the row remains in the database with is_deleted=Y for audit purposes.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input DeleteTaskInput) (*mcp.CallToolResult, any, error) {
		if input.ID == "" {
			return nil, nil, fmt.Errorf("id is required")
		}
		resp, err := client.deleteRequest(ctx, "/api/v1/tasks/"+url.PathEscape(input.ID))
		if err != nil {
			return nil, nil, fmt.Errorf("API request failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode == 404 {
			return nil, nil, fmt.Errorf("task not found: %s", input.ID)
		}
		if resp.StatusCode != 204 {
			raw, _ := io.ReadAll(resp.Body)
			return nil, nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(raw))
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Task %s deleted.\n", input.ID)}},
		}, nil, nil
	})
}

// --- purge_completed_tasks ---

type PurgeCompletedTasksInput struct{}

func registerPurgeCompletedTasks(server *mcp.Server, client *apiClient) {
	mcp.AddTool[PurgeCompletedTasksInput, any](server, &mcp.Tool{
		Name:        "purge_completed_tasks",
		Description: "Soft-delete every completed task in a single call. Housekeeping convenience for clearing the list after a review session. Returns the count affected. This is not reversible through the API.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ PurgeCompletedTasksInput) (*mcp.CallToolResult, any, error) {
		resp, err := client.postJSON(ctx, "/api/v1/tasks/purge-completed", nil)
		if err != nil {
			return nil, nil, fmt.Errorf("API request failed: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			raw, _ := io.ReadAll(resp.Body)
			return nil, nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(raw))
		}
		var body struct {
			Deleted int64 `json:"deleted"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, nil, fmt.Errorf("decode response: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf("Soft-deleted %d completed task(s).\n", body.Deleted),
			}},
		}, nil, nil
	})
}

// --- purge_deleted_tasks ---

// PurgeDeletedTasksInput controls the age cutoff for the hard-purge. Zero
// means "use the server default" (30 days). Negative values are rejected
// server-side.
type PurgeDeletedTasksInput struct {
	OlderThanDays int `json:"older_than_days,omitempty"`
}

func registerPurgeDeletedTasks(server *mcp.Server, client *apiClient) {
	mcp.AddTool[PurgeDeletedTasksInput, any](server, &mcp.Tool{
		Name: "purge_deleted_tasks",
		Description: "PERMANENTLY remove soft-deleted tasks older than older_than_days (default 30, must be > 0). " +
			"This is the only operation that actually frees rows from the task store — every other 'delete' just tombstones. " +
			"Irreversible. Returns purged and skipped counts; skipped means rows that were soft-deleted but inside the safety window. " +
			"A '0 purged, N skipped' result confirms the age gate is working with nothing eligible — distinct from '0 purged, 0 skipped' which means there were no soft-deleted rows at all. " +
			"Pair with list_tasks { include_deleted: true } to confirm what's eligible before running.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input PurgeDeletedTasksInput) (*mcp.CallToolResult, any, error) {
		days := input.OlderThanDays
		path := "/api/v1/tasks/purge-deleted"
		if days > 0 {
			path = fmt.Sprintf("%s?older_than_days=%d", path, days)
		}
		resp, err := client.postJSON(ctx, path, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("API request failed: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			raw, _ := io.ReadAll(resp.Body)
			return nil, nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(raw))
		}
		var body struct {
			Deleted int64 `json:"deleted"`
			Skipped int64 `json:"skipped"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, nil, fmt.Errorf("decode response: %w", err)
		}
		windowDays := days
		if windowDays == 0 {
			windowDays = 30 // server default; matches purgeDeletedDefaultDays
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf("Hard-purged %d task(s); %d skipped (newer than %d days).\n",
					body.Deleted, body.Skipped, windowDays),
			}},
		}, nil, nil
	})
}
