// Package taskattach provides the signing primitive and content-addressed
// blob store backing UltraBridge's CalDAV ATTACH support. A third-party CalDAV
// client fetches an ATTACH URI with no auth header, so the attachment-serving
// endpoints can't sit behind UB's Bearer/Basic middleware — instead the URL
// itself carries a signature this package mints and verifies.
//
// It is a leaf package (imports nothing internal) so the web handler, the
// CalDAV backend, and main wiring can all depend on it without an import cycle,
// mirroring internal/spcserver/oss.
package taskattach

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/url"
	"strings"
)

// RoutePrefix is the path both the URL builders and the serving handlers agree
// on. Inline-binary downloads are RoutePrefix+"{sha}"; the FN page render is
// RoutePrefix+"fn-render".
const RoutePrefix = "/api/v1/attachments/"

// Signer produces and verifies STABLE (non-expiring) signatures for the public
// attachment-fetch URLs UB hands to third-party CalDAV clients. Unlike the SPC
// oss.Signer it deliberately has NO timestamp/TTL term: a synced task keeps its
// ATTACH URL indefinitely, so a freshness window would silently break the link
// later. Freshness isn't the threat model — the signature only gates which
// already-synced content a URL-holder may fetch. Rotating Secret invalidates
// every previously-issued URL at once.
//
// Signature = sha256_hex(strings.Join(parts,"|") + "|" + Secret) — the same
// plain-SHA-256-with-concatenated-secret style as internal/spcserver/oss (NOT
// HMAC). Callers MUST pass a fixed domain token as the first part (e.g.
// "attach", "fnrender") so a signature minted for one route can't be replayed
// against another.
type Signer struct{ Secret string }

// Sign returns the hex signature over the ordered parts. The parts are joined
// with "|"; ULID ids and hex shas never contain "|", and domain tokens are
// fixed literals, so the join is unambiguous in practice.
func (s Signer) Sign(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|") + "|" + s.Secret))
	return hex.EncodeToString(sum[:])
}

// Valid reports whether sig matches Sign(parts...), in constant time.
func (s Signer) Valid(sig string, parts ...string) bool {
	want := s.Sign(parts...)
	return subtle.ConstantTimeCompare([]byte(sig), []byte(want)) == 1
}

// SignedAttachmentPath returns the relative, signed download path for inline-
// binary content addressed by sha (the path segment IS the sha). Callers
// prepend the external base URL and may append cosmetic &type=/&name= params.
// The signature is over ("attach", sha).
func (s Signer) SignedAttachmentPath(sha string) string {
	return RoutePrefix + url.PathEscape(sha) + "?sig=" + url.QueryEscape(s.Sign("attach", sha))
}

// SignedFNRenderPath returns the relative, signed path that renders a
// ForestNote page to JPEG. notePath is the canonical forestnote:// URI in UB's
// fnpath form (forestnote://{notebook}/{page}). Signature is over
// ("fnrender", notePath).
func (s Signer) SignedFNRenderPath(notePath string) string {
	return RoutePrefix + "fn-render?path=" + url.QueryEscape(notePath) +
		"&sig=" + url.QueryEscape(s.Sign("fnrender", notePath))
}
