package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/sysop/ultrabridge/internal/chat"
	"github.com/sysop/ultrabridge/internal/rag"
	"github.com/sysop/ultrabridge/internal/search"
)

type searchService struct {
	searchIndex search.SearchIndex
	retriever   rag.SearchRetriever
	embedder    rag.Embedder
	embedStore  *rag.Store
	embedModel  string
	chatStore   *chat.Store
	chatAPIURL  string
	chatModel   string
	logger      *slog.Logger
}

func NewSearchService(
	si search.SearchIndex,
	r rag.SearchRetriever,
	embedder rag.Embedder,
	embedStore *rag.Store,
	embedModel string,
	cs *chat.Store,
	apiURL string,
	model string,
	logger *slog.Logger,
) SearchService {
	return &searchService{
		searchIndex: si,
		retriever:   r,
		embedder:    embedder,
		embedStore:  embedStore,
		embedModel:  embedModel,
		chatStore:   cs,
		chatAPIURL:  apiURL,
		chatModel:   model,
		logger:      logger,
	}
}

func (s *searchService) TriggerBackfill(ctx context.Context) error {
	if s.embedder == nil || s.embedStore == nil {
		return fmt.Errorf("embedding pipeline not configured")
	}
	go func() {
		n, err := rag.Backfill(context.Background(), s.embedStore, s.embedder, s.embedModel, s.logger)
		if err != nil {
			s.logger.Error("backfill failed", "error", err)
		} else {
			s.logger.Info("backfill complete", "count", n)
		}
	}()
	return nil
}

func (s *searchService) GetEmbeddingCount(ctx context.Context) int {
	if s.embedStore == nil {
		return 0
	}
	return len(s.embedStore.AllEmbeddings())
}

func (s *searchService) HasEmbeddingPipeline() bool {
	return s.embedder != nil && s.embedStore != nil
}

// searchDefaultLimit is the result cap used when the caller passes 0. Picked
// to match the legacy implicit default the retriever was returning (~20)
// while giving the LLM-side caller a useful ceiling per query.
const searchDefaultLimit = 20

// searchMaxLimit is the hard ceiling — any caller-supplied limit above this
// is clamped down. Keeps a misbehaving client from yanking the entire index
// through a single hybrid query.
const searchMaxLimit = 100

func (s *searchService) Search(ctx context.Context, query, folder string, sources []string, limit int) ([]SearchResult, error) {
	if s.retriever == nil {
		return nil, nil
	}
	// Resolve the caller's limit against the service defaults: 0/negative
	// means "use the default"; anything over the max is clamped down.
	switch {
	case limit <= 0:
		limit = searchDefaultLimit
	case limit > searchMaxLimit:
		limit = searchMaxLimit
	}
	// Hybrid retrieval (FTS5 + vector RRF) with the source-type facet. Going
	// through the retriever (rather than the bare FTS index) means digests and
	// ForestNote pages are searchable here exactly as they are in chat, and each
	// result carries its source type for the UI facet/badge.
	rr, err := s.retriever.Search(ctx, rag.SearchRequest{
		Query:   query,
		Folder:  folder,
		Sources: sources,
		Limit:   limit,
	})
	if err != nil {
		s.logger.Error("search failed", "error", err)
		return nil, err
	}
	results := make([]SearchResult, 0, len(rr))
	for _, r := range rr {
		results = append(results, SearchResult{
			Path:       r.NotePath,
			Page:       r.Page,
			Title:      r.TitleText,
			Snippet:    makeSnippet(r.BodyText, query, 240),
			Score:      float32(r.Score),
			SourceType: r.SourceType,
		})
	}
	return results, nil
}

// makeSnippet builds a short preview from body text, centered on the first
// query-term match when present (the hybrid retriever returns full body text,
// not an FTS snippet). Byte-based; good enough for a preview.
func makeSnippet(body, query string, max int) string {
	body = strings.TrimSpace(body)
	if body == "" || len(body) <= max {
		return body
	}
	lb := strings.ToLower(body)
	idx := -1
	for _, term := range strings.Fields(strings.ToLower(query)) {
		if i := strings.Index(lb, term); i >= 0 {
			idx = i
			break
		}
	}
	if idx < 0 {
		return strings.TrimSpace(body[:max]) + "…"
	}
	start := idx - max/3
	if start < 0 {
		start = 0
	}
	end := start + max
	if end > len(body) {
		end = len(body)
		start = end - max
	}
	snip := strings.TrimSpace(body[start:end])
	if start > 0 {
		snip = "…" + snip
	}
	if end < len(body) {
		snip = snip + "…"
	}
	return snip
}

