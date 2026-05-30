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

// callTool invokes a registered tool by name via an in-process MCP client/server
// pair (generic over the existing per-tool helpers).
func callTool(t *testing.T, server *mcp.Server, name string, input any) (*mcp.CallToolResult, error) {
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

func TestListTextBoxes(t *testing.T) {
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

	client := newAPIClient(mockServer.URL, "", "", "", "")
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	registerTools(server, client)

	res, err := callTool(t, server, "list_text_boxes", ListTextBoxesInput{NotebookID: "NB1"})
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

func TestListTextBoxesRequiresNotebook(t *testing.T) {
	client := newAPIClient("http://unused", "", "", "", "")
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	registerTools(server, client)
	if _, err := callTool(t, server, "list_text_boxes", ListTextBoxesInput{}); err == nil {
		t.Error("expected error for missing notebook_id")
	}
}

func TestEditTextBox(t *testing.T) {
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

	client := newAPIClient(mockServer.URL, "", "", "", "")
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	registerTools(server, client)

	res, err := callTool(t, server, "edit_text_box", EditTextBoxInput{BoxID: "BOX1", Text: "new text"})
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

func TestEditTextBoxRequiresID(t *testing.T) {
	client := newAPIClient("http://unused", "", "", "", "")
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	registerTools(server, client)
	if _, err := callTool(t, server, "edit_text_box", EditTextBoxInput{Text: "x"}); err == nil {
		t.Error("expected error for missing box_id")
	}
}

func TestEditTextBoxAPIError(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"text box not found"}`))
	}))
	defer mockServer.Close()

	client := newAPIClient(mockServer.URL, "", "", "", "")
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	registerTools(server, client)

	if _, err := callTool(t, server, "edit_text_box", EditTextBoxInput{BoxID: "GONE", Text: "x"}); err == nil {
		t.Error("expected error surfaced from a non-200 API response")
	}
}
