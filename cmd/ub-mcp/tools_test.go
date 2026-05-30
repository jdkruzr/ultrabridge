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

// TestSearchNotesBasic verifies search_notes tool returns formatted text with
// the fields the /api/search endpoint actually emits (service.SearchResult
// shape: path / page / snippet / score). Previously this test mocked a
// richer response (note_path/body_text/title_text/folder/device/note_date/
// url) that the real API never emitted — which is why the decoder silently
// produced empty-body results in production until a user hit it.
func TestSearchNotesBasic(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/search" && r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{
					"path":    "/notes/test.note",
					"page":    0,
					"snippet": "This is test content",
					"score":   0.95,
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockServer.Close()

	client := newAPIClient(mockServer.URL, "", "", "", "")
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "test-server",
		Version: "1.0.0",
	}, nil)

	registerTools(server, client)

	input := SearchNotesInput{Query: "test"}
	result, _, err := testCallSearchNotesTool(server, client, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result, got nil")
	}
	if len(result.Content) == 0 {
		t.Fatal("expected content in result")
	}
	textContent, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	text := textContent.Text
	if text == "" {
		t.Fatal("expected non-empty text")
	}
	if !strings.Contains(text, "/notes/test.note") {
		t.Errorf("missing note path in response: %s", text)
	}
	if !strings.Contains(text, "page 0") {
		t.Errorf("missing page number in response: %s", text)
	}
	if !strings.Contains(text, "This is test content") {
		t.Errorf("missing snippet body in response: %s", text)
	}
	// URL format: {baseURL}/files?detail={urlencoded path}
	if !strings.Contains(text, mockServer.URL+"/files?detail=") {
		t.Errorf("missing web-UI detail URL in response: %s", text)
	}
	if !strings.Contains(text, "%2Fnotes%2Ftest.note") {
		t.Errorf("URL does not include the encoded path: %s", text)
	}
}

// TestSearchNotesEmptyQuery verifies error when query is missing.
func TestSearchNotesEmptyQuery(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]map[string]interface{}{})
	}))
	defer mockServer.Close()

	client := newAPIClient(mockServer.URL, "", "", "", "")
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "test-server",
		Version: "1.0.0",
	}, nil)

	registerTools(server, client)

	input := SearchNotesInput{Query: ""}
	_, _, err := testCallSearchNotesTool(server, client, input)

	if err == nil {
		t.Fatal("expected error for empty query")
	}
	if !strings.Contains(err.Error(), "query is required") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestSearchNotesWithFilters verifies query parameters are passed correctly.
func TestSearchNotesWithFilters(t *testing.T) {
	// Capture the request to verify parameters
	var capturedQuery string
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/search" && r.Method == "GET" {
			capturedQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]map[string]interface{}{})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockServer.Close()

	client := newAPIClient(mockServer.URL, "", "", "", "")
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "test-server",
		Version: "1.0.0",
	}, nil)

	registerTools(server, client)

	input := SearchNotesInput{
		Query:    "handwriting",
		Folder:   "Work",
		Device:   "Supernote",
		DateFrom: "2026-04-01",
		DateTo:   "2026-04-08",
		Limit:    20,
	}
	_, _, err := testCallSearchNotesTool(server, client, input)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify all parameters are in the query
	if !strings.Contains(capturedQuery, "q=handwriting") {
		t.Errorf("missing query param: %s", capturedQuery)
	}
	if !strings.Contains(capturedQuery, "folder=Work") {
		t.Errorf("missing folder param: %s", capturedQuery)
	}
	if !strings.Contains(capturedQuery, "device=Supernote") {
		t.Errorf("missing device param: %s", capturedQuery)
	}
	if !strings.Contains(capturedQuery, "from=2026-04-01") {
		t.Errorf("missing from param: %s", capturedQuery)
	}
	if !strings.Contains(capturedQuery, "to=2026-04-08") {
		t.Errorf("missing to param: %s", capturedQuery)
	}
	if !strings.Contains(capturedQuery, "limit=20") {
		t.Errorf("missing limit param: %s", capturedQuery)
	}
}

