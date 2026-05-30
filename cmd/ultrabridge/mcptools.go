package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpAPIClient calls UltraBridge's own JSON API endpoints using a
// persistent internal bearer token for self-authentication.
//
// baseURL is the loopback API URL (e.g. "http://localhost:8443") used for
// the underlying HTTP requests — that's what the binary actually talks to.
// publicBaseURL is the externally-reachable URL of this deployment
// (boox_external_base_url setting) — emitted into search_notes deep-links
// so an LLM/remote consumer rendering the result can click through. When
// publicBaseURL is empty, formatters fall back to baseURL (works only for
// callers on the same host).
type mcpAPIClient struct {
	baseURL       string
	publicBaseURL string
	internalToken string
	http          *http.Client
	logger        *slog.Logger
	verbose       bool
}

func newMCPAPIClient(baseURL, publicBaseURL, internalToken string, logger *slog.Logger, verbose bool) *mcpAPIClient {
	return &mcpAPIClient{
		baseURL:       baseURL,
		publicBaseURL: publicBaseURL,
		internalToken: internalToken,
		http:          &http.Client{},
		logger:        logger,
		verbose:       verbose,
	}
}

// displayBaseURL returns the URL to use when building deep-links rendered
// back to a human/LLM consumer. Prefers the public base when set; falls
// back to the loopback baseURL when not (which means same-host clicks work
// but remote clicks see localhost).
func (c *mcpAPIClient) displayBaseURL() string {
	if c.publicBaseURL != "" {
		return strings.TrimRight(c.publicBaseURL, "/")
	}
	return c.baseURL
}

func (c *mcpAPIClient) get(ctx context.Context, path string) (*http.Response, error) {
	return c.request(ctx, http.MethodGet, path, nil)
}

// postJSON POSTs a JSON body (or nil for no-body side-effect endpoints).
func (c *mcpAPIClient) postJSON(ctx context.Context, path string, body interface{}) (*http.Response, error) {
	return c.request(ctx, http.MethodPost, path, body)
}

// patchJSON PATCHes a JSON body.
func (c *mcpAPIClient) patchJSON(ctx context.Context, path string, body interface{}) (*http.Response, error) {
	return c.request(ctx, http.MethodPatch, path, body)
}

// deleteRequest issues a DELETE.
func (c *mcpAPIClient) deleteRequest(ctx context.Context, path string) (*http.Response, error) {
	return c.request(ctx, http.MethodDelete, path, nil)
}

func (c *mcpAPIClient) request(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.internalToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.internalToken)
	}
	return c.http.Do(req)
}

// --- Task tool input types ---

type listTasksInput struct {
	Status    string `json:"status,omitempty"`
	DueBefore string `json:"due_before,omitempty"`
	DueAfter  string `json:"due_after,omitempty"`
	// ForestNote provenance + metadata filters; see the standalone
	// cmd/ub-mcp/tasks.go for the full filter semantics. These are mirror
	// fields to keep the two MCP surfaces aligned.
	NotebookID     string `json:"notebook_id,omitempty"`
	NotebookName   string `json:"notebook_name,omitempty"`
	Source         string `json:"source,omitempty"`
	Category       string `json:"category,omitempty"`
	Priority       string `json:"priority,omitempty"`
	IncludeDeleted bool   `json:"include_deleted,omitempty"`
}

type getTaskInput struct {
	ID string `json:"id"`
}

type createTaskInput struct {
	Title      string   `json:"title"`
	DueAt      string   `json:"due_at,omitempty"`
	Detail     string   `json:"detail,omitempty"`
	URL        string   `json:"url,omitempty"`
	Priority   string   `json:"priority,omitempty"`
	Categories []string `json:"categories,omitempty"`
	Comment    string   `json:"comment,omitempty"`
}

type updateTaskInput struct {
	ID            string    `json:"id"`
	Title         *string   `json:"title,omitempty"`
	DueAt         *string   `json:"due_at,omitempty"`
	ClearDueAt    bool      `json:"clear_due_at,omitempty"`
	Detail        *string   `json:"detail,omitempty"`
	URL           *string   `json:"url,omitempty"`
	ClearURL      bool      `json:"clear_url,omitempty"`
	Priority      *string   `json:"priority,omitempty"`
	ClearPriority bool      `json:"clear_priority,omitempty"`
	Categories    *[]string `json:"categories,omitempty"`
	Comment       *string   `json:"comment,omitempty"`
	ClearComment  bool      `json:"clear_comment,omitempty"`
}

