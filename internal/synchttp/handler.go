// Package synchttp is the thin HTTP transport for /sync/v1: decode JSON →
// syncsvc.Sync → encode JSON, mapping service sentinel errors to status codes
// (spec §7.1). It never touches the store directly. Authentication (401) is
// handled upstream by the auth middleware that wraps this handler.
package synchttp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jdkruzr/rhizome/server-go/bounded"
	"github.com/sysop/ultrabridge/internal/syncsvc"
)

// DefaultMaxBytes caps the request body. v1 servers MAY accept large pushes
// (single user); a body over the cap yields 413 so a future cap can't silently
// truncate (spec §7.1). 0 disables the cap.
const DefaultMaxBytes = 64 << 20 // 64 MiB

// Syncer is the service entry point (satisfied by *syncsvc.Service).
type Syncer interface {
	Sync(ctx context.Context, req syncsvc.Request) (syncsvc.Response, error)
}

type handler struct {
	svc      Syncer
	maxBytes int64
	logger   *slog.Logger
}

// New returns the /sync/v1 handler. maxBytes <= 0 disables the body cap.
func New(svc Syncer, maxBytes int64, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &handler{svc: svc, maxBytes: maxBytes, logger: logger}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, exists := r.Header[http.CanonicalHeaderKey(bounded.Header)]; exists {
		h.serveBounded(w, r)
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if h.maxBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, h.maxBytes)
	}

	var req syncsvc.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErr(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeErr(w, http.StatusBadRequest, "malformed JSON: "+err.Error())
		return
	}

	resp, err := h.svc.Sync(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, syncsvc.ErrBadRequest):
			writeErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, syncsvc.ErrSchemaMismatch), errors.Is(err, syncsvc.ErrUnsupportedVersion):
			writeErr(w, http.StatusConflict, err.Error())
		default:
			h.logger.Error("sync handler: internal error", "err", err)
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		// Status already sent (200) once the encoder writes; just log.
		h.logger.Error("sync handler: encode response", "err", err)
	}
}

func (h *handler) serveBounded(w http.ResponseWriter, r *http.Request) {
	limits := bounded.Defaults()
	if h.maxBytes > 0 && h.maxBytes < int64(limits.MaxBodyBytes) {
		limits.MaxBodyBytes = int(h.maxBytes)
		limits.TargetPageBytes = min(limits.TargetPageBytes, limits.MaxBodyBytes)
	}
	req, limits, err := bounded.ReadRequest(w, r, limits)
	var body []byte
	if err == nil {
		svc, ok := h.svc.(interface {
			SyncBounded(context.Context, bounded.Request, bounded.Limits) ([]byte, error)
		})
		if !ok {
			err = bounded.Fail(503, "bounded_service_unavailable")
		} else {
			body, err = svc.SyncBounded(r.Context(), req, limits)
		}
	}
	if err != nil {
		bounded.WriteError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
