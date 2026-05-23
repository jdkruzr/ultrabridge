// Package oss implements the Supernote Private Cloud presigned-URL signing
// primitive used for file download (Phase 3) and upload (Phase 4). It is a leaf
// package — it imports nothing internal — so handlers and the server can both
// depend on it without an import cycle.
//
// Despite the real-SPC class name SignVerifier, the URL signing is plain
// SHA-256 with the secret concatenated into the data and the digest hex-encoded
// — NOT HMAC. See docs/spc-protocol.md §6 for the decompiled algorithm and the
// golden-master vectors pinned in sign_test.go.
package oss

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"time"
)

// Freshness windows from SignVerifier.java (§6): downloads are valid for 24h,
// uploads for 30 min.
const (
	downloadTTL = 24 * time.Hour
	uploadTTL   = 30 * time.Minute
)

// EncryptPath base64url-encodes (no padding) the UTF-8 bytes of a path. The
// real-SPC method is named encryptPath but is just URL-safe Base64
// (O_OssLocalController.java:192).
func EncryptPath(path string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(path))
}

// DecryptPath reverses EncryptPath.
func DecryptPath(enc string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Signer signs and verifies presigned URLs against a single per-install secret.
// Because UB issues and verifies these URLs itself (the device treats them as
// opaque), the secret need not match real SPC's hardcoded SECRET_KEY. Now is an
// injectable clock for tests; a nil Now uses time.Now.
type Signer struct {
	Secret string
	Now    func() time.Time
}

func (s *Signer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func sha256Hex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// DownloadSignature = sha256_hex(encPath + timestamp + nonce + secret).
func (s *Signer) DownloadSignature(encPath string, tsMillis int64, nonce string) string {
	return sha256Hex(encPath + strconv.FormatInt(tsMillis, 10) + nonce + s.Secret)
}

// UploadSignature = sha256_hex(encPath + timestamp + nonce + fileSize + secret).
// Real SPC always passes fileSize 0 (O_OssLocalController:80). Reused by Phase 4.
func (s *Signer) UploadSignature(encPath string, tsMillis int64, nonce string, fileSize int64) string {
	return sha256Hex(encPath + strconv.FormatInt(tsMillis, 10) + nonce + strconv.FormatInt(fileSize, 10) + s.Secret)
}

// ValidateDownload returns true iff the signature matches and the timestamp is
// within the 24h download window. There is no nonce-replay tracking in SPC —
// the timestamp window is the only freshness guard (§6).
func (s *Signer) ValidateDownload(sig string, tsMillis int64, nonce, encPath string) bool {
	if !s.withinWindow(tsMillis, downloadTTL) {
		return false
	}
	return constEq(sig, s.DownloadSignature(encPath, tsMillis, nonce))
}

// ValidateUpload returns true iff the signature matches and the timestamp is
// within the 30min upload window. Reused by Phase 4.
func (s *Signer) ValidateUpload(sig string, tsMillis int64, nonce, encPath string, fileSize int64) bool {
	if !s.withinWindow(tsMillis, uploadTTL) {
		return false
	}
	return constEq(sig, s.UploadSignature(encPath, tsMillis, nonce, fileSize))
}

// withinWindow reports whether tsMillis is no older than ttl relative to now.
// A future timestamp (negative age) is tolerated, matching SPC's one-sided check.
func (s *Signer) withinWindow(tsMillis int64, ttl time.Duration) bool {
	age := s.now().Sub(time.UnixMilli(tsMillis))
	return age <= ttl
}

func constEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
