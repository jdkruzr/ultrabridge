package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/rag"
)

// mockRetriever implements rag.SearchRetriever for testing.
type mockRetriever struct {
	results []rag.SearchResult
	err     error
}

func (m *mockRetriever) Search(ctx context.Context, req rag.SearchRequest) ([]rag.SearchResult, error) {
	return m.results, m.err
}

// TestHandlerHandleAskWithValidQuestion tests basic SSE streaming with a working vLLM.
func TestHandlerHandleAskWithValidQuestion(t *testing.T) {
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	defer db.Close()

	// Create a mock vLLM server that streams SSE
	vllmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"Hello"}}]}`)
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":" world"}}]}`)
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer vllmServer.Close()

	store := NewStore(db)
	retriever := &mockRetriever{results: []rag.SearchResult{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHandler(store, retriever, vllmServer.URL, "test-model", logger)

	// POST /chat/ask with a question
	reqBody := `{"session_id":0,"question":"What is this?"}`
	httpReq := httptest.NewRequest("POST", "/chat/ask", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleAsk(w, httpReq)

	// Verify response is text/event-stream
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// Verify we get SSE events with content
	body := w.Body.String()
	if !strings.Contains(body, `"type":"session"`) {
		t.Errorf("response missing session event")
	}
	if !strings.Contains(body, `"type":"content"`) {
		t.Errorf("response missing content event")
	}
	if !strings.Contains(body, `"type":"done"`) {
		t.Errorf("response missing done event")
	}
}

// TestHandlerHandleAskCreatesSession tests that a new session is created.
func TestHandlerHandleAskCreatesSession(t *testing.T) {
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	defer db.Close()

	vllmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"OK"}}]}`)
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer vllmServer.Close()

	store := NewStore(db)
	retriever := &mockRetriever{results: []rag.SearchResult{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHandler(store, retriever, vllmServer.URL, "test-model", logger)

	reqBody := `{"session_id":0,"question":"Test question"}`
	httpReq := httptest.NewRequest("POST", "/chat/ask", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleAsk(w, httpReq)

	// Verify session was created in DB
	sessions, err := store.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("len(sessions) = %d, want 1", len(sessions))
	}
}

// TestHandlerHandleAskSavesMessages tests that user and assistant messages are saved.
func TestHandlerHandleAskSavesMessages(t *testing.T) {
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	defer db.Close()

	vllmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"Response"}}]}`)
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer vllmServer.Close()

	store := NewStore(db)
	retriever := &mockRetriever{results: []rag.SearchResult{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHandler(store, retriever, vllmServer.URL, "test-model", logger)

	reqBody := `{"session_id":0,"question":"What is AI?"}`
	httpReq := httptest.NewRequest("POST", "/chat/ask", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleAsk(w, httpReq)

	// Get the session ID from the SSE response
	body := w.Body.String()
	var sessionEvent struct {
		Type      string `json:"type"`
		SessionID int64  `json:"session_id"`
	}
	// Parse the session event
	lines := strings.Split(body, "\n\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if err := json.Unmarshal([]byte(data), &sessionEvent); err == nil && sessionEvent.Type == "session" {
				break
			}
		}
	}

	if sessionEvent.SessionID == 0 {
		t.Fatalf("failed to extract session_id from response")
	}

	// Verify messages were saved
	messages, err := store.GetMessages(context.Background(), sessionEvent.SessionID)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(messages) != 2 {
		t.Errorf("len(messages) = %d, want 2", len(messages))
	}
	if len(messages) >= 1 && messages[0].Role != "user" {
		t.Errorf("messages[0].Role = %q, want user", messages[0].Role)
	}
	if len(messages) >= 2 && messages[1].Role != "assistant" {
		t.Errorf("messages[1].Role = %q, want assistant", messages[1].Role)
	}
}

// TestHandlerBuildPromptIncludesCitations tests that system prompt includes citation instructions.
func TestHandlerBuildPromptIncludesCitations(t *testing.T) {
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	defer db.Close()

	store := NewStore(db)
	retriever := &mockRetriever{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHandler(store, retriever, "http://localhost:8000", "test-model", logger)

	// Create a session
	sess, err := store.CreateSession(context.Background(), "Test")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Build prompt with search results
	results := []rag.SearchResult{
		{
			NotePath: "/notes/file1.note",
			Page:     1,
			BodyText: "Sample text from note",
			Device:   "Supernote",
			Folder:   "folder1",
		},
	}

	messages := handler.buildPrompt(context.Background(), sess.ID, "Test question", results)

	// Verify system prompt includes citation instructions
	if len(messages) == 0 {
		t.Fatalf("buildPrompt returned empty messages")
	}
	systemPrompt := messages[0]["content"]
	if !strings.Contains(systemPrompt, "[filename, p.N]") {
		t.Errorf("system prompt missing citation format instruction")
	}
	if !strings.Contains(systemPrompt, "file1.note") {
		t.Errorf("system prompt missing retrieved note filename")
	}
}

// TestHandlerHandleAskVLLMUnreachable tests error handling when vLLM is unreachable.
func TestHandlerHandleAskVLLMUnreachable(t *testing.T) {
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	defer db.Close()

	store := NewStore(db)
	retriever := &mockRetriever{results: []rag.SearchResult{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Point to non-existent server
	handler := NewHandler(store, retriever, "http://localhost:55555", "test-model", logger)

	reqBody := `{"session_id":0,"question":"What is AI?"}`
	httpReq := httptest.NewRequest("POST", "/chat/ask", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleAsk(w, httpReq)

	// Verify we get an error event, not a crash
	body := w.Body.String()
	if !strings.Contains(body, `"type":"error"`) {
		t.Errorf("response missing error event when vLLM unreachable")
	}
}

// TestHandlerHandleAskValidatesQuestion tests that empty questions are rejected.
func TestHandlerHandleAskValidatesQuestion(t *testing.T) {
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	defer db.Close()

	store := NewStore(db)
	retriever := &mockRetriever{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHandler(store, retriever, "http://localhost:8000", "test-model", logger)

	// Request with empty question
	reqBody := `{"session_id":0,"question":""}`
	httpReq := httptest.NewRequest("POST", "/chat/ask", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleAsk(w, httpReq)

	// Verify 400 response
	if w.Code != http.StatusBadRequest {
		t.Errorf("status code = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandlerHandleAskUsesConfiguredModel tests that configured model is used.
func TestHandlerHandleAskUsesConfiguredModel(t *testing.T) {
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	defer db.Close()

	var capturedRequest []byte
	vllmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedRequest, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"OK"}}]}`)
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer vllmServer.Close()

	store := NewStore(db)
	retriever := &mockRetriever{results: []rag.SearchResult{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	configuredModel := "custom-model-v2"
	handler := NewHandler(store, retriever, vllmServer.URL, configuredModel, logger)

	reqBody := `{"session_id":0,"question":"What is this?"}`
	httpReq := httptest.NewRequest("POST", "/chat/ask", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleAsk(w, httpReq)

	// Verify the configured model was sent to vLLM
	var vllmReq struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(capturedRequest, &vllmReq); err != nil {
		t.Fatalf("failed to parse vLLM request: %v", err)
	}
	if vllmReq.Model != configuredModel {
		t.Errorf("model sent to vLLM = %q, want %q", vllmReq.Model, configuredModel)
	}
}
