package rag

// FCIS: Imperative Shell

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// embedTimeout bounds a single embed request. The embedding model on this
// deployment runs CPU-only (~9.4s/1000 chars), so it's generous on purpose —
// latency is not a concern for backfill/background embedding, and chunking
// keeps each request small. See memory project_ollama_embedding_cpu_and_chunking.
const embedTimeout = 120 * time.Second

// Embedder generates embedding vectors from text.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// EmbedStore persists per-chunk embeddings for pages. Implemented by *Store.
type EmbedStore interface {
	Save(ctx context.Context, notePath string, page, chunk int, embedding []float32, model string) error
	DeletePage(ctx context.Context, notePath string, page int) error
}

// EmbedAndStorePage chunks text, embeds each chunk, and replaces the page's
// stored embeddings. Best-effort: failed chunks are logged and skipped. Prior
// embeddings are preserved if every chunk fails (e.g. Ollama down), so a
// transient outage during re-index doesn't lose data; they're replaced only
// once at least one chunk embeds. Empty text drops the page's embeddings.
// Returns the number of chunks stored. Latency is intentionally unbounded here
// (the embed model is slow/CPU-bound) — callers run this off hot paths.
func EmbedAndStorePage(ctx context.Context, e Embedder, s EmbedStore, notePath string, page int, text, model string, logger *slog.Logger) int {
	if logger == nil {
		logger = slog.Default()
	}
	chunks := ChunkText(text)
	if len(chunks) == 0 {
		// Page emptied → drop any prior embeddings.
		if err := s.DeletePage(ctx, notePath, page); err != nil {
			logger.Warn("embed: clear emptied page failed", "path", notePath, "page", page, "err", err)
		}
		return 0
	}

	type chunkVec struct {
		idx int
		vec []float32
	}
	var ok []chunkVec
	for i, c := range chunks {
		vec, err := e.Embed(ctx, c)
		if err != nil {
			logger.Warn("embed: chunk failed", "path", notePath, "page", page, "chunk", i, "err", err)
			continue
		}
		ok = append(ok, chunkVec{i, vec})
	}
	if len(ok) == 0 {
		// Keep whatever was there — don't blow away good vectors on a full failure.
		logger.Warn("embed: all chunks failed; keeping prior embeddings", "path", notePath, "page", page, "chunks", len(chunks))
		return 0
	}

	if err := s.DeletePage(ctx, notePath, page); err != nil {
		logger.Warn("embed: clear page before replace failed", "path", notePath, "page", page, "err", err)
	}
	stored := 0
	for _, cv := range ok {
		if err := s.Save(ctx, notePath, page, cv.idx, cv.vec, model); err != nil {
			logger.Warn("embed: save chunk failed", "path", notePath, "page", page, "chunk", cv.idx, "err", err)
			continue
		}
		stored++
	}
	return stored
}

// OllamaEmbedder calls Ollama's /api/embed endpoint.
type OllamaEmbedder struct {
	baseURL string
	model   string
	client  *http.Client
	logger  *slog.Logger
}

func NewOllamaEmbedder(baseURL, model string, logger *slog.Logger) *OllamaEmbedder {
	return &OllamaEmbedder{
		baseURL: baseURL,
		model:   model,
		client:  &http.Client{Timeout: embedTimeout},
		logger:  logger,
	}
}

// Model returns the model name (used for storing in note_embeddings.model column).
func (e *OllamaEmbedder) Model() string { return e.model }

type embedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embedResponse struct {
	Embeddings [][]float64 `json:"embeddings"`
}

func (e *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(embedRequest{Model: e.model, Input: text})
	if err != nil {
		return nil, fmt.Errorf("marshal embed request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", e.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Include Ollama's error body — e.g. "the input length exceeds the
		// context length" — so failures are diagnosable from the log.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("ollama returned %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	var result embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if len(result.Embeddings) == 0 {
		return nil, fmt.Errorf("empty embeddings response")
	}

	// Convert float64 (JSON) to float32 (storage)
	f64 := result.Embeddings[0]
	f32 := make([]float32, len(f64))
	for i, v := range f64 {
		f32[i] = float32(v)
	}
	return f32, nil
}
