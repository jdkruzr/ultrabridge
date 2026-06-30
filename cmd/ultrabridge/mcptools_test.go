package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func callMCPTool(t *testing.T, server *mcp.Server, name string, input any) (*mcp.CallToolResult, error) {
	t.Helper()
	ctx := context.Background()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	sess, err := mcpClient.Connect(ctx, clientTransport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	b, _ := json.Marshal(input)
	var args map[string]any
	if err := json.Unmarshal(b, &args); err != nil {
		return nil, err
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, err
	}
	if res.IsError {
		if len(res.Content) > 0 {
			if tc, ok := res.Content[0].(*mcp.TextContent); ok {
				return nil, fmt.Errorf("%s", tc.Text)
			}
		}
		return nil, fmt.Errorf("tool returned error")
	}
	return res, nil
}

// formatMCPTask is the surface every MCP client renders. It must surface task
// ATTACHMENTS so the MCP output stays aligned with the REST task shape.
func TestFormatMCPTask_RendersAttachment(t *testing.T) {
	task := mcpTask{
		ID:     "t1",
		Title:  "discovery",
		Status: "needsAction",
		Attachments: []mcpAttachment{{
			URL:      "https://ub.example/api/v1/attachments/abc?sig=x",
			FmtType:  "text/plain",
			Filename: "recognized-text.txt",
			Size:     9,
			Inline:   true,
		}},
	}
	out := formatMCPTask(task)
	want := "Attachment: recognized-text.txt [text/plain, 9 bytes] https://ub.example/api/v1/attachments/abc?sig=x"
	if !strings.Contains(out, want) {
		t.Errorf("attachment not rendered.\n got: %q\nwant substring: %q", out, want)
	}
}

func TestFormatMCPTask_NoAttachmentNoLine(t *testing.T) {
	out := formatMCPTask(mcpTask{ID: "t2", Title: "x", Status: "needsAction"})
	if strings.Contains(out, "Attachment:") {
		t.Errorf("unexpected Attachment line for an attachment-less task:\n%s", out)
	}
}

func TestMCPSearchNotesForwardsRationalFiltersAndReturnsStructuredOutput(t *testing.T) {
	var gotQuery url.Values
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/search" && r.Method == "GET" {
			gotQuery = r.URL.Query()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]map[string]any{{
				"path":         "forestnote://NB1/PG1",
				"page":         0,
				"title":        "Planning",
				"snippet":      "alpha project notes",
				"score":        0.77,
				"source_type":  "forestnote",
				"folder":       "Work",
				"device_model": "Viwoods",
				"created_at":   "2026-06-01T00:00:00Z",
				"modified_at":  "2026-06-02T00:00:00Z",
			}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockServer.Close()

	client := newMCPAPIClient(mockServer.URL, "https://ub.example", "", nil, false)
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	registerMCPTools(server, client)

	res, err := callMCPTool(t, server, "search_notes", searchNotesInput{
		Query:       "alpha",
		Sources:     []string{"forestnote", "boox"},
		Folder:      "Work",
		Location:    "source:forestnote:folder:abc",
		DeviceModel: "Viwoods",
		CreatedFrom: "2026-06-01",
		ModifiedTo:  "2026-06-30",
		Sort:        "date_desc",
		Mode:        "keyword",
		Limit:       5,
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if gotQuery.Get("q") != "alpha" || gotQuery.Get("folder") != "Work" ||
		gotQuery.Get("location") != "source:forestnote:folder:abc" ||
		gotQuery.Get("device_model") != "Viwoods" ||
		gotQuery.Get("created_from") != "2026-06-01" ||
		gotQuery.Get("modified_to") != "2026-06-30" ||
		gotQuery.Get("sort") != "date_desc" ||
		gotQuery.Get("mode") != "keyword" ||
		gotQuery.Get("limit") != "5" {
		t.Fatalf("forwarded query = %v", gotQuery)
	}
	if sources := gotQuery["source"]; len(sources) != 2 || sources[0] != "forestnote" || sources[1] != "boox" {
		t.Fatalf("source params = %v, want [forestnote boox]", sources)
	}

	var out searchNotesOutput
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode structured content: %v", err)
	}
	if out.Count != 1 || out.Results[0].SourceType != "forestnote" || out.Results[0].DetailURL != "https://ub.example/files?detail=forestnote%3A%2F%2FNB1%2FPG1" {
		t.Fatalf("structured output = %+v", out)
	}
	tc := res.Content[0].(*mcp.TextContent)
	if !strings.Contains(tc.Text, "Source type: forestnote") || !strings.Contains(tc.Text, "Device model: Viwoods") {
		t.Fatalf("text fallback missing metadata:\n%s", tc.Text)
	}
}

func TestMCPSearchNotesAcceptsDeprecatedAliases(t *testing.T) {
	var gotQuery url.Values
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]any{})
	}))
	defer mockServer.Close()

	client := newMCPAPIClient(mockServer.URL, "", "", nil, false)
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	registerMCPTools(server, client)

	if _, err := callMCPTool(t, server, "search_notes", searchNotesInput{
		Query:    "alpha",
		Device:   "Palma2",
		DateFrom: "2026-01-01",
		DateTo:   "2026-02-01",
	}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if gotQuery.Get("device_model") != "Palma2" ||
		gotQuery.Get("modified_from") != "2026-01-01" ||
		gotQuery.Get("modified_to") != "2026-02-01" {
		t.Fatalf("deprecated aliases forwarded as %v", gotQuery)
	}
}

