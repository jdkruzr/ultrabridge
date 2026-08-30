package mcpauth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/notedb"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

// TestMigrate_Idempotent verifies Migrate is safe to call multiple times.
func TestMigrate_Idempotent(t *testing.T) {
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	defer db.Close()

	// Call Migrate twice
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("second Migrate (idempotent): %v", err)
	}
}

func TestRegisterAndGetOAuthClient(t *testing.T) {
	db := openTestDB(t)
	want := OAuthClient{
		ClientName:              "Claude",
		RedirectURIs:            []string{"https://claude.ai/api/mcp/auth_callback"},
		TokenEndpointAuthMethod: "none",
	}
	registered, err := RegisterOAuthClient(context.Background(), db, want)
	if err != nil {
		t.Fatalf("RegisterOAuthClient: %v", err)
	}
	if registered.ClientID == "" || registered.CreatedAt == 0 {
		t.Fatalf("registered client missing generated fields: %#v", registered)
	}

	got, err := GetOAuthClient(context.Background(), db, registered.ClientID)
	if err != nil {
		t.Fatalf("GetOAuthClient: %v", err)
	}
	if got.ClientName != want.ClientName || got.TokenEndpointAuthMethod != "none" || len(got.RedirectURIs) != 1 || got.RedirectURIs[0] != want.RedirectURIs[0] {
		t.Fatalf("GetOAuthClient = %#v", got)
	}
}

// TestCreateToken verifies mcp-oauth.AC1.1: token generation and storage.
// - Raw token is 43 chars of URL-safe base64
// - Token hash is 64-char hex string
// - Each call generates different tokens
func TestCreateToken(t *testing.T) {
	db := openTestDB(t)

	rawToken1, tokenHash1, err := CreateToken(context.Background(), db, "test-label-1")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	// AC1.1: Verify raw token is 43 chars, valid URL-safe base64
	if len(rawToken1) != 43 {
		t.Errorf("raw token length: got %d, want 43", len(rawToken1))
	}
	if _, err := base64.RawURLEncoding.DecodeString(rawToken1); err != nil {
		t.Errorf("raw token not valid URL-safe base64: %v", err)
	}

	// AC1.1: Verify token hash is 64-char hex (SHA-256)
	if len(tokenHash1) != 64 {
		t.Errorf("token hash length: got %d, want 64", len(tokenHash1))
	}
	// Verify it's hex by trying to parse it
	if _, err := base64.StdEncoding.DecodeString(tokenHash1); err == nil {
		// If we can decode it as base64, it's not hex-like (hex chars are valid base64)
		// Better check: all chars should be 0-9, a-f
		for _, c := range tokenHash1 {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Errorf("token hash contains non-hex character: %c", c)
			}
		}
	}

	// AC1.1: Create a second token, verify raw tokens differ
	rawToken2, _, err := CreateToken(context.Background(), db, "test-label-2")
	if err != nil {
		t.Fatalf("CreateToken second: %v", err)
	}
	if rawToken1 == rawToken2 {
		t.Error("raw tokens should differ")
	}
}

// TestValidateToken_Valid verifies mcp-oauth.AC1.2: validation returns label.
func TestValidateToken_Valid(t *testing.T) {
	db := openTestDB(t)

	rawToken, _, err := CreateToken(context.Background(), db, "my-label")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	label, err := ValidateToken(context.Background(), db, rawToken)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}

	if label != "my-label" {
		t.Errorf("label: got %q, want %q", label, "my-label")
	}
}

// TestValidateToken_UpdatesLastUsed verifies mcp-oauth.AC1.3: last_used timestamp updates.
func TestValidateToken_UpdatesLastUsed(t *testing.T) {
	db := openTestDB(t)

	rawToken, tokenHash, err := CreateToken(context.Background(), db, "test-label")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	// First validation
	_, err = ValidateToken(context.Background(), db, rawToken)
	if err != nil {
		t.Fatalf("ValidateToken first: %v", err)
	}

	// List tokens and check last_used is non-zero
	tokens, err := ListTokens(context.Background(), db)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("expected 1 token, got %d", len(tokens))
	}
	if tokens[0].LastUsed == 0 {
		t.Error("last_used should be non-zero after validation")
	}

	firstLastUsed := tokens[0].LastUsed

	// Sleep briefly and validate again
	time.Sleep(10 * time.Millisecond)

	_, err = ValidateToken(context.Background(), db, rawToken)
	if err != nil {
		t.Fatalf("ValidateToken second: %v", err)
	}

	// List tokens again and verify last_used increased
	tokens, err = ListTokens(context.Background(), db)
	if err != nil {
		t.Fatalf("ListTokens second: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("expected 1 token, got %d", len(tokens))
	}
	if tokens[0].LastUsed <= firstLastUsed {
		t.Errorf("last_used should increase: first %d, second %d", firstLastUsed, tokens[0].LastUsed)
	}

	// Verify token hash matches
	if tokens[0].TokenHash != tokenHash {
		t.Errorf("token hash mismatch: got %q, want %q", tokens[0].TokenHash, tokenHash)
	}
}

