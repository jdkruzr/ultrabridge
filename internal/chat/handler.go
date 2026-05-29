// FCIS: Imperative Shell
package chat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/sysop/ultrabridge/internal/rag"
)

// Handler serves the chat SSE endpoint.
type Handler struct {
	store     *Store
	retriever rag.SearchRetriever
	apiURL    string
	model     string
	logger    *slog.Logger
}

func NewHandler(store *Store, retriever rag.SearchRetriever, apiURL, model string, logger *slog.Logger) *Handler {
	return &Handler{
		store:     store,
		retriever: retriever,
		apiURL:    apiURL,
		model:     model,
		logger:    logger,
	}
}

// HandleAsk handles POST /chat/ask. Orchestrates retrieval → prompt → vLLM → SSE proxy.
func (h *Handler) HandleAsk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID int64  `json:"session_id"`
		Question  string `json:"question"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}
	if req.Question == "" {
		http.Error(w, `{"error":"question is required"}`, http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// Create session if needed
	if req.SessionID == 0 {
		sess, err := h.store.CreateSession(ctx, truncateTitle(req.Question, 50))
		if err != nil {
			h.logger.Error("create session", "err", err)
			http.Error(w, `{"error":"failed to create session"}`, http.StatusInternalServerError)
			return
		}
		req.SessionID = sess.ID
	}

	// Save user message
	if _, err := h.store.AddMessage(ctx, req.SessionID, "user", req.Question); err != nil {
		h.logger.Error("save user message", "err", err)
	}

	// Retrieve relevant context
	results, err := h.retriever.Search(ctx, rag.SearchRequest{
		Query: req.Question,
		Limit: 5,
	})
	if err != nil {
		h.logger.Error("retrieval failed", "err", err)
		// Continue without context rather than failing
		results = nil
	}

	// Assemble prompt
	messages := h.buildPrompt(ctx, req.SessionID, req.Question, results)

	// Stream from vLLM to client
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Send session_id and retrieved context as first event so client knows which session to display
	// and can use search results for citation linkification
	sessionEvent := map[string]interface{}{
		"type":       "session",
		"session_id": req.SessionID,
		"context":    results,
	}
	fmt.Fprintf(w, "data: %s\n\n", mustJSON(sessionEvent))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	fullResponse, err := h.streamFromVLLM(ctx, w, messages)
	if err != nil {
		h.logger.Error("vllm stream failed", "err", err)
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]string{
			"type":  "error",
			"error": "Failed to generate response. Is the chat model running?",
		}))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}

	// Save assistant response
	if _, err := h.store.AddMessage(ctx, req.SessionID, "assistant", fullResponse); err != nil {
		h.logger.Error("save assistant message", "err", err)
	}

	// Send done event
	fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]string{"type": "done"}))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (h *Handler) buildPrompt(ctx context.Context, sessionID int64, question string, results []rag.SearchResult) []map[string]string {
	var messages []map[string]string

	// System prompt with citation instructions
	systemPrompt := `You are a helpful assistant that answers questions about handwritten notes. Use the provided note excerpts to answer the question. Always cite your sources using the format [filename, p.N] where filename is the note file name and N is the page number.

If the provided notes don't contain enough information to answer the question, say so clearly.`

	// Add retrieved context (skip pages with trivially short text)
	if len(results) > 0 {
		var contextBuilder strings.Builder
		contextBuilder.WriteString("\n\n--- Retrieved Notes ---\n")
		for _, r := range results {
			if len(strings.TrimSpace(r.BodyText)) < 10 {
				continue
			}
			filename := filepath.Base(r.NotePath)
			contextBuilder.WriteString(fmt.Sprintf("\n[%s, p.%d]", filename, r.Page))
			if r.Device != "" {
				contextBuilder.WriteString(fmt.Sprintf(" (Device: %s", r.Device))
				if r.Folder != "" {
					contextBuilder.WriteString(fmt.Sprintf(", Folder: %s", r.Folder))
				}
				contextBuilder.WriteString(")")
			}
			contextBuilder.WriteString(":\n")
			contextBuilder.WriteString(r.BodyText)
			contextBuilder.WriteString("\n")
		}
		systemPrompt += contextBuilder.String()
	}

	messages = append(messages, map[string]string{"role": "system", "content": systemPrompt})

	// Add conversation history (last 10 messages for context window management)
	history, _ := h.store.GetMessages(ctx, sessionID)
	// Skip the just-added user message (last one) and limit history
	if len(history) > 1 {
		start := 0
		if len(history) > 11 {
			start = len(history) - 11
		}
		for _, m := range history[start : len(history)-1] {
			messages = append(messages, map[string]string{"role": m.Role, "content": m.Content})
		}
	}

	// Add current question
	messages = append(messages, map[string]string{"role": "user", "content": question})

	return messages
}

func (h *Handler) streamFromVLLM(ctx context.Context, w http.ResponseWriter, messages []map[string]string) (string, error) {
	reqBody := map[string]interface{}{
		"model":       h.model,
		"messages":    messages,
		"stream":      true,
		"temperature": 0.7,
		"chat_template_kwargs": map[string]interface{}{
			"enable_thinking": false,
		},
		"max_tokens": 2048,
	}
	body, _ := json.Marshal(reqBody)

	client := &http.Client{Timeout: 120 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "POST", h.apiURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("vllm connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("vllm returned %d: %s", resp.StatusCode, string(respBody))
	}

	flusher, _ := w.(http.Flusher)
	var fullResponse strings.Builder

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()

		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if line == "data: [DONE]" {
			break
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		jsonStr := strings.TrimPrefix(line, "data: ")
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(jsonStr), &chunk); err != nil {
			continue
		}

		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			content := chunk.Choices[0].Delta.Content
			fullResponse.WriteString(content)

			// Forward as SSE to browser
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]string{
				"type":    "content",
				"content": content,
			}))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}

	return fullResponse.String(), scanner.Err()
}

func truncateTitle(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}
