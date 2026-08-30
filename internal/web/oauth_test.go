package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/mcpauth"
	"github.com/sysop/ultrabridge/internal/notedb"
)

func registerOAuthTestClient(t *testing.T, handler *Handler, redirectURI string) string {
	t.Helper()
	body := `{"client_name":"Claude","redirect_uris":["` + redirectURI + `"],"token_endpoint_auth_method":"none","software_id":"claude-web"}`
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.HandleOAuthRegister(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("register: status %d, body %q", w.Code, w.Body.String())
	}
	var response struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.ClientID == "" {
		t.Fatalf("register response: client_id=%q err=%v", response.ClientID, err)
	}
	return response.ClientID
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestOAuthCodeFlow(t *testing.T) {
	handler := newTestHandler()
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := mcpauth.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate mcpauth: %v", err)
	}
	handler.noteDB = db
	redirectURI := "https://example.com/callback"
	clientID := registerOAuthTestClient(t, handler, redirectURI)
	verifier := strings.Repeat("v", 48)

	t.Run("authorize issues a bound code and redirects", func(t *testing.T) {
		query := url.Values{
			"client_id":             {clientID},
			"redirect_uri":          {redirectURI},
			"response_type":         {"code"},
			"state":                 {"abc"},
			"code_challenge":        {pkceChallenge(verifier)},
			"code_challenge_method": {"S256"},
		}
		req := httptest.NewRequest(http.MethodGet, "/authorize?"+query.Encode(), nil)
		w := httptest.NewRecorder()
		handler.HandleOAuthAuthorize(w, req)

		if w.Code != http.StatusFound {
			t.Fatalf("want 302, got %d: %s", w.Code, w.Body.String())
		}
		loc, err := url.Parse(w.Header().Get("Location"))
		if err != nil {
			t.Fatalf("bad Location: %v", err)
		}
		code := loc.Query().Get("code")
		if len(code) < 20 || loc.Query().Get("state") != "abc" {
			t.Fatalf("redirect missing code/state: %s", loc.String())
		}

		form := url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"client_id":     {clientID},
			"redirect_uri":  {redirectURI},
			"code_verifier": {verifier},
		}
		tokenReq := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
		tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		tokenW := httptest.NewRecorder()
		handler.HandleOAuthToken(tokenW, tokenReq)
		if tokenW.Code != http.StatusOK {
			t.Fatalf("token: status %d, body %q", tokenW.Code, tokenW.Body.String())
		}
		var token map[string]any
		if err := json.Unmarshal(tokenW.Body.Bytes(), &token); err != nil || token["access_token"] == "" || token["token_type"] != "Bearer" {
			t.Fatalf("token response = %#v, err=%v", token, err)
		}
	})

	t.Run("authorize rejects unregistered redirect URI", func(t *testing.T) {
		query := url.Values{
			"client_id":             {clientID},
			"redirect_uri":          {"https://evil.example/callback"},
			"response_type":         {"code"},
			"code_challenge":        {pkceChallenge(verifier)},
			"code_challenge_method": {"S256"},
		}
		w := httptest.NewRecorder()
		handler.HandleOAuthAuthorize(w, httptest.NewRequest(http.MethodGet, "/authorize?"+query.Encode(), nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("want 400, got %d", w.Code)
		}
	})

	t.Run("registration rejects insecure remote redirect", func(t *testing.T) {
		body := `{"redirect_uris":["http://evil.example/callback"],"token_endpoint_auth_method":"none"}`
		w := httptest.NewRecorder()
		handler.HandleOAuthRegister(w, httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("want 400, got %d", w.Code)
		}
	})

	t.Run("registration allows localhost HTTP", func(t *testing.T) {
		registerOAuthTestClient(t, handler, "http://localhost:3000/callback")
	})

	t.Run("token rejects wrong PKCE verifier", func(t *testing.T) {
		code := handler.generateOAuthCodeFor(oauthCode{
			clientID: clientID, redirectURI: redirectURI, codeChallenge: pkceChallenge(verifier),
		})
		form := url.Values{
			"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
			"redirect_uri": {redirectURI}, "code_verifier": {"wrong"},
		}
		req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		handler.HandleOAuthToken(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("want 400, got %d", w.Code)
		}
	})

	t.Run("code is single-use", func(t *testing.T) {
		code := handler.generateOAuthCode()
		if _, ok := handler.consumeOAuthCode(code); !ok {
			t.Fatal("first consume should succeed")
		}
		if _, ok := handler.consumeOAuthCode(code); ok {
			t.Fatal("second consume should fail")
		}
	})

	t.Run("expired code is rejected", func(t *testing.T) {
		code := "expired-test-code"
		handler.oauthCodesMu.Lock()
		if handler.oauthCodes == nil {
			handler.oauthCodes = make(map[string]oauthCode)
		}
		handler.oauthCodes[code] = oauthCode{expiresAt: time.Now().Add(-time.Minute)}
		handler.oauthCodesMu.Unlock()
		if _, ok := handler.consumeOAuthCode(code); ok {
			t.Fatal("expired code should be rejected")
		}
	})
}