type completeTaskInput struct {
	ID string `json:"id"`
}

type deleteTaskInput struct {
	ID string `json:"id"`
}

type purgeCompletedTasksInput struct{}

// purgeDeletedTasksInput controls the age cutoff for the hard-purge. Zero
// means "use the server default" (30 days). Negative values are rejected
// server-side. Mirrors PurgeDeletedTasksInput in cmd/ub-mcp/tasks.go.
type purgeDeletedTasksInput struct {
	OlderThanDays int `json:"older_than_days,omitempty"`
}

// mcpTaskLink mirrors service.TaskLink (back-reference to the note a task
// was auto-extracted from). Local copy so this file doesn't import the
// internal service package.
type mcpTaskLink struct {
	AppName  string `json:"app_name"`
	FilePath string `json:"file_path"`
	Page     int    `json:"page"`
}

// mcpNativeDeepLink mirrors the Supernote/Viwoods native deep-link blob
// stuffed into the URL field on device-created tasks. See the standalone
// ub-mcp/tasks.go for the full rationale; both surfaces decode it for the
// same reason (avoid dumping base64 walls into LLM context).
type mcpNativeDeepLink struct {
	AppName  string `json:"appName"`
	FileID   string `json:"fileId"`
	FilePath string `json:"filePath"`
	Page     int    `json:"page"`
	PageID   string `json:"pageId"`
	Filename string `json:"-"`
}

// decodeMCPNativeDeepLink is the in-process MCP twin of decodeNativeDeepLink
// in cmd/ub-mcp/tasks.go. Kept separate to preserve the package boundary
// between the two MCP surfaces (no shared internal package for this) — the
// duplication is documented in cmd/ub-mcp/CLAUDE.md's "two surfaces" note.
func decodeMCPNativeDeepLink(raw string) (mcpNativeDeepLink, bool) {
	// `eyJ` is the base64 of `{"<letter>`; every native deep-link payload
	// has an ASCII-letter first key (`appName`, `fileId`, etc.) so its
	// encoding always starts with this prefix. Non-deep-link URLs that
	// happen to start with `eyJ` are caught by the json.Unmarshal +
	// AppName checks below.
	if !strings.HasPrefix(raw, "eyJ") {
		return mcpNativeDeepLink{}, false
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return mcpNativeDeepLink{}, false
	}
	var dl mcpNativeDeepLink
	if err := json.Unmarshal(decoded, &dl); err != nil {
		return mcpNativeDeepLink{}, false
	}
	if dl.AppName == "" {
		return mcpNativeDeepLink{}, false
	}
	if dl.FilePath != "" {
		if i := strings.LastIndex(dl.FilePath, "/"); i >= 0 {
			dl.Filename = dl.FilePath[i+1:]
		} else {
			dl.Filename = dl.FilePath
		}
	}
	return dl, true
}

// mcpTaskForestNote mirrors service.TaskForestNote. Local copy keeps this
// file decoupled from internal/service.
type mcpTaskForestNote struct {
	NotebookID   string `json:"notebook_id,omitempty"`
	PageID       string `json:"page_id,omitempty"`
	NotebookName string `json:"notebook_name,omitempty"`
	Source       string `json:"source,omitempty"`
	NativeURL    string `json:"native_url,omitempty"`
}

// mcpTask mirrors service.Task's JSON shape for decoding /api/v1/tasks
// responses.
type mcpTask struct {
	ID          string             `json:"id"`
	Title       string             `json:"title"`
	Status      string             `json:"status"`
	CreatedAt   time.Time          `json:"created_at"`
	DueAt       *time.Time         `json:"due_at,omitempty"`
	CompletedAt *time.Time         `json:"completed_at,omitempty"`
	Detail      *string            `json:"detail,omitempty"`
	Links       *mcpTaskLink       `json:"links,omitempty"`
	URL         *string            `json:"url,omitempty"`
	Priority    *string            `json:"priority,omitempty"`
	Categories  []string           `json:"categories,omitempty"`
	ForestNote  *mcpTaskForestNote `json:"forestnote,omitempty"`
	Comment     string             `json:"comment,omitempty"`
	Deleted     bool               `json:"deleted,omitempty"`
}