// TestSearchNotesDefaultLimit verifies default limit of 10 is applied.
func TestSearchNotesDefaultLimit(t *testing.T) {
	var capturedQuery string
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/search" && r.Method == "GET" {
			capturedQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]map[string]interface{}{})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockServer.Close()

	client := newAPIClient(mockServer.URL, "", "", "", "")
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "test-server",
		Version: "1.0.0",
	}, nil)

	registerTools(server, client)

	input := SearchNotesInput{Query: "test"}
	_, _, err := testCallSearchNotesTool(server, client, input)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(capturedQuery, "limit=10") {
		t.Errorf("expected default limit=10, got: %s", capturedQuery)
	}
}

// TestGetNotePagesValid verifies get_note_pages returns ordered page text.
func TestGetNotePagesValid(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/notes/pages" && r.URL.Query().Get("path") == "/path/to/note.note" && r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{
					"page":       0,
					"body_text":  "Page 0 content",
					"title_text": "Introduction",
					"keywords":   "intro, start",
				},
				{
					"page":       1,
					"body_text":  "Page 1 content",
					"title_text": "Details",
					"keywords":   "details, info",
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockServer.Close()

	client := newAPIClient(mockServer.URL, "", "", "", "")
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "test-server",
		Version: "1.0.0",
	}, nil)

	registerTools(server, client)

	input := GetNotePagesInput{NotePath: "/path/to/note.note"}
	result, _, err := testCallGetNotePagesTool(server, client, input)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result == nil || len(result.Content) == 0 {
		t.Fatal("expected content in result")
	}

	textContent, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}

	text := textContent.Text

	// Verify pages are ordered and contain expected content
	if !strings.Contains(text, "Page 0") {
		t.Error("missing Page 0 in response")
	}
	if !strings.Contains(text, "Introduction") {
		t.Error("missing Introduction title")
	}
	if !strings.Contains(text, "Page 0 content") {
		t.Error("missing Page 0 content")
	}
	if !strings.Contains(text, "Page 1") {
		t.Error("missing Page 1 in response")
	}
	if !strings.Contains(text, "Details") {
		t.Error("missing Details title")
	}
	if !strings.Contains(text, "Page 1 content") {
		t.Error("missing Page 1 content")
	}
}

// TestGetNotePagesNotFound verifies error when note doesn't exist.
func TestGetNotePagesNotFound(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockServer.Close()

	client := newAPIClient(mockServer.URL, "", "", "", "")
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "test-server",
		Version: "1.0.0",
	}, nil)

	registerTools(server, client)

	input := GetNotePagesInput{NotePath: "/nonexistent/note.note"}
	_, _, err := testCallGetNotePagesTool(server, client, input)

	if err == nil {
		t.Fatal("expected error for nonexistent note")
	}
	if !strings.Contains(err.Error(), "note not found") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestGetNotePagesEmptyPath verifies error when path is missing.
func TestGetNotePagesEmptyPath(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockServer.Close()

	client := newAPIClient(mockServer.URL, "", "", "", "")
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "test-server",
		Version: "1.0.0",
	}, nil)

	registerTools(server, client)

	input := GetNotePagesInput{NotePath: ""}
	_, _, err := testCallGetNotePagesTool(server, client, input)

	if err == nil {
		t.Fatal("expected error for empty path")
	}
	if !strings.Contains(err.Error(), "note_path is required") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestGetNoteImageValid verifies get_note_image returns base64-encoded JPEG with correct MIME type.
func TestGetNoteImageValid(t *testing.T) {
	// Create test JPEG data (minimal valid JPEG header)
	testImageData := []byte{
		0xFF, 0xD8, 0xFF, 0xE0, // JPEG SOI + APP0
		0x00, 0x10, // APP0 length
		0x4A, 0x46, 0x49, 0x46, // JFIF identifier
		0x00, 0x01, 0x00, // JFIF version
		0xFF, 0xD9, // EOI marker
	}

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/notes/pages/image" && r.URL.Query().Get("path") == "/path/to/note.note" && r.URL.Query().Get("page") == "0" && r.Method == "GET" {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(testImageData)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockServer.Close()

	client := newAPIClient(mockServer.URL, "", "", "", "")
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "test-server",
		Version: "1.0.0",
	}, nil)

	registerTools(server, client)

	input := GetNoteImageInput{NotePath: "/path/to/note.note", Page: 0}
	result, _, err := testCallGetNoteImageTool(server, client, input)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result == nil || len(result.Content) == 0 {
		t.Fatal("expected content in result")
	}

	imageContent, ok := result.Content[0].(*mcp.ImageContent)
	if !ok {
		t.Fatalf("expected ImageContent, got %T", result.Content[0])
	}

	// Verify MIME type
	if imageContent.MIMEType != "image/jpeg" {
		t.Errorf("expected MIME type image/jpeg, got %s", imageContent.MIMEType)
	}

	// Verify data is base64-encoded (as []byte)
	if len(imageContent.Data) == 0 {
		t.Fatal("expected base64-encoded image data")
	}
}

