package processor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// OCRFormatAnthropic uses the Anthropic Messages API (/v1/messages).
	// Compatible with direct Anthropic API and OpenRouter.
	OCRFormatAnthropic = "anthropic"

	// OCRFormatOpenAI uses the OpenAI Chat Completions API (/v1/chat/completions).
	// Compatible with vLLM, Ollama, and any OpenAI-compatible endpoint.
	OCRFormatOpenAI = "openai"
)

// DefaultOCRPrompt is used when no custom prompt is configured.
const DefaultOCRPrompt = "Transcribe all handwritten text from this page exactly as written. Return only the text, no commentary."

// OCRClient posts JPEG images to a vision API and returns transcribed text.
// Supports both Anthropic Messages API format and OpenAI Chat Completions format.
type OCRClient struct {
	apiURL string
	apiKey string
	model  string
	format string
	client *http.Client
}

// NewOCRClient creates an OCRClient.
// apiURL is the API base (e.g. "https://api.anthropic.com", "https://openrouter.ai/api",
// or "http://localhost:8000" for a local vLLM instance).
// format is OCRFormatAnthropic or OCRFormatOpenAI.
func NewOCRClient(apiURL, apiKey, model, format string) *OCRClient {
	if format != OCRFormatOpenAI {
		format = OCRFormatAnthropic // default
	}
	return &OCRClient{
		apiURL: apiURL,
		apiKey: apiKey,
		model:  model,
		format: format,
		client: &http.Client{Timeout: 5 * time.Minute},
	}
}

// Model returns the model name this client posts to the vision API. Used
// by the Boox pipeline to tag completed jobs with the api_model they ran
// against. (The Supernote pipeline reaches into the unexported field
// directly because it lives in the same package.)
func (c *OCRClient) Model() string { return c.model }

// Recognize sends a JPEG page image to the vision API and returns the transcribed text.
// If prompt is empty, the default prompt is used.
func (c *OCRClient) Recognize(ctx context.Context, jpegData []byte, prompt string) (string, error) {
	if prompt == "" {
		prompt = DefaultOCRPrompt
	}
	if c.format == OCRFormatOpenAI {
		return c.recognizeOpenAI(ctx, jpegData, prompt)
	}
	return c.recognizeAnthropic(ctx, jpegData, prompt)
}

// ── Anthropic Messages API ────────────────────────────────────────────────────

type anthropicRequest struct {
	Model     string         `json:"model"`
	MaxTokens int            `json:"max_tokens"`
	Messages  []anthropicMsg `json:"messages"`
}

type anthropicMsg struct {
	Role    string             `json:"role"`
	Content []anthropicContent `json:"content"`
}

type anthropicContent struct {
	Type   string           `json:"type"`
	Text   string           `json:"text,omitempty"`
	Source *anthropicSource `json:"source,omitempty"`
}

type anthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (c *OCRClient) recognizeAnthropic(ctx context.Context, jpegData []byte, prompt string) (string, error) {
	reqBody := anthropicRequest{
		Model:     c.model,
		MaxTokens: 4096,
		Messages: []anthropicMsg{{
			Role: "user",
			Content: []anthropicContent{
				{Type: "text", Text: prompt},
				{Type: "image", Source: &anthropicSource{
					Type:      "base64",
					MediaType: "image/jpeg",
					Data:      base64.StdEncoding.EncodeToString(jpegData),
				}},
			},
		}},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("ocrclient marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.apiURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ocrclient request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ocrclient post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("ocrclient API error %d: %s", resp.StatusCode, b)
	}

	var vResp anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&vResp); err != nil {
		return "", fmt.Errorf("ocrclient decode: %w", err)
	}
	if len(vResp.Content) == 0 {
		return "", fmt.Errorf("ocrclient: empty response")
	}
	return vResp.Content[0].Text, nil
}

// ── OpenAI Chat Completions API ───────────────────────────────────────────────

type openAIRequest struct {
	Model     string      `json:"model"`
	MaxTokens int         `json:"max_tokens"`
	Messages  []openAIMsg `json:"messages"`
	// ChatTemplateKwargs is a vLLM extension to the OpenAI Chat Completions
	// schema — passed through to the model's chat template at render time.
	// We use it to suppress Qwen3's reasoning/thinking tokens for OCR
	// (`enable_thinking: false`), which would otherwise produce hundreds of
	// `<think>...</think>` tokens before the actual transcription.
	//
	// Strictness caveat: this is a vLLM-only feature. OpenAI's Chat
	// Completions has historically ignored unknown top-level fields, but
	// strictness varies across compatible gateways (OpenRouter, Together,
	// LiteLLM, Groq, Fireworks, Ollama all differ; OpenAI proper has been
	// tightening param validation in newer modes). If you point this at a
	// strict endpoint and OCR starts 400-ing, the cheapest fix is to flip
	// back to OCRFormatAnthropic, or remove this field — a config gate
	// would be the principled fix once we have a second vLLM-only feature
	// to share it.
	//
	// JSON encoding: omitempty elides the field when the map is nil; a
	// non-nil empty map (`map[string]any{}`) still serializes as `{}`.
	// Today's only writer (recognizeOpenAI) always populates one key, so
	// the field always ships when format=openai.
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
}

type openAIMsg struct {
	Role    string          `json:"role"`
	Content []openAIContent `json:"content"`
}

type openAIContent struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *openAIImgURL `json:"image_url,omitempty"`
}

type openAIImgURL struct {
	URL string `json:"url"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func (c *OCRClient) recognizeOpenAI(ctx context.Context, jpegData []byte, prompt string) (string, error) {
	dataURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpegData)

	reqBody := openAIRequest{
		Model:     c.model,
		MaxTokens: 4096,
		Messages: []openAIMsg{{
			Role: "user",
			Content: []openAIContent{
				{Type: "text", Text: prompt},
				{Type: "image_url", ImageURL: &openAIImgURL{URL: dataURL}},
			},
		}},
		// Disable Qwen3's thinking tokens for OCR — the recognized text is
		// the only thing we want back, and the `<think>...</think>` preamble
		// adds latency, eats max_tokens budget, and pollutes the body when
		// it occasionally fails to terminate cleanly. See struct comment for
		// non-Qwen endpoint behavior.
		ChatTemplateKwargs: map[string]any{"enable_thinking": false},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("ocrclient marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.apiURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ocrclient request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ocrclient post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("ocrclient API error %d: %s", resp.StatusCode, b)
	}

	var vResp openAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&vResp); err != nil {
		return "", fmt.Errorf("ocrclient decode: %w", err)
	}
	if len(vResp.Choices) == 0 {
		return "", fmt.Errorf("ocrclient: empty response")
	}
	return vResp.Choices[0].Message.Content, nil
}
