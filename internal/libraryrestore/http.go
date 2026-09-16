package libraryrestore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jdkruzr/rhizome/server-go/assets"
	"github.com/sysop/ultrabridge/internal/auth"
	"github.com/sysop/ultrabridge/internal/syncgeneration"
	"github.com/sysop/ultrabridge/internal/syncidentity"
)

// Handler is explicitly mounted by an enrolled, quiesce-capable candidate host.
// Publication requires Basic administrator authentication. Discovery, baseline
// download and adoption accept a valid OLD key without granting normal sync access.
func (s *Service) Handler(admin *auth.Middleware) http.Handler {
	pub := admin.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth.IdentityFromContext(r.Context()).Method != "basic" {
			http.Error(w, "admin_auth_required", 403)
			return
		}
		var req struct {
			ID        string `json:"request_id"`
			Expected  string `json:"expected_generation"`
			Snapshot  string `json:"snapshot"`
			Publisher string `json:"publisher"`
		}
		if !decode(w, r, &req) {
			return
		}
		// Native publication may copy a large library. Ordinary row routes retain
		// the host's short timeout; cancellation still stops staging before commit.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(20 * time.Minute))
		b, err := s.Publish(r.Context(), syncgeneration.Request{ID: req.ID, Expected: req.Expected, SnapshotHash: req.Snapshot, Publisher: req.Publisher})
		respond(w, b, err)
	}))
	identity := syncidentity.Store{DB: s.DB}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
			http.Error(w, "native_restore_only", 403)
			return
		}
		if r.URL.Path == "/sync/restore/v1/publish" {
			pub.ServeHTTP(w, r)
			return
		}
		authz := r.Header.Get("Authorization")
		if !strings.HasPrefix(authz, "Bearer ") {
			http.Error(w, "device_key_required", 401)
			return
		}
		site, err := identity.Resolve(r.Context(), strings.TrimPrefix(authz, "Bearer "))
		if err != nil {
			http.Error(w, "invalid_device_key", 401)
			return
		}
		switch r.URL.Path {
		case "/sync/restore/v1/state":
			if r.Method != "GET" {
				http.Error(w, "method_not_allowed", 405)
				return
			}
			generation, err := syncgeneration.Current(r.Context(), s.DB)
			if err != nil {
				respond(w, nil, err)
				return
			}
			b, e := s.Baseline(r.Context())
			if e != nil && e != sql.ErrNoRows {
				respond(w, nil, e)
				return
			}
			var baseline *Baseline
			if e == nil {
				baseline = &b
			}
			_, _, admission := syncgeneration.Admit(r.Context(), s.DB, site)
			if admission != nil && !errors.Is(admission, syncgeneration.ErrReplaced) {
				respond(w, nil, admission)
				return
			}
			respond(w, map[string]any{"generation": generation, "needs_adoption": errors.Is(admission, syncgeneration.ErrReplaced), "baseline": baseline}, nil)
		case "/sync/restore/v1/adopt":
			var a Adoption
			if !decode(w, r, &a) {
				return
			}
			b, err := s.Adopt(r.Context(), site, a)
			respond(w, b, err)
		default:
			// Only the current baseline is downloadable through this exception.
			// A stale key cannot list arbitrary assets or write even a single chunk.
			if r.Method != "GET" {
				http.Error(w, "method_not_allowed", 405)
				return
			}
			b, e := s.Baseline(r.Context())
			if e != nil {
				respond(w, nil, e)
				return
			}
			prefix := "/sync/restore/v1/assets/" + b.Snapshot
			if r.URL.Path != prefix && !strings.HasPrefix(r.URL.Path, prefix+"/") {
				http.NotFound(w, r)
				return
			}
			if r.URL.Query().Get("generation") != b.Generation {
				respond(w, nil, syncgeneration.ErrConflict)
				return
			}
			clone := r.Clone(r.Context())
			u := *r.URL
			u.Path = "/sync/assets/v1/" + strings.TrimPrefix(r.URL.Path, "/sync/restore/v1/assets/")
			clone.URL = &u
			assets.NewHandler(&assets.SQLStore{DB: s.DB}).ServeHTTP(w, clone)
		}
	})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != "POST" {
		http.Error(w, "method_not_allowed", 405)
		return false
	}
	if r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "json_required", 415)
		return false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	var extra any
	if d.Decode(v) != nil || d.Decode(&extra) != io.EOF {
		http.Error(w, "invalid_restore_request", 400)
		return false
	}
	return true
}
func respond(w http.ResponseWriter, v any, err error) {
	if err != nil {
		status := 503
		code := "restore_unavailable"
		switch {
		case errors.Is(err, syncgeneration.ErrConflict), errors.Is(err, syncgeneration.ErrReplaced):
			status = 409
			code = err.Error()
		case errors.Is(err, ErrSnapshot), errors.Is(err, syncgeneration.ErrInvalid):
			status = 422
			code = "invalid_library_snapshot"
		}
		http.Error(w, code, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
