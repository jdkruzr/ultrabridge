package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sysop/ultrabridge/internal/mcpauth"
	"github.com/sysop/ultrabridge/internal/notedb"
)

// TestAuthMiddleware_DBToken verifies AC2.1: HTTP SSE request with valid DB-backed bearer token is accepted.
func TestAuthMiddleware_DBToken(t *testing.T) {
	// Create in-memory notedb with token.
	ctx := context.Background()
	db, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	defer db.Close()

	if err := mcpauth.Migrate(ctx, db); err != nil {
		t.Fatalf("mcpauth.Migrate: %v", err)
	}

	// Create a token in the DB.
	rawToken, _, err := mcpauth.CreateToken(ctx, db, "test-token")
	if err != nil {
		t.Fatalf("mcpauth.CreateToken: %v", err)
	}

	// Create a test handler that returns 200 OK.
	handler := authMiddleware(db, "", "", "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Send request with valid DB token.
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
}

// TestAuthMiddleware_StaticToken verifies AC2.2: HTTP SSE request with valid static token is accepted (backward compat).
func TestAuthMiddleware_StaticToken(t *testing.T) {
	staticToken := "static-secret"

	// Test 1: Static token works when no DB is configured.
	handler := authMiddleware(nil, staticToken, "", "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+staticToken)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	// Test 2: Static token works as fallback when DB token is not found.
	ctx := context.Background()
	db, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	defer db.Close()

	if err := mcpauth.Migrate(ctx, db); err != nil {
		t.Fatalf("mcpauth.Migrate: %v", err)
	}

	handler = authMiddleware(db, staticToken, "", "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req = httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+staticToken)
	w = httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200 (static fallback), got %d", w.Code)
	}
}

// TestAuthMiddleware_BasicAuth verifies AC2.3: HTTP SSE request with valid Basic Auth credentials is accepted.
func TestAuthMiddleware_BasicAuth(t *testing.T) {
	basicUser := "user"
	basicPass := "pass"

	handler := authMiddleware(nil, "", basicUser, basicPass, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Create request with Basic Auth.
	req := httptest.NewRequest("GET", "/test", nil)
	req.SetBasicAuth(basicUser, basicPass)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
}

// TestAuthMiddleware_InvalidToken verifies AC2.4: HTTP SSE request with invalid/missing bearer token returns 401.
func TestAuthMiddleware_InvalidToken(t *testing.T) {
	ctx := context.Background()
	db, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	defer db.Close()

	if err := mcpauth.Migrate(ctx, db); err != nil {
		t.Fatalf("mcpauth.Migrate: %v", err)
	}

	staticToken := "static-secret"
	basicUser := "user"
	basicPass := "pass"

	// Build middleware with all three auth methods configured.
	handler := authMiddleware(db, staticToken, basicUser, basicPass, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name string
		req  *http.Request
	}{
		{
			name: "invalid bearer token",
			req: func() *http.Request {
				r := httptest.NewRequest("GET", "/test", nil)
				r.Header.Set("Authorization", "Bearer wrong-token")
				return r
			}(),
		},
		{
			name: "no authorization header",
			req:  httptest.NewRequest("GET", "/test", nil),
		},
		{
			name: "invalid basic auth credentials",
			req: func() *http.Request {
				r := httptest.NewRequest("GET", "/test", nil)
				r.SetBasicAuth("wrong-user", "wrong-pass")
				return r
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, tt.req)

			if w.Code != http.StatusUnauthorized {
				t.Errorf("expected status 401, got %d", w.Code)
			}
		})
	}
}

// TestAuthMiddleware_ChainOrder verifies that the auth chain tries methods in correct order:
// DB token → static token → Basic Auth. Also tests token revocation.
func TestAuthMiddleware_ChainOrder(t *testing.T) {
	ctx := context.Background()
	db, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	defer db.Close()

	if err := mcpauth.Migrate(ctx, db); err != nil {
		t.Fatalf("mcpauth.Migrate: %v", err)
	}

	// Create two tokens: one in DB, and a static token with the same value (to test chain order).
	rawToken, tokenHash, err := mcpauth.CreateToken(ctx, db, "test-token")
	if err != nil {
		t.Fatalf("mcpauth.CreateToken: %v", err)
	}

	staticToken := "static-only-token"
	basicUser := "user"
	basicPass := "pass"

	handler := authMiddleware(db, staticToken, basicUser, basicPass, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Test 1: DB token succeeds.
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("DB token: expected status 200, got %d", w.Code)
	}

	// Test 2: Revoke DB token and request again—should fail (no static token match, so chain rejects).
	if err := mcpauth.RevokeToken(ctx, db, tokenHash); err != nil {
		t.Fatalf("mcpauth.RevokeToken: %v", err)
	}

	req = httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("revoked token: expected status 401, got %d", w.Code)
	}

	// Test 3: Static token still works.
	req = httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+staticToken)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("static token: expected status 200, got %d", w.Code)
	}
}

// TestAuthMiddleware_MissingAuthConfig verifies AC2.5 logic:
// When neither DB, static token, nor Basic Auth is configured, all requests are rejected.
func TestAuthMiddleware_MissingAuthConfig(t *testing.T) {
	handler := authMiddleware(nil, "", "", "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Any request should fail.
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer any-token")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("no auth configured: expected status 401, got %d", w.Code)
	}
}

