package processor

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIsTransient_ThroughWrapping checks the classification survives the
// caller's own %w wrapping — the Boox worker returns "ocr page 3: %w", so a
// bare type assertion at the call site would miss every real failure.
func TestIsTransient_ThroughWrapping(t *testing.T) {
	base := Transient(errors.New("connection refused"))
	wrapped := fmt.Errorf("ocr page 3: %w", base)
	if !IsTransient(wrapped) {
		t.Error("IsTransient lost the marker through %w wrapping")
	}
	if !strings.Contains(wrapped.Error(), "connection refused") {
		t.Errorf("wrapped message lost the cause: %v", wrapped)
	}
	if IsTransient(fmt.Errorf("parse note: entry not found")) {
		t.Error("plain error classified as transient")
	}
	if Transient(nil) != nil {
		t.Error("Transient(nil) should stay nil")
	}
}

func TestTransientHTTPStatus(t *testing.T) {
	for _, tc := range []struct {
		code int
		want bool
	}{
		{http.StatusInternalServerError, true}, // vLLM "model failed to load"
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusTooManyRequests, true}, // explicit backpressure
		{http.StatusRequestTimeout, true},  // server reporting its own timeout
		{http.StatusBadRequest, false},     // a verdict: identical on every retry
		{http.StatusUnauthorized, false},   // bad credentials
		{http.StatusNotFound, false},       // wrong endpoint
		{http.StatusUnprocessableEntity, false},
	} {
		if got := transientHTTPStatus(tc.code); got != tc.want {
			t.Errorf("transientHTTPStatus(%d) = %v, want %v", tc.code, got, tc.want)
		}
	}
}

// TestOCRClient_ClassifiesRealFailures replays the two failures found in
// production on 2026-07-26 (Boox jobs 2391 and 2412) plus a permanent one,
// end to end through Recognize.
func TestOCRClient_ClassifiesRealFailures(t *testing.T) {
	t.Run("backend unreachable (job 2391)", func(t *testing.T) {
		// A listener that is closed immediately: dialing gets refused, the
		// same shape as "dial tcp 192.168.9.199:8000: connect: connection refused".
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()

		c := NewOCRClient(url, "k", "m", OCRFormatOpenAI)
		_, err := c.Recognize(t.Context(), []byte("jpeg"), "prompt")
		if err == nil {
			t.Fatal("expected an error from a closed backend")
		}
		if !IsTransient(err) {
			t.Errorf("unreachable backend not transient: %v", err)
		}
	})

	t.Run("model failed to load, HTTP 500 (job 2412)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"code":500,"message":"model name=qwen3.6-27b-vision failed to load","type":"server_error"}}`)
		}))
		defer srv.Close()

		c := NewOCRClient(srv.URL, "k", "m", OCRFormatOpenAI)
		_, err := c.Recognize(t.Context(), []byte("jpeg"), "prompt")
		if err == nil {
			t.Fatal("expected an error from a 500")
		}
		if !IsTransient(err) {
			t.Errorf("HTTP 500 not transient: %v", err)
		}
		if !strings.Contains(err.Error(), "failed to load") {
			t.Errorf("error dropped the server's message: %v", err)
		}
	})

	t.Run("bad request is permanent", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"unsupported image format"}`)
		}))
		defer srv.Close()

		c := NewOCRClient(srv.URL, "k", "m", OCRFormatOpenAI)
		_, err := c.Recognize(t.Context(), []byte("jpeg"), "prompt")
		if err == nil {
			t.Fatal("expected an error from a 400")
		}
		if IsTransient(err) {
			t.Errorf("HTTP 400 classified as transient; it would retry forever: %v", err)
		}
	})
}
