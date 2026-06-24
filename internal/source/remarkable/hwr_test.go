package remarkable

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHWRClientSignsAndReturnsJIIX(t *testing.T) {
	const body = `{"configuration":{"lang":"en_US"},"content":"ink"}`
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != hwrAPIPath {
			t.Fatalf("path = %q, want %q", r.URL.Path, hwrAPIPath)
		}
		if r.Header.Get("Accept") != hwrJIIX {
			t.Fatalf("Accept = %q, want %q", r.Header.Get("Accept"), hwrJIIX)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("applicationKey") != "app-key" {
			t.Fatalf("applicationKey = %q", r.Header.Get("applicationKey"))
		}
		data, _ := io.ReadAll(r.Body)
		gotBody = string(data)
		mac := hmac.New(sha512.New, []byte("app-key"+"hmac-secret"))
		_, _ = mac.Write([]byte(body))
		if got := r.Header.Get("hmac"); got != hex.EncodeToString(mac.Sum(nil)) {
			t.Fatalf("hmac = %q, want signed body", got)
		}
		w.Header().Set("Content-Type", hwrJIIX)
		_, _ = w.Write([]byte(`{"label":"hello"}`))
	}))
	defer srv.Close()

	client := newHWRClient(Config{
		HWRApplicationKey: "app-key",
		HWRHMAC:           "hmac-secret",
		HWRHost:           srv.URL,
	})
	resp, err := client.Recognize(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	if gotBody != body {
		t.Fatalf("body = %s, want %s", gotBody, body)
	}
	if string(resp) != `{"label":"hello"}` {
		t.Fatalf("resp = %s", resp)
	}
}

func TestHWRClientLanguageOverrideSignsModifiedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(data), `"lang":"de_DE"`) {
			t.Fatalf("body = %s, want overridden lang", data)
		}
		mac := hmac.New(sha512.New, []byte("app-key"+"hmac-secret"))
		_, _ = mac.Write(data)
		if got := r.Header.Get("hmac"); got != hex.EncodeToString(mac.Sum(nil)) {
			t.Fatalf("hmac = %q, want modified body signature", got)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client := newHWRClient(Config{
		HWRApplicationKey: "app-key",
		HWRHMAC:           "hmac-secret",
		HWRLangOverride:   "de_DE",
		HWRHost:           srv.URL,
	})
	if _, err := client.Recognize(context.Background(), []byte(`{"configuration":{"lang":"en_US"}}`)); err != nil {
		t.Fatalf("Recognize: %v", err)
	}
}

func TestHWRClientRequiresCredentials(t *testing.T) {
	_, err := newHWRClient(Config{}).Recognize(context.Background(), []byte(`{}`))
	if err == nil {
		t.Fatal("Recognize succeeded without credentials, want error")
	}
}
