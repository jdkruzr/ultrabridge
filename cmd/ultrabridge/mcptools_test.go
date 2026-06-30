package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