func formatMCPTask(t mcpTask) string {
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
		// Friendly-decode the Supernote/Viwoods native deep-link blob (see
		// decodeMCPNativeDeepLink) so list_tasks doesn't dump a wall of
		// base64 into the LLM context. Falls through to the bare URL when
		// the value isn't a recognized native-deep-link payload.
		if dl, ok := decodeMCPNativeDeepLink(*t.URL); ok && dl.Filename != "" {
			if dl.Page > 0 {
				sb.WriteString(fmt.Sprintf("Source: %s (page %d)\n", dl.Filename, dl.Page))
			} else {
				sb.WriteString(fmt.Sprintf("Source: %s\n", dl.Filename))
			}
			sb.WriteString(fmt.Sprintf("URL (native deep-link, base64): %s\n", *t.URL))
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

// MCP tool input types

type searchNotesInput struct {
	Query    string `json:"query"`
	Folder   string `json:"folder,omitempty"`
	Device   string `json:"device,omitempty"`
	DateFrom string `json:"date_from,omitempty"`
	DateTo   string `json:"date_to,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

type getNotePagesInput struct {
	NotePath string `json:"note_path"`
}

type getNoteImageInput struct {
	NotePath string `json:"note_path"`
	Page     int    `json:"page"`
}

func registerMCPTools(server *mcp.Server, client *mcpAPIClient) {
	// search_notes
	mcp.AddTool[searchNotesInput, any](server, &mcp.Tool{
		Name:        "search_notes",
		Description: "Search handwritten notes by keyword query. Returns matching pages with text content, metadata, and links to the UltraBridge web UI. Supports filtering by folder, device, and date range.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input searchNotesInput) (*mcp.CallToolResult, any, error) {
		if client.verbose && client.logger != nil {
			client.logger.Info("MCP tool call", "tool", "search_notes", "input", input)
		}
		if input.Query == "" {
			return nil, nil, fmt.Errorf("query is required")
		}
		params := url.Values{"q": {input.Query}}
		if input.Folder != "" {
			params.Set("folder", input.Folder)
		}
		if input.Device != "" {
			params.Set("device", input.Device)
		}
		if input.DateFrom != "" {
			params.Set("from", input.DateFrom)
		}
		if input.DateTo != "" {
			params.Set("to", input.DateTo)
		}
		limit := input.Limit
		if limit <= 0 {
			limit = 10
		}
		params.Set("limit", fmt.Sprintf("%d", limit))

		resp, err := client.get(ctx, "/api/search?"+params.Encode())
		if err != nil {
			return nil, nil, fmt.Errorf("API request failed: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			return nil, nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body))
		}

		// API shape is service.SearchResult with snake_case: path/page/
		// snippet/score. Decoder previously expected richer fields
		// (note_path/body_text/etc.) the v1 API doesn't emit; every field
		// silently got its zero value and MCP produced empty-body results.
		var results []struct {
			Path    string  `json:"path"`
			Page    int     `json:"page"`
			Snippet string  `json:"snippet"`
			Score   float64 `json:"score"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
			return nil, nil, fmt.Errorf("decode response: %w", err)
		}

		var sb strings.Builder
		for i, r := range results {
			sb.WriteString(fmt.Sprintf("--- Result %d ---\n", i+1))
			sb.WriteString(fmt.Sprintf("Note: %s (page %d)\n", r.Path, r.Page))
			detailURL := fmt.Sprintf("%s/files?detail=%s", client.displayBaseURL(), url.QueryEscape(r.Path))
			sb.WriteString(fmt.Sprintf("URL: %s\n", detailURL))
			sb.WriteString(fmt.Sprintf("Text:\n%s\n\n", r.Snippet))
		}
		if len(results) == 0 {
			sb.WriteString("No results found.\n")
		}

		if client.verbose && client.logger != nil {
			client.logger.Info("MCP tool result", "tool", "search_notes", "results", len(results))
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: sb.String()},
			},
		}, nil, nil
	})

	// get_note_pages
	mcp.AddTool[getNotePagesInput, any](server, &mcp.Tool{
		Name:        "get_note_pages",
		Description: "Get all page text content for a specific note. Returns pages ordered by page number with body text and title.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input getNotePagesInput) (*mcp.CallToolResult, any, error) {
		if client.verbose && client.logger != nil {
			client.logger.Info("MCP tool call", "tool", "get_note_pages", "input", input)
		}
		if input.NotePath == "" {
			return nil, nil, fmt.Errorf("note_path is required")
		}
		params := url.Values{"path": {input.NotePath}}
		resp, err := client.get(ctx, "/api/notes/pages?"+params.Encode())
		if err != nil {
			return nil, nil, fmt.Errorf("API request failed: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == 404 {
			return nil, nil, fmt.Errorf("note not found: %s", input.NotePath)
		}
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			return nil, nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body))
		}

		var pages []struct {
			Page      int    `json:"page"`
			BodyText  string `json:"body_text"`
			TitleText string `json:"title_text"`
			Keywords  string `json:"keywords"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&pages); err != nil {
			return nil, nil, fmt.Errorf("decode response: %w", err)
		}

		var sb strings.Builder
		for _, p := range pages {
			sb.WriteString(fmt.Sprintf("--- Page %d ---\n", p.Page))
			if p.TitleText != "" {
				sb.WriteString(fmt.Sprintf("Title: %s\n", p.TitleText))
			}
			sb.WriteString(p.BodyText)
			sb.WriteString("\n\n")
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: sb.String()},
			},
		}, nil, nil
	})

	// get_note_image
	mcp.AddTool[getNoteImageInput, any](server, &mcp.Tool{
		Name:        "get_note_image",
		Description: "Get the rendered page image (JPEG) from a note. Returns the image for visual inspection of handwritten content.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input getNoteImageInput) (*mcp.CallToolResult, any, error) {
		if client.verbose && client.logger != nil {
			client.logger.Info("MCP tool call", "tool", "get_note_image", "input", input)
		}
		if input.NotePath == "" {
			return nil, nil, fmt.Errorf("note_path is required")
		}
		params := url.Values{
			"path": {input.NotePath},
			"page": {fmt.Sprintf("%d", input.Page)},
		}
		resp, err := client.get(ctx, "/api/notes/pages/image?"+params.Encode())
		if err != nil {
			return nil, nil, fmt.Errorf("API request failed: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == 404 {
			return nil, nil, fmt.Errorf("page image not found: %s page %d", input.NotePath, input.Page)
		}
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			return nil, nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body))
		}

		imageData, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, nil, fmt.Errorf("read image: %w", err)
		}

		if client.verbose && client.logger != nil {
			client.logger.Info("MCP tool result", "tool", "get_note_image", "bytes", len(imageData))
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.ImageContent{
					Data:     imageData,
					MIMEType: "image/jpeg",
				},
			},
		}, nil, nil
	})

	// --- Task tools ---

	// list_tasks
	mcp.AddTool[listTasksInput, any](server, &mcp.Tool{
		Name: "list_tasks",
		Description: "List tasks from UltraBridge. Optional filters: " +
			"status (needs_action / completed / all, default all); " +
			"due_before / due_after as RFC3339 (tasks with no due date excluded when either is set); " +
			"notebook_id / notebook_name / source (ForestNote provenance — match tasks created from a specific notebook or input source); " +
			"category (single VTODO CATEGORIES entry, case-sensitive); " +
			"priority (VTODO PRIORITY value 1-9); " +
			"include_deleted=true to surface soft-tombstoned rows (default false). " +
			"Returns title, status, due/completed times, URL, priority, categories, ForestNote provenance, and detail when present.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input listTasksInput) (*mcp.CallToolResult, any, error) {
		if client.verbose && client.logger != nil {
			client.logger.Info("MCP tool call", "tool", "list_tasks", "input", input)
		}
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
		var tasks []mcpTask
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
			sb.WriteString(formatMCPTask(t))
			sb.WriteString("\n")
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: sb.String()}},
		}, nil, nil
	})

	// get_task
	mcp.AddTool[getTaskInput, any](server, &mcp.Tool{
		Name:        "get_task",
		Description: "Fetch a single task by id. Returns the full task surface: title, status, due/completed times, URL, priority, categories, detail, comment, and any ForestNote provenance (notebook id+name, page id, source, native URL) when the task came from a notebook page.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input getTaskInput) (*mcp.CallToolResult, any, error) {
		if client.verbose && client.logger != nil {
			client.logger.Info("MCP tool call", "tool", "get_task", "input", input)
		}
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
		var t mcpTask
		if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
			return nil, nil, fmt.Errorf("decode response: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: formatMCPTask(t)}},
		}, nil, nil
	})

	// create_task
	mcp.AddTool[createTaskInput, any](server, &mcp.Tool{
		Name: "create_task",
		Description: "Create a new task. Requires a title; everything else is optional. " +
			"due_at must be RFC3339 when provided. " +
			"url and priority land in dedicated columns (priority is the VTODO PRIORITY value, \"1\"-\"9\"). " +
			"categories and comment ride in the iCal blob, so they're readable via get_task right after create. " +
			"The new task syncs to configured CalDAV devices on the next sync cycle.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input createTaskInput) (*mcp.CallToolResult, any, error) {
		if client.verbose && client.logger != nil {
			client.logger.Info("MCP tool call", "tool", "create_task", "input", input)
		}
		if input.Title == "" {
			return nil, nil, fmt.Errorf("title is required")
		}
		body := map[string]interface{}{"title": input.Title}
		if input.DueAt != "" {
			parsed, err := time.Parse(time.RFC3339, input.DueAt)
			if err != nil {
				return nil, nil, fmt.Errorf("due_at must be RFC3339: %w", err)
			}
			body["due_at"] = parsed
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
		var created mcpTask
		if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
			return nil, nil, fmt.Errorf("decode response: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Created:\n" + formatMCPTask(created)}},
		}, nil, nil
	})

	// update_task
	mcp.AddTool[updateTaskInput, any](server, &mcp.Tool{
		Name: "update_task",
		Description: "Partially update a task. Only supplied fields are changed. " +
			"Use clear_due_at / clear_url / clear_priority / clear_comment to null out a column (the Clear flag wins over the value pointer when both are set). " +
			"Categories is wholesale: send a list to replace the existing set, an empty list to clear, or omit to leave unchanged. " +
			"Detail and comment can be cleared by sending an empty string. Title cannot be empty.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input updateTaskInput) (*mcp.CallToolResult, any, error) {
		if client.verbose && client.logger != nil {
			client.logger.Info("MCP tool call", "tool", "update_task", "input", input)
		}
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
		var updated mcpTask
		if err := json.NewDecoder(resp.Body).Decode(&updated); err != nil {
			return nil, nil, fmt.Errorf("decode response: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Updated:\n" + formatMCPTask(updated)}},
		}, nil, nil
	})

	// complete_task
	mcp.AddTool[completeTaskInput, any](server, &mcp.Tool{
		Name:        "complete_task",
		Description: "Mark a task as completed. Idempotent — re-completing an already-completed task is a no-op.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input completeTaskInput) (*mcp.CallToolResult, any, error) {
		if client.verbose && client.logger != nil {
			client.logger.Info("MCP tool call", "tool", "complete_task", "input", input)
		}
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

	// delete_task
	mcp.AddTool[deleteTaskInput, any](server, &mcp.Tool{
		Name:        "delete_task",
		Description: "Soft-delete a task. The task is hidden from all views and removed from device sync, but the row remains in the database with is_deleted=Y for audit purposes.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input deleteTaskInput) (*mcp.CallToolResult, any, error) {
		if client.verbose && client.logger != nil {
			client.logger.Info("MCP tool call", "tool", "delete_task", "input", input)
		}
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

	// purge_completed_tasks
	mcp.AddTool[purgeCompletedTasksInput, any](server, &mcp.Tool{
		Name:        "purge_completed_tasks",
		Description: "Soft-delete every completed task in a single call. Housekeeping convenience for clearing the list after a review session. Returns the count affected. This is not reversible through the API.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ purgeCompletedTasksInput) (*mcp.CallToolResult, any, error) {
		if client.verbose && client.logger != nil {
			client.logger.Info("MCP tool call", "tool", "purge_completed_tasks")
		}
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

	// purge_deleted_tasks — the *only* path that actually frees rows from
	// the task store. Every other "delete" tombstones. Mirrors the same
	// tool on the standalone cmd/ub-mcp binary so both MCP surfaces expose
	// the same capability.
	mcp.AddTool[purgeDeletedTasksInput, any](server, &mcp.Tool{
		Name: "purge_deleted_tasks",
		Description: "PERMANENTLY remove soft-deleted tasks older than older_than_days (default 30, must be > 0). " +
			"This is the only operation that actually frees rows from the task store — every other 'delete' just tombstones. " +
			"Irreversible. Returns purged and skipped counts; skipped means rows that were soft-deleted but inside the safety window. " +
			"A '0 purged, N skipped' result confirms the age gate is working with nothing eligible — distinct from '0 purged, 0 skipped' which means there were no soft-deleted rows at all. " +
			"Pair with list_tasks { include_deleted: true } to confirm what's eligible before running.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input purgeDeletedTasksInput) (*mcp.CallToolResult, any, error) {
		if client.verbose && client.logger != nil {
			client.logger.Info("MCP tool call", "tool", "purge_deleted_tasks", "input", input)
		}
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
			windowDays = 30
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf("Hard-purged %d task(s); %d skipped (newer than %d days).\n",
					body.Deleted, body.Skipped, windowDays),
			}},
		}, nil, nil
	})
}