func TestMCPListTextBoxes(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/forestnote/text-boxes" && r.URL.Query().Get("notebook") == "NB1" && r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": "BOX1", "page_id": "PG1", "text": "shopping list", "z": 0},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockServer.Close()

	client := newMCPAPIClient(mockServer.URL, "", "", nil, false)
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	registerMCPTools(server, client)

	res, err := callMCPTool(t, server, "list_text_boxes", listTextBoxesInput{NotebookID: "NB1"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Content[0])
	}
	if !strings.Contains(tc.Text, "BOX1") || !strings.Contains(tc.Text, "shopping list") {
		t.Errorf("output missing box id/text: %q", tc.Text)
	}
}

func TestMCPListTextBoxesRequiresNotebook(t *testing.T) {
	client := newMCPAPIClient("http://unused", "", "", nil, false)
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	registerMCPTools(server, client)
	if _, err := callMCPTool(t, server, "list_text_boxes", listTextBoxesInput{}); err == nil {
		t.Error("expected error for missing notebook_id")
	}
}

func TestMCPEditTextBox(t *testing.T) {
	var gotBody map[string]string
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/forestnote/text-boxes/edit" && r.Method == "POST" {
			json.NewDecoder(r.Body).Decode(&gotBody)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockServer.Close()

	client := newMCPAPIClient(mockServer.URL, "", "", nil, false)
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	registerMCPTools(server, client)

	res, err := callMCPTool(t, server, "edit_text_box", editTextBoxInput{BoxID: "BOX1", Text: "new text"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if gotBody["id"] != "BOX1" || gotBody["text"] != "new text" {
		t.Errorf("forwarded body = %+v, want id=BOX1 text='new text'", gotBody)
	}
	tc := res.Content[0].(*mcp.TextContent)
	if !strings.Contains(tc.Text, "BOX1") {
		t.Errorf("expected confirmation mentioning the box, got %q", tc.Text)
	}
}

func TestMCPEditTextBoxRequiresID(t *testing.T) {
	client := newMCPAPIClient("http://unused", "", "", nil, false)
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	registerMCPTools(server, client)
	if _, err := callMCPTool(t, server, "edit_text_box", editTextBoxInput{Text: "x"}); err == nil {
		t.Error("expected error for missing box_id")
	}
}

func TestMCPEditTextBoxAPIError(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"text box not found"}`))
	}))
	defer mockServer.Close()

	client := newMCPAPIClient(mockServer.URL, "", "", nil, false)
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	registerMCPTools(server, client)

	if _, err := callMCPTool(t, server, "edit_text_box", editTextBoxInput{BoxID: "GONE", Text: "x"}); err == nil {
		t.Error("expected error surfaced from a non-200 API response")
	}
}

func TestMCPListTasksReturnsStructuredOutput(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/tasks" && r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]mcpTask{{
				ID:         "t1",
				Title:      "follow up",
				Status:     "needs_action",
				Priority:   stringPtr("1"),
				Categories: []string{"urgent"},
				ForestNote: &mcpTaskForestNote{NotebookID: "NB1", NotebookName: "Planning", Source: "lasso"},
			}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockServer.Close()

	client := newMCPAPIClient(mockServer.URL, "", "", nil, false)
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	registerMCPTools(server, client)

	res, err := callMCPTool(t, server, "list_tasks", listTasksInput{Priority: "1", Category: "urgent"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var out taskListOutput
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode structured content: %v", err)
	}
	if out.Count != 1 || out.Tasks[0].ID != "t1" || out.Tasks[0].ForestNote.NotebookID != "NB1" {
		t.Fatalf("structured output = %+v", out)
	}
	tc := res.Content[0].(*mcp.TextContent)
	if !strings.Contains(tc.Text, "From ForestNote notebook: Planning") {
		t.Fatalf("text fallback missing provenance:\n%s", tc.Text)
	}
}

func stringPtr(s string) *string { return &s }