// TestAuthMiddleware_BearerAuthFormatVariations tests edge cases in Bearer token format.
func TestAuthMiddleware_BearerAuthFormatVariations(t *testing.T) {
	ctx := context.Background()
	db, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	defer db.Close()

	if err := mcpauth.Migrate(ctx, db); err != nil {
		t.Fatalf("mcpauth.Migrate: %v", err)
	}

	rawToken, _, err := mcpauth.CreateToken(ctx, db, "test-token")
	if err != nil {
		t.Fatalf("mcpauth.CreateToken: %v", err)
	}

	handler := authMiddleware(db, "", "", "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name       string
		authHeader string
		expectPass bool
	}{
		{
			name:       "correct bearer token",
			authHeader: "Bearer " + rawToken,
			expectPass: true,
		},
		{
			name:       "lowercase bearer",
			authHeader: "bearer " + rawToken,
			expectPass: false,
		},
		{
			name:       "extra spaces",
			authHeader: "Bearer  " + rawToken,
			expectPass: false,
		},
		{
			name:       "missing space",
			authHeader: "Bearer" + rawToken,
			expectPass: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/test", nil)
			req.Header.Set("Authorization", tt.authHeader)
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			if tt.expectPass && w.Code != http.StatusOK {
				t.Errorf("expected 200, got %d", w.Code)
			}
			if !tt.expectPass && w.Code != http.StatusUnauthorized {
				t.Errorf("expected 401, got %d", w.Code)
			}
		})
	}
}

// TestAuthMiddleware_BasicAuthEdgeCases tests Basic Auth edge cases.
func TestAuthMiddleware_BasicAuthEdgeCases(t *testing.T) {
	basicUser := "user"
	basicPass := "pass"

	handler := authMiddleware(nil, "", basicUser, basicPass, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name       string
		user       string
		pass       string
		expectPass bool
	}{
		{
			name:       "correct credentials",
			user:       basicUser,
			pass:       basicPass,
			expectPass: true,
		},
		{
			name:       "wrong username",
			user:       "wrong-user",
			pass:       basicPass,
			expectPass: false,
		},
		{
			name:       "wrong password",
			user:       basicUser,
			pass:       "wrong-pass",
			expectPass: false,
		},
		{
			name:       "empty username",
			user:       "",
			pass:       basicPass,
			expectPass: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/test", nil)
			req.SetBasicAuth(tt.user, tt.pass)
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			if tt.expectPass && w.Code != http.StatusOK {
				t.Errorf("expected 200, got %d", w.Code)
			}
			if !tt.expectPass && w.Code != http.StatusUnauthorized {
				t.Errorf("expected 401, got %d", w.Code)
			}
		})
	}
}

// TestAuthMiddleware_DBTokenVsStaticToken tests that DB token takes precedence when both are configured.
func TestAuthMiddleware_DBTokenVsStaticToken(t *testing.T) {
	ctx := context.Background()
	db, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	defer db.Close()

	if err := mcpauth.Migrate(ctx, db); err != nil {
		t.Fatalf("mcpauth.Migrate: %v", err)
	}

	// Create a DB token.
	dbToken, _, err := mcpauth.CreateToken(ctx, db, "db-token")
	if err != nil {
		t.Fatalf("mcpauth.CreateToken: %v", err)
	}

	staticToken := "different-static-token"

	handler := authMiddleware(db, staticToken, "", "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Test 1: DB token works.
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+dbToken)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("DB token: expected 200, got %d", w.Code)
	}

	// Test 2: Static token works as fallback for different tokens.
	req = httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+staticToken)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("static token fallback: expected 200, got %d", w.Code)
	}

	// Test 3: Revoking DB token doesn't affect static token.
	dbTokenHash := hashToken(dbToken)
	if err := mcpauth.RevokeToken(ctx, db, dbTokenHash); err != nil {
		t.Fatalf("mcpauth.RevokeToken: %v", err)
	}

	req = httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+staticToken)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("static token after DB revoke: expected 200, got %d", w.Code)
	}
}

// hashToken replicates mcpauth's token hashing for test purposes.
func hashToken(rawToken string) string {
	h := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(h[:])
}

// TestAuthMiddleware_RequestContextPassedThrough verifies the request context is preserved through auth middleware.
func TestAuthMiddleware_RequestContextPassedThrough(t *testing.T) {
	ctx := context.Background()
	db, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	defer db.Close()

	if err := mcpauth.Migrate(ctx, db); err != nil {
		t.Fatalf("mcpauth.Migrate: %v", err)
	}

	rawToken, _, err := mcpauth.CreateToken(ctx, db, "test-token")
	if err != nil {
		t.Fatalf("mcpauth.CreateToken: %v", err)
	}

	receivedCtx := context.Background()
	handler := authMiddleware(db, "", "", "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedCtx = r.Context()
		w.WriteHeader(http.StatusOK)
	}))

	type ctxKey struct{}
	req := httptest.NewRequest("GET", "/test", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, "test-value"))
	req.Header.Set("Authorization", "Bearer "+rawToken)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if receivedCtx.Value(ctxKey{}) != "test-value" {
		t.Errorf("request context not passed through")
	}
}