// TestGetNoteImageNotFound verifies error when page image doesn't exist.
func TestGetNoteImageNotFound(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockServer.Close()

	client := newAPIClient(mockServer.URL, "", "", "", "")
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "test-server",
		Version: "1.0.0",
	}, nil)

	registerTools(server, client)

	input := GetNoteImageInput{NotePath: "/nonexistent/note.note", Page: 0}
	_, _, err := testCallGetNoteImageTool(server, client, input)

	if err == nil {
		t.Fatal("expected error for nonexistent image")
	}
	if !strings.Contains(err.Error(), "page image not found") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestGetNoteImageEmptyPath verifies error when path is missing.
func TestGetNoteImageEmptyPath(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockServer.Close()

	client := newAPIClient(mockServer.URL, "", "", "", "")
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "test-server",
		Version: "1.0.0",
	}, nil)

	registerTools(server, client)

	input := GetNoteImageInput{NotePath: "", Page: 0}
	_, _, err := testCallGetNoteImageTool(server, client, input)

	if err == nil {
		t.Fatal("expected error for empty path")
	}
	if !strings.Contains(err.Error(), "note_path is required") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestAPIClientBasicAuth verifies Basic Auth is set when credentials provided.
func TestAPIClientBasicAuth(t *testing.T) {
	var capturedAuth string
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]interface{}{})
	}))
	defer mockServer.Close()

	client := newAPIClient(mockServer.URL, "", "", "testuser", "testpass")
	ctx := context.Background()
	resp, err := client.get(ctx, "/api/search?q=test")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if capturedAuth == "" {
		t.Fatal("expected Authorization header")
	}

	if !strings.Contains(capturedAuth, "Basic") {
		t.Errorf("expected Basic auth, got: %s", capturedAuth)
	}
}

// TestAPIClientBearerToken verifies a Bearer token is sent (and takes
// precedence over Basic Auth) when a token is provided.
func TestAPIClientBearerToken(t *testing.T) {
	var capturedAuth string
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]interface{}{})
	}))
	defer mockServer.Close()

	// Token set alongside user/pass: token must win.
	client := newAPIClient(mockServer.URL, "", "tok123", "testuser", "testpass")
	ctx := context.Background()
	resp, err := client.get(ctx, "/api/search?q=test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if capturedAuth != "Bearer tok123" {
		t.Errorf("expected \"Bearer tok123\", got: %s", capturedAuth)
	}
}

// TestAPIClientNoAuth verifies no auth header when credentials not provided.
func TestAPIClientNoAuth(t *testing.T) {
	var capturedAuth string
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]interface{}{})
	}))
	defer mockServer.Close()

	client := newAPIClient(mockServer.URL, "", "", "", "")
	ctx := context.Background()
	resp, err := client.get(ctx, "/api/search?q=test")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if capturedAuth != "" {
		t.Errorf("expected no Authorization header, got: %s", capturedAuth)
	}
}

// TestSearchNotesNoResults handles empty result set gracefully.
func TestSearchNotesNoResults(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/search" && r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]map[string]interface{}{})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockServer.Close()

	client := newAPIClient(mockServer.URL, "", "", "", "")
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "test-server",
		Version: "1.0.0",
	}, nil)

	registerTools(server, client)

	input := SearchNotesInput{Query: "nonexistent"}
	result, _, err := testCallSearchNotesTool(server, client, input)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	textContent, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}

	text := textContent.Text
	if !strings.Contains(text, "No results found") {
		t.Errorf("expected 'No results found' message, got: %s", text)
	}
}

