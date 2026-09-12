package syncidentity

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/sysop/ultrabridge/internal/auth"
)

// AdminHandler must be wrapped with the host's account authentication. It also
// requires the BASIC-admin identity explicitly: generic bearer access is not
// enrollment authority. Only fixture wiring exists today. Deploy behind TLS.
func (s Store) AdminHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if auth.IdentityFromContext(r.Context()).Method != "basic" {
			http.Error(w, "admin_auth_required", 403)
			return
		}
		if r.Method != "POST" {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method_not_allowed", 405)
			return
		}
		// Browser-origin requests need the future settings UI's CSRF integration;
		// this candidate endpoint is deliberately native/API-only.
		if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
			http.Error(w, "browser_enrollment_not_enabled", 403)
			return
		}
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "json_required", 415)
			return
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
		d.DisallowUnknownFields()
		decode := func(v any) error {
			if d.Decode(v) != nil {
				return ErrInvalid
			}
			var extra any
			if d.Decode(&extra) != io.EOF {
				return ErrInvalid
			}
			return nil
		}
		var err error
		switch r.URL.Path {
		case "/sync/devices/v1/enroll":
			var e Enrollment
			err = decode(&e)
			if err == nil {
				err = s.Enroll(r.Context(), e)
			}
		case "/sync/devices/v1/revoke":
			var e struct {
				SiteID string `json:"site_id"`
			}
			err = decode(&e)
			if err == nil {
				err = s.Revoke(r.Context(), e.SiteID)
			}
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			code := http.StatusBadRequest
			message := "invalid_enrollment_request"
			switch {
			case errors.Is(err, ErrConflict), errors.Is(err, ErrAdoption):
				code = 409
				message = err.Error()
			case errors.Is(err, ErrServerSite):
				code = 403
				message = err.Error()
			case errors.Is(err, ErrInvalid):
				message = err.Error()
			default:
				// Never echo SQL/internal details or request credential material.
				code = 503
				message = "identity_store_unavailable"
			}
			http.Error(w, message, code)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// Bind authorizes each request independently, including capability, asset and
// search reads. No cached identity survives revocation and no Basic fallback.
func (s Store) Bind(next func(site string, w http.ResponseWriter, r *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Fields(r.Header.Get("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			unauthorized(w)
			return
		}
		site, err := s.Resolve(r.Context(), parts[1])
		if err != nil {
			if errors.Is(err, ErrInvalid) {
				unauthorized(w)
			} else {
				http.Error(w, "identity_store_unavailable", 503)
			}
			return
		}
		next(site, w, r)
	})
}
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="ForestNote Device"`)
	http.Error(w, "device_credential_required", 401)
}
