package processor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRecognizeOpenAI_DisablesQwenThinking confirms the OpenAI request body
// carries chat_template_kwargs.enable_thinking=false so Qwen3-class models
// suppress their reasoning preamble. Non-Qwen endpoints tolerate the unknown
// key per OpenAI Chat Completions's permissive-extra-fields convention.
func TestRecognizeOpenAI_DisablesQwenThinking(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &capturedBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"transcribed"}}]}`))
	}))
	defer srv.Close()

	c := NewOCRClient(srv.URL, "test-key", "qwen2.5-vl-7b", OCRFormatOpenAI)
	text, err := c.Recognize(context.Background(), []byte("fake-jpeg-bytes"), "Transcribe.")
	if err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	if text != "transcribed" {
		t.Errorf("text: got %q, want %q", text, "transcribed")
	}

	kwargs, ok := capturedBody["chat_template_kwargs"].(map[string]any)
	if !ok {
		t.Fatalf("chat_template_kwargs missing or wrong type; body keys: %v", keysOf(capturedBody))
	}
	if think, ok := kwargs["enable_thinking"].(bool); !ok || think {
		t.Errorf("enable_thinking: got %v (type %T), want false (bool)", kwargs["enable_thinking"], kwargs["enable_thinking"])
	}
}

// TestRecognizeAnthropic_NoKwargs is a regression guard: the Anthropic
// Messages API has no chat-template concept, and shipping a kwargs key on
// that path would be a serializer leak. Confirm the request body is clean.
func TestRecognizeAnthropic_NoKwargs(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &capturedBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"transcribed"}]}`))
	}))
	defer srv.Close()

	c := NewOCRClient(srv.URL, "test-key", "claude-sonnet-4-6", OCRFormatAnthropic)
	if _, err := c.Recognize(context.Background(), []byte("jpg"), "go"); err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	if _, present := capturedBody["chat_template_kwargs"]; present {
		t.Errorf("Anthropic request must not carry chat_template_kwargs; body: %+v", capturedBody)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