// Helper functions for testing tool handlers via actual MCP client-server communication

// testCallSearchNotesTool calls the search_notes tool via an in-process MCP client-server pair.
func testCallSearchNotesTool(server *mcp.Server, client *apiClient, input SearchNotesInput) (*mcp.CallToolResult, any, error) {
	ctx := context.Background()

	// Create an in-process client-server connection
	clientTransport, serverTransport := mcp.NewInMemoryTransports()

	// Connect server to its transport
	go func() {
		server.Run(ctx, serverTransport)
	}()

	// Create client and connect to transport
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	clientSession, err := mcpClient.Connect(ctx, clientTransport, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect client: %w", err)
	}

	// Serialize input to map
	inputBytes, err := json.Marshal(input)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal input: %w", err)
	}
	var inputMap map[string]any
	if err := json.Unmarshal(inputBytes, &inputMap); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal input: %w", err)
	}

	// Call the tool via the MCP client
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "search_notes",
		Arguments: inputMap,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("CallTool failed: %w", err)
	}

	// Check if the MCP result indicates an error from the tool handler
	if result.IsError {
		if len(result.Content) > 0 {
			if tc, ok := result.Content[0].(*mcp.TextContent); ok {
				return nil, nil, fmt.Errorf("%s", tc.Text)
			}
		}
		return nil, nil, fmt.Errorf("tool returned error")
	}

	return result, nil, nil
}

// testCallGetNotePagesTool calls the get_note_pages tool via an in-process MCP client-server pair.
func testCallGetNotePagesTool(server *mcp.Server, client *apiClient, input GetNotePagesInput) (*mcp.CallToolResult, any, error) {
	ctx := context.Background()

	// Create an in-process client-server connection
	clientTransport, serverTransport := mcp.NewInMemoryTransports()

	// Connect server to its transport
	go func() {
		server.Run(ctx, serverTransport)
	}()

	// Create client and connect to transport
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	clientSession, err := mcpClient.Connect(ctx, clientTransport, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect client: %w", err)
	}

	// Serialize input to map
	inputBytes, err := json.Marshal(input)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal input: %w", err)
	}
	var inputMap map[string]any
	if err := json.Unmarshal(inputBytes, &inputMap); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal input: %w", err)
	}

	// Call the tool via the MCP client
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_note_pages",
		Arguments: inputMap,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("CallTool failed: %w", err)
	}

	// Check if the MCP result indicates an error from the tool handler
	if result.IsError {
		if len(result.Content) > 0 {
			if tc, ok := result.Content[0].(*mcp.TextContent); ok {
				return nil, nil, fmt.Errorf("%s", tc.Text)
			}
		}
		return nil, nil, fmt.Errorf("tool returned error")
	}

	return result, nil, nil
}

// testCallGetNoteImageTool calls the get_note_image tool via an in-process MCP client-server pair.
func testCallGetNoteImageTool(server *mcp.Server, client *apiClient, input GetNoteImageInput) (*mcp.CallToolResult, any, error) {
	ctx := context.Background()

	// Create an in-process client-server connection
	clientTransport, serverTransport := mcp.NewInMemoryTransports()

	// Connect server to its transport
	go func() {
		server.Run(ctx, serverTransport)
	}()

	// Create client and connect to transport
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	clientSession, err := mcpClient.Connect(ctx, clientTransport, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect client: %w", err)
	}

	// Serialize input to map
	inputBytes, err := json.Marshal(input)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal input: %w", err)
	}
	var inputMap map[string]any
	if err := json.Unmarshal(inputBytes, &inputMap); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal input: %w", err)
	}

	// Call the tool via the MCP client
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_note_image",
		Arguments: inputMap,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("CallTool failed: %w", err)
	}

	// Check if the MCP result indicates an error from the tool handler
	if result.IsError {
		if len(result.Content) > 0 {
			if tc, ok := result.Content[0].(*mcp.TextContent); ok {
				return nil, nil, fmt.Errorf("%s", tc.Text)
			}
		}
		return nil, nil, fmt.Errorf("tool returned error")
	}

	return result, nil, nil
}
