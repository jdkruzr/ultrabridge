package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// callTaskTool is a compact in-process client-server invocation helper,
// modeled on the per-tool helpers in tools_test.go but parameterized by
// tool name. Returns the tool result or the surfaced error.
func callTaskTool(t *testing.T, server *mcp.Server, toolName string, input interface{}) (*mcp.CallToolResult, error) {
	t.Helper()
	ctx := context.Background()

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	go func() { server.Run(ctx, serverTransport) }()

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := mcpClient.Connect(ctx, clientTransport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}

	raw, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal input: %w", err)
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("unmarshal input: %w", err)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: toolName, Arguments: args})
	if err != nil {
		return nil, err
	}
	if result.IsError {
		if len(result.Content) > 0 {
			if tc, ok := result.Content[0].(*mcp.TextContent); ok {
				return nil, fmt.Errorf("%s", tc.Text)
			}
		}
		return nil, fmt.Errorf("tool returned error")
	}
	return result, nil
}

// Builds a server with the task tools registered against a given API base.
func newTaskTestServer(baseURL string) *mcp.Server {
	client := newAPIClient(baseURL, "", "", "", "")
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	registerTools(server, client)
	return server
}

// resultText extracts the first TextContent body from a tool result, failing
// the test if that shape isn't present.
func resultText(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	if r == nil || len(r.Content) == 0 {
		t.Fatal("expected content in result")
	}
	tc, ok := r.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", r.Content[0])
	}
	return tc.Text
}

// --- list_tasks ---