// TestValidateToken_InvalidToken verifies mcp-oauth.AC1.4: invalid token returns error.
func TestValidateToken_InvalidToken(t *testing.T) {
	db := openTestDB(t)

	// Try to validate a random string that was never created
	_, err := ValidateToken(context.Background(), db, "this-is-a-random-string")
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("ValidateToken with invalid token: got %v, want ErrInvalidToken", err)
	}
}

// TestValidateToken_RevokedToken verifies mcp-oauth.AC1.5: revoked token returns error.
func TestValidateToken_RevokedToken(t *testing.T) {
	db := openTestDB(t)

	rawToken, tokenHash, err := CreateToken(context.Background(), db, "test-label")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	// Revoke the token
	if err := RevokeToken(context.Background(), db, tokenHash); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	// Try to validate the revoked token
	_, err = ValidateToken(context.Background(), db, rawToken)
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("ValidateToken with revoked token: got %v, want ErrInvalidToken", err)
	}
}

// TestListTokens verifies mcp-oauth.AC1.6: list returns all tokens with metadata.
// Tests token hash presence, label accuracy, timestamp validity, and ordering.
func TestListTokens(t *testing.T) {
	db := openTestDB(t)

	// Create two tokens with different labels
	rawToken1, _, err := CreateToken(context.Background(), db, "label-1")
	if err != nil {
		t.Fatalf("CreateToken 1: %v", err)
	}

	time.Sleep(5 * time.Millisecond) // Ensure created_at differs

	_, _, err = CreateToken(context.Background(), db, "label-2")
	if err != nil {
		t.Fatalf("CreateToken 2: %v", err)
	}

	// Validate one token to update last_used
	if _, err := ValidateToken(context.Background(), db, rawToken1); err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}

	tokens, err := ListTokens(context.Background(), db)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}

	// AC1.6: Verify we have both tokens
	if len(tokens) != 2 {
		t.Fatalf("expected 2 tokens, got %d", len(tokens))
	}

	// AC1.6: Verify ordering is newest first
	if tokens[0].Label != "label-2" || tokens[1].Label != "label-1" {
		t.Errorf("ordering should be newest first: got [%q, %q], want [%q, %q]",
			tokens[0].Label, tokens[1].Label, "label-2", "label-1")
	}

	// AC1.6: Verify both tokens have correct labels
	if tokens[0].Label != "label-2" {
		t.Errorf("token[0] label: got %q, want %q", tokens[0].Label, "label-2")
	}
	if tokens[1].Label != "label-1" {
		t.Errorf("token[1] label: got %q, want %q", tokens[1].Label, "label-1")
	}

	// AC1.6: Verify token hashes are non-empty and hex-like
	for i, tok := range tokens {
		if len(tok.TokenHash) != 64 {
			t.Errorf("token[%d] hash length: got %d, want 64", i, len(tok.TokenHash))
		}
		// First 8 chars can be used as truncated display
		if len(tok.TokenHash) < 8 {
			t.Errorf("token[%d] hash too short for truncation", i)
		}
	}

	// AC1.6: Verify created_at timestamps are valid and non-zero
	for i, tok := range tokens {
		if tok.CreatedAt <= 0 {
			t.Errorf("token[%d] created_at should be non-zero: %d", i, tok.CreatedAt)
		}
	}

	// AC1.6: Verify token[0] (label-2) has last_used=0 (never used)
	if tokens[0].LastUsed != 0 {
		t.Errorf("token[0] (label-2) last_used should be 0: got %d", tokens[0].LastUsed)
	}

	// AC1.6: Verify token[1] (label-1) has last_used set (was validated)
	if tokens[1].LastUsed == 0 {
		t.Error("token[1] (label-1) last_used should be non-zero after validation")
	}

	// AC1.6: Verify created_at ordering matches
	if tokens[0].CreatedAt < tokens[1].CreatedAt {
		t.Errorf("created_at ordering: token[0] should be >= token[1], got %d < %d",
			tokens[0].CreatedAt, tokens[1].CreatedAt)
	}
}

// TestRevokeToken_Nonexistent verifies RevokeToken is idempotent.
func TestRevokeToken_Nonexistent(t *testing.T) {
	db := openTestDB(t)

	// Revoke a hash that doesn't exist
	err := RevokeToken(context.Background(), db, "nonexistent-hash")
	if err != nil {
		t.Errorf("RevokeToken nonexistent: %v", err)
	}
}
