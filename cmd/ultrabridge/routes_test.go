package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegisterWebRoutesLeavesSetupPublic(t *testing.T) {
	mux := http.NewServeMux()
	public := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	protected := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	registerWebRoutes(mux, public, protected)

	for _, path := range []string{"/setup", "/setup/save"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("protected route status = %d, want 401", rec.Code)
	}
}

func TestOAuthBaseURLHonorsForwardedHTTPS(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://internal/.well-known/oauth-authorization-server", nil)
	req.Host = "notes.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	if got := oauthBaseURL(req); got != "https://notes.example.com" {
		t.Fatalf("oauthBaseURL = %q", got)
	}
}