func (s *searchService) Ask(ctx context.Context, question string, sessionID int) (<-chan ChatResponse, error) {
	if s.chatStore == nil {
		return nil, fmt.Errorf("chat not enabled")
	}

	out := make(chan ChatResponse)

	go func() {
		defer close(out)

		sid := int64(sessionID)
		// 1. Create session if needed
		if sid == 0 {
			sess, err := s.chatStore.CreateSession(ctx, truncateTitle(question, 50))
			if err != nil {
				out <- ChatResponse{Type: "error", Content: "failed to create session"}
				return
			}
			sid = sess.ID
		}

		// 2. Save user message
		if _, err := s.chatStore.AddMessage(ctx, sid, "user", question); err != nil {
			s.logger.Error("save user message", "err", err)
		}

		// 3. Send session info
		out <- ChatResponse{Type: "session", Data: map[string]interface{}{"session_id": sid}}

		// 4. Retrieve context
		var ragResults []rag.SearchResult
		if s.retriever != nil {
			var err error
			ragResults, err = s.retriever.Search(ctx, rag.SearchRequest{
				Query: question,
				Limit: 5,
			})
			if err != nil {
				s.logger.Error("retrieval failed", "err", err)
			} else {
				out <- ChatResponse{Type: "context", Data: ragResults}
			}
		}

		// 5. Build Prompt
		messages := s.buildPrompt(ctx, sid, question, ragResults)

		// 6. Stream from vLLM
		fullResponse, err := s.streamFromVLLM(ctx, out, messages)
		if err != nil {
			s.logger.Error("vllm stream failed", "err", err)
			out <- ChatResponse{Type: "error", Content: "Failed to generate response."}
			return
		}

		// 7. Save assistant response
		if _, err := s.chatStore.AddMessage(ctx, sid, "assistant", fullResponse); err != nil {
			s.logger.Error("save assistant message", "err", err)
		}

		out <- ChatResponse{Type: "done"}
	}()

	return out, nil
}

func (s *searchService) ListSessions(ctx context.Context) (interface{}, error) {
	if s.chatStore == nil {
		return nil, fmt.Errorf("chat not enabled")
	}
	return s.chatStore.ListSessions(ctx)
}

func (s *searchService) GetMessages(ctx context.Context, sessionID int) (interface{}, error) {
	if s.chatStore == nil {
		return nil, fmt.Errorf("chat not enabled")
	}
	return s.chatStore.GetMessages(ctx, int64(sessionID))
}

// Internal helpers (extracted from chat.Handler)

func (s *searchService) buildPrompt(ctx context.Context, sessionID int64, question string, results []rag.SearchResult) []map[string]string {
	var messages []map[string]string

	systemPrompt := `You are a helpful assistant that answers questions about handwritten notes. Use the provided note excerpts to answer the question. Always cite your sources using the format [filename, p.N] where filename is the note file name and N is the page number.

If the provided notes don't contain enough information to answer the question, say so clearly.`

	if len(results) > 0 {
		var contextBuilder strings.Builder
		contextBuilder.WriteString("\n\n--- Retrieved Notes ---\n")
		for _, r := range results {
			if len(strings.TrimSpace(r.BodyText)) < 10 {
				continue
			}
			filename := filepath.Base(r.NotePath)
			contextBuilder.WriteString(fmt.Sprintf("\n[%s, p.%d]", filename, r.Page))
			contextBuilder.WriteString(":\n")
			contextBuilder.WriteString(r.BodyText)
			contextBuilder.WriteString("\n")
		}
		systemPrompt += contextBuilder.String()
	}

	messages = append(messages, map[string]string{"role": "system", "content": systemPrompt})

	history, _ := s.chatStore.GetMessages(ctx, sessionID)
	if len(history) > 1 {
		start := 0
		if len(history) > 11 {
			start = len(history) - 11
		}
		for _, m := range history[start : len(history)-1] {
			messages = append(messages, map[string]string{"role": m.Role, "content": m.Content})
		}
	}

	messages = append(messages, map[string]string{"role": "user", "content": question})
	return messages
}

func (s *searchService) streamFromVLLM(ctx context.Context, out chan<- ChatResponse, messages []map[string]string) (string, error) {
	reqBody := map[string]interface{}{
		"model":       s.chatModel,
		"messages":    messages,
		"stream":      true,
		"temperature": 0.7,
		"max_tokens":  2048,
	}
	body, _ := json.Marshal(reqBody)

	client := &http.Client{Timeout: 120 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "POST", s.chatAPIURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var fullResponse strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(line[6:]), &chunk); err != nil {
			continue
		}

		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			content := chunk.Choices[0].Delta.Content
			fullResponse.WriteString(content)
			out <- ChatResponse{Type: "content", Content: content}
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
