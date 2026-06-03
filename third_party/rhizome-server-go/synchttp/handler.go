// Package synchttp is the HTTP transport for /sync/v1: method + auth + JSON envelope around the
// syncsvc service, mapping its sentinel errors to status codes. See spec/protocol.md §I.6.
package synchttp

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jdkruzr/rhizome/server-go/auth"
	"github.com/jdkruzr/rhizome/server-go/syncsvc"
)

// Handler serves POST /sync/v1.
type Handler struct {
	svc  *syncsvc.Service
	auth auth.Authenticator
}

// New wires a Service and an Authenticator into an http.Handler.
func New(svc *syncsvc.Service, a auth.Authenticator) *Handler {
	return &Handler{svc: svc, auth: a}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.auth.Authenticate(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="rhizome"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req syncsvc.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	resp, err := h.svc.Sync(req)
	if err != nil {
		switch {
		case errors.Is(err, syncsvc.ErrSchemaMismatch), errors.Is(err, syncsvc.ErrUnsupportedVersion):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, syncsvc.ErrBadRequest):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		// Header already sent (200); nothing actionable beyond logging at the caller.
		return
	}
}