func TestListTasks_WithFilters(t *testing.T) {
	var capturedQuery string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/tasks" && r.Method == http.MethodGet {
			capturedQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{"id": "t1", "title": "Ship it", "status": "needsAction"},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	server := newTaskTestServer(mock.URL)
	result, err := callTaskTool(t, server, "list_tasks", ListTasksInput{
		Status:    "needs_action",
		DueBefore: "2026-05-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Ship it") || !strings.Contains(text, "t1") {
		t.Errorf("task not rendered: %s", text)
	}
	if !strings.Contains(capturedQuery, "status=needs_action") {
		t.Errorf("status filter missing from query: %s", capturedQuery)
	}
	if !strings.Contains(capturedQuery, "due_before=2026-05-01T00%3A00%3A00Z") {
		t.Errorf("due_before missing from query: %s", capturedQuery)
	}
}

func TestListTasks_EmptyResult(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]interface{}{})
	}))
	defer mock.Close()

	server := newTaskTestServer(mock.URL)
	result, err := callTaskTool(t, server, "list_tasks", ListTasksInput{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(resultText(t, result), "No tasks") {
		t.Errorf("expected empty-state message; got %s", resultText(t, result))
	}
}

// --- get_task ---

func TestGetTask_Success(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/tasks/t1" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id": "t1", "title": "Review deck", "status": "needsAction",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	server := newTaskTestServer(mock.URL)
	result, err := callTaskTool(t, server, "get_task", GetTaskInput{ID: "t1"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(resultText(t, result), "Review deck") {
		t.Errorf("title missing: %s", resultText(t, result))
	}
}

func TestGetTask_NotFound(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	server := newTaskTestServer(mock.URL)
	_, err := callTaskTool(t, server, "get_task", GetTaskInput{ID: "ghost"})
	if err == nil {
		t.Fatal("expected error for missing id")
	}
	if !strings.Contains(err.Error(), "task not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGetTask_EmptyID(t *testing.T) {
	server := newTaskTestServer("http://unused")
	_, err := callTaskTool(t, server, "get_task", GetTaskInput{})
	if err == nil {
		t.Fatal("expected error for empty id")
	}
	if !strings.Contains(err.Error(), "id is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- create_task ---

func TestCreateTask_Success(t *testing.T) {
	var capturedBody map[string]interface{}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/tasks" && r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &capturedBody)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id": "new-id", "title": "Buy milk", "status": "needsAction",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	server := newTaskTestServer(mock.URL)
	result, err := callTaskTool(t, server, "create_task", CreateTaskInput{
		Title: "Buy milk",
		DueAt: "2026-04-18T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(resultText(t, result), "Buy milk") {
		t.Errorf("created task not rendered: %s", resultText(t, result))
	}
	if capturedBody["title"] != "Buy milk" {
		t.Errorf("title not sent: %+v", capturedBody)
	}
	if capturedBody["due_at"] == nil {
		t.Errorf("due_at not sent: %+v", capturedBody)
	}
}

func TestCreateTask_MissingTitle(t *testing.T) {
	server := newTaskTestServer("http://unused")
	_, err := callTaskTool(t, server, "create_task", CreateTaskInput{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "title is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCreateTask_BadDueAt(t *testing.T) {
	server := newTaskTestServer("http://unused")
	_, err := callTaskTool(t, server, "create_task", CreateTaskInput{
		Title: "x",
		DueAt: "not-a-date",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "RFC3339") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- update_task ---

func TestUpdateTask_PartialPatch(t *testing.T) {
	var capturedBody map[string]interface{}
	var capturedMethod string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		if r.URL.Path == "/api/v1/tasks/t1" && r.Method == http.MethodPatch {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &capturedBody)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id": "t1", "title": "Reviewed", "status": "needsAction",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	server := newTaskTestServer(mock.URL)
	newTitle := "Reviewed"
	_, err := callTaskTool(t, server, "update_task", UpdateTaskInput{
		ID:    "t1",
		Title: &newTitle,
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if capturedMethod != http.MethodPatch {
		t.Errorf("expected PATCH, got %s", capturedMethod)
	}
	if capturedBody["title"] != "Reviewed" {
		t.Errorf("title not sent: %+v", capturedBody)
	}
	if _, ok := capturedBody["clear_due_at"]; ok {
		t.Errorf("clear_due_at shouldn't be in body when not set: %+v", capturedBody)
	}
}

func TestUpdateTask_ClearDueAt(t *testing.T) {
	var capturedBody map[string]interface{}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/tasks/t1" && r.Method == http.MethodPatch {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &capturedBody)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id": "t1", "title": "x", "status": "needsAction",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	server := newTaskTestServer(mock.URL)
	_, err := callTaskTool(t, server, "update_task", UpdateTaskInput{
		ID:         "t1",
		ClearDueAt: true,
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if capturedBody["clear_due_at"] != true {
		t.Errorf("clear_due_at not sent: %+v", capturedBody)
	}
}

func TestUpdateTask_NoFields(t *testing.T) {
	server := newTaskTestServer("http://unused")
	_, err := callTaskTool(t, server, "update_task", UpdateTaskInput{ID: "t1"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "no fields to update") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- complete_task ---

func TestCompleteTask_Success(t *testing.T) {
	var capturedPath string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	server := newTaskTestServer(mock.URL)
	result, err := callTaskTool(t, server, "complete_task", CompleteTaskInput{ID: "t1"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(resultText(t, result), "completed") {
		t.Errorf("unexpected ack: %s", resultText(t, result))
	}
	if capturedPath != "/api/v1/tasks/t1/complete" {
		t.Errorf("wrong URL: %s", capturedPath)
	}
}

func TestCompleteTask_NotFound(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	server := newTaskTestServer(mock.URL)
	_, err := callTaskTool(t, server, "complete_task", CompleteTaskInput{ID: "ghost"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "task not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- delete_task ---

func TestDeleteTask_Success(t *testing.T) {
	var capturedMethod string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	server := newTaskTestServer(mock.URL)
	result, err := callTaskTool(t, server, "delete_task", DeleteTaskInput{ID: "t1"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if capturedMethod != http.MethodDelete {
		t.Errorf("expected DELETE, got %s", capturedMethod)
	}
	if !strings.Contains(resultText(t, result), "deleted") {
		t.Errorf("unexpected ack: %s", resultText(t, result))
	}
}

// --- purge_completed_tasks ---

func TestPurgeCompleted_Success(t *testing.T) {
	var capturedPath string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	server := newTaskTestServer(mock.URL)
	result, err := callTaskTool(t, server, "purge_completed_tasks", PurgeCompletedTasksInput{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if capturedPath != "/api/v1/tasks/purge-completed" {
		t.Errorf("wrong URL: %s", capturedPath)
	}
	if !strings.Contains(resultText(t, result), "purged") {
		t.Errorf("unexpected ack: %s", resultText(t, result))
	}
}
