// Package auth provides the SPC device-facing JWT auth: a hand-rolled HS256
// mint/verify and the x-access-token middleware. The signing secret is
// Constant.SECRET (see appconfig KeySPCJWTSecret). UB auth is stateless — we
// verify the HMAC signature and trust the userId claim; we deliberately do NOT
// replicate SPC's Redis token-existence check (UB is single-user and the device
// does no client-side JWT validation, proven in Phase 0c).
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// jwtHeader is the fixed HS256 header the device expects, pre-encoded.
const jwtHeader = `{"typ":"JWT","alg":"HS256"}`

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// sign computes the RawURLEncoded HMAC-SHA256 of the signing input.
func sign(signingInput, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	return b64url(mac.Sum(nil))
}

// Mint produces an HS256 JWT for userID using secret, matching the claim shape
// the live device accepted in Phase 0c: {userId, createTime (unix sec),
// key "<userId>_<sec>_<ms>", exp (far future — terminal tokens are effectively
// non-expiring)}.
func Mint(userID, secret string) string {
	now := time.Now()
	sec := now.Unix()
	ms := now.UnixMilli()
	claims := map[string]any{
		"userId":     userID,
		"createTime": sec,
		"key":        fmt.Sprintf("%s_%d_%d", userID, sec, ms),
		"exp":        now.AddDate(60, 0, 0).Unix(),
	}
	cb, _ := json.Marshal(claims)
	signingInput := b64url([]byte(jwtHeader)) + "." + b64url(cb)
	return signingInput + "." + sign(signingInput, secret)
}

// Verify checks the token's HMAC signature against secret (constant-time) and
// returns the userId claim. It does not enforce exp — the device's terminal
// tokens are non-expiring, and UB's gate is signature validity plus a non-empty
// userId, per the stateless design.
func Verify(token, secret string) (userID string, err error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("auth: malformed token")
	}
	signingInput := parts[0] + "." + parts[1]
	if !hmac.Equal([]byte(sign(signingInput, secret)), []byte(parts[2])) {
		return "", errors.New("auth: signature mismatch")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("auth: decode payload: %w", err)
	}
	var claims struct {
		UserID string `json:"userId"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("auth: parse claims: %w", err)
	}
	if claims.UserID == "" {
		return "", errors.New("auth: empty userId claim")
	}
	return claims.UserID, nil
}
