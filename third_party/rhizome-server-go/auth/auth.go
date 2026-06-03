// Package auth guards the sync endpoint. v1 ships single-account HTTP Basic; the Authenticator
// interface keeps it pluggable (a bearer-token or mTLS check drops in without touching synchttp).
// There is no multi-tenancy — one account per server instance (spec/protocol.md §I.6).
package auth

import (
	"crypto/subtle"
	"net/http"
)

// Authenticator reports whether a request is allowed to sync.
type Authenticator interface {
	Authenticate(r *http.Request) bool
}

// Basic is a single fixed username/password checked via HTTP Basic auth.
type Basic struct {
	user string
	pass string
}

// NewBasic builds a single-account Basic authenticator.
func NewBasic(user, pass string) Basic { return Basic{user: user, pass: pass} }

// Authenticate reports whether the request carries the configured credentials. The comparison is
// constant-time to avoid leaking the secret through timing.
func (b Basic) Authenticate(r *http.Request) bool {
	u, p, ok := r.BasicAuth()
	if !ok {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(u), []byte(b.user)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(p), []byte(b.pass)) == 1
	return userOK && passOK
}

// AllowAll is an Authenticator that permits every request — for local demos/tests only.
type AllowAll struct{}

func (AllowAll) Authenticate(*http.Request) bool { return true }
