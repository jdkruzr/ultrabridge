package auth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

const testSecret = "test-signing-secret"

// TestMintVerifyRoundtrip verifies a UB-minted token verifies back to its
// userId under the same secret.
// Verifies: spc-phase-1.AC2.3
func TestMintVerifyRoundtrip(t *testing.T) {
	const uid = "1184673925533868032"
	tok := Mint(uid, testSecret)

	got, err := Verify(tok, testSecret)
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if got != uid {
		t.Errorf("round-trip userId: got %q, want %q", got, uid)
	}
}

// TestVerifyRejectsTamperedSignature verifies a flipped signature byte fails.
// Verifies: spc-phase-1.AC2.3
func TestVerifyRejectsTamperedSignature(t *testing.T) {
	tok := Mint("u1", testSecret)
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 token parts, got %d", len(parts))
	}
	// Flip the last character of the signature.
	sig := []byte(parts[2])
	if sig[len(sig)-1] == 'A' {
		sig[len(sig)-1] = 'B'
	} else {
		sig[len(sig)-1] = 'A'
	}
	tampered := parts[0] + "." + parts[1] + "." + string(sig)

	if _, err := Verify(tampered, testSecret); err == nil {
		t.Errorf("expected error for tampered signature, got nil")
	}
}

// TestVerifyRejectsWrongSecret verifies a token minted under one secret fails
// verification under another.
// Verifies: spc-phase-1.AC2.3
func TestVerifyRejectsWrongSecret(t *testing.T) {
	tok := Mint("u1", testSecret)
	if _, err := Verify(tok, "a-different-secret"); err == nil {
		t.Errorf("expected error for wrong secret, got nil")
	}
}

// TestMintClaimShape verifies the minted payload carries the keys the live
// device accepted in 0c: userId, createTime, key, exp.
func TestMintClaimShape(t *testing.T) {
	tok := Mint("u1", testSecret)
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 token parts, got %d", len(parts))
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	for _, k := range []string{"userId", "createTime", "key", "exp"} {
		if _, ok := claims[k]; !ok {
			t.Errorf("minted claims missing key %q: %v", k, claims)
		}
	}

	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if string(header) != `{"typ":"JWT","alg":"HS256"}` {
		t.Errorf("unexpected header: %s", header)
	}
}
