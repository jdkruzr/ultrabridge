package main // FCIS: Imperative Shell

// MCP tools for task manipulation. Each tool is a thin wrapper over the
// /api/v1/tasks/* endpoints; the real business logic lives there.
//
// All mutations flow through UltraBridge's existing sync path — a change
// made here propagates to the configured CalDAV device on the next sync
// cycle (UB-wins conflict resolution).

import (
	"context"
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
		sb.WriteString(fmt.Sprintf("URL: %s\n", *t.URL))
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
	Title string `json:"title"`
	DueAt string `json:"due_at,omitempty"` // RFC3339; optional
}

func registerCreateTask(server *mcp.Server, client *apiClient) {
	mcp.AddTool[CreateTaskInput, any](server, &mcp.Tool{
		Name:        "create_task",
		Description: "Create a new task. Requires a title; due_at is optional and must be RFC3339 when provided. The new task syncs to configured CalDAV devices on the next sync cycle.",
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
// leave the task unchanged. ClearDueAt wins over DueAt when both are set.
type UpdateTaskInput struct {
	ID         string  `json:"id"`
	Title      *string `json:"title,omitempty"`
	DueAt      *string `json:"due_at,omitempty"` // RFC3339
	ClearDueAt bool    `json:"clear_due_at,omitempty"`
	Detail     *string `json:"detail,omitempty"`
}

func registerUpdateTask(server *mcp.Server, client *apiClient) {
	mcp.AddTool[UpdateTaskInput, any](server, &mcp.Tool{
		Name:        "update_task",
		Description: "Partially update a task. Only supplied fields are changed. Use clear_due_at=true to remove an existing due date (takes priority over due_at when both set). Detail can be cleared by sending an empty string. Title cannot be empty.",
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
		Description: "Soft-delete every completed task in a single call. Housekeeping convenience for clearing the list after a review session. This is not reversible through the API.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ PurgeCompletedTasksInput) (*mcp.CallToolResult, any, error) {
		resp, err := client.postJSON(ctx, "/api/v1/tasks/purge-completed", nil)
		if err != nil {
			return nil, nil, fmt.Errorf("API request failed: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 204 {
			raw, _ := io.ReadAll(resp.Body)
			return nil, nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(raw))
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "All completed tasks purged.\n"}},
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
			"Irreversible. Returns the number of rows removed. Use this to clear the trash backlog; pair with list_tasks { include_deleted: true } " +
			"to confirm what's eligible before running.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input PurgeDeletedTasksInput) (*mcp.CallToolResult, any, error) {
		path := "/api/v1/tasks/purge-deleted"
		if input.OlderThanDays > 0 {
			path = fmt.Sprintf("%s?older_than_days=%d", path, input.OlderThanDays)
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
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, nil, fmt.Errorf("decode response: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf("Hard-purged %d soft-deleted task(s).\n", body.Deleted),
			}},
		}, nil, nil
	})
}
