package spcserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer builds a Server with no DB (the routes exercised here don't
// touch it).
func newTestServer() *Server {
	return New(Config{Mode: "server", JWTSecret: "s"})
}

// TestSocketIOMountedSameListener verifies /socket.io/ is served on the same
// mux as /api/* — an unauthenticated request reaches the socket handler's auth
// gate (401), not a 404. Verifies: spc-phase-1.AC3.1 (single listener)
func TestSocketIOMountedSameListener(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/socket.io/?EIO=3&transport=websocket", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusNotFound {
		t.Fatalf("/socket.io/ not mounted (404)")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 from socket auth gate, got %d", rec.Code)
	}

	// Sanity: /api/* still served on the same mux.
	apiReq := httptest.NewRequest(http.MethodPost, "/api/equipment/bind/status", nil)
	apiRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(apiRec, apiReq)
	if apiRec.Code != http.StatusOK {
		t.Errorf("bind/status on same mux: got %d", apiRec.Code)
	}
}

// TestSocketRegistryExposed verifies the registry is available for 1d pushes.
func TestSocketRegistryExposed(t *testing.T) {
	if newTestServer().SocketRegistry() == nil {
		t.Errorf("SocketRegistry() returned nil")
	}
}

// TestDownloadAuthBoundary verifies the Phase 3 routing intent (spc-phase-3.AC3.5):
// download_v3 is behind JWT, but GET /api/oss/download is NOT — the query-string
// signature is its only auth, since the device fetches it opaquely with no token.
// The JWT gate (auth.Middleware) signals failure with the SPC E0712 "not logged
// in" envelope (HTTP 200 body), not an HTTP status, so we distinguish on the body.
func TestDownloadAuthBoundary(t *testing.T) {
	srv := newTestServer()

	// download_v3 with no token → E0712 envelope (JWT-protected; handler never runs).
	v3 := httptest.NewRequest(http.MethodPost, "/api/file/3/files/download_v3", nil)
	v3rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(v3rec, v3)
	if !strings.Contains(v3rec.Body.String(), "E0712") {
		t.Errorf("download_v3 without token: body %q lacks E0712 (must be JWT-protected)", v3rec.Body.String())
	}

	// GET /api/oss/download with no token must reach the handler: not 404, and
	// NOT the E0712 envelope. With an invalid signature it returns the SPC 500
	// plain-text failure — proving it bypasses the JWT gate.
	dl := httptest.NewRequest(http.MethodGet, "/api/oss/download?path=x&signature=y&timestamp=0&nonce=n", nil)
	dlrec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(dlrec, dl)
	if dlrec.Code == http.StatusNotFound {
		t.Fatalf("GET /api/oss/download not mounted (404)")
	}
	if strings.Contains(dlrec.Body.String(), "E0712") {
		t.Errorf("GET /api/oss/download is behind the JWT gate (got E0712); it must be signature-only")
	}
	if dlrec.Code != http.StatusInternalServerError {
		t.Errorf("GET /api/oss/download with bad signature: got %d, want 500", dlrec.Code)
	}
}

func TestUploadAuthBoundary(t *testing.T) {
	srv := newTestServer()

	// apply + finish with no token → E0712 envelope (JWT-protected).
	for _, path := range []string{"/api/file/3/files/upload/apply", "/api/file/2/files/upload/finish"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if !strings.Contains(rec.Body.String(), "E0712") {
			t.Errorf("%s without token: body %q lacks E0712 (must be JWT-protected)", path, rec.Body.String())
		}
	}

	// POST /api/oss/upload with no token must reach the handler: not 404, not
	// E0712. With an invalid signature it returns the SPC 500 plain-text failure.
	up := httptest.NewRequest(http.MethodPost, "/api/oss/upload?path=x&signature=y&timestamp=0&nonce=n", nil)
	uprec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(uprec, up)
	if uprec.Code == http.StatusNotFound {
		t.Fatalf("POST /api/oss/upload not mounted (404)")
	}
	if strings.Contains(uprec.Body.String(), "E0712") {
		t.Errorf("POST /api/oss/upload is behind the JWT gate (got E0712); it must be signature-only")
	}
	if uprec.Code != http.StatusInternalServerError {
		t.Errorf("POST /api/oss/upload with bad signature: got %d, want 500", uprec.Code)
	}
}

func TestPartnerRoutesMountedWithAuthBoundary(t *testing.T) {
	srv := newTestServer()

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/file/3/files/list"},
		{http.MethodPost, "/api/file/list/query"},
		{http.MethodPost, "/api/file/path/query"},
		{http.MethodPost, "/api/file/folder/list/query"},
		{http.MethodPost, "/api/file/list/search"},
		{http.MethodPost, "/api/file/label/list/search"},
		{http.MethodPost, "/api/file/folder/add"},
		{http.MethodPost, "/api/file/download/url"},
		{http.MethodPost, "/api/file/upload/apply"},
		{http.MethodPost, "/api/file/3/files/upload/confirm"},
		{http.MethodPost, "/api/file/upload/finish"},
		{http.MethodPost, "/api/file/move"},
		{http.MethodPost, "/api/file/rename"},
		{http.MethodPost, "/api/file/copy"},
		{http.MethodPost, "/api/file/delete"},
		{http.MethodPost, "/api/file/recycle/list/query"},
		{http.MethodPost, "/api/file/recycle/revert"},
		{http.MethodPost, "/api/file/recycle/delete"},
		{http.MethodPost, "/api/file/recycle/clear"},
		{http.MethodPost, "/api/file/note/to/png"},
		{http.MethodPost, "/api/file/note/to/pdf"},
		{http.MethodPost, "/api/file/pdfwithmark/to/pdf"},
		{http.MethodGet, "/api/query/email/config"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Fatalf("%s %s not mounted", tc.method, tc.path)
		}
		if !strings.Contains(rec.Body.String(), "E0712") {
			t.Fatalf("%s %s without token: body %q lacks E0712", tc.method, tc.path, rec.Body.String())
		}
	}

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/official/system/base/param"},
		{http.MethodGet, "/api/query/email/publickey"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Fatalf("%s %s not mounted", tc.method, tc.path)
		}
		if strings.Contains(rec.Body.String(), "E0712") {
			t.Fatalf("%s %s should be pre-login, got %q", tc.method, tc.path, rec.Body.String())
		}
	}
}

func TestPartnerPreLoginCompatibilityRoutes(t *testing.T) {
	srv := newTestServer()

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/official/user/account/login/new"},
		{http.MethodGet, "/api/official/user/account/login/equipment"},
		{http.MethodPost, "/api/file/query/server"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s got HTTP %d body %q", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"success":true`) {
			t.Fatalf("%s %s should return pre-login success, got %q", tc.method, tc.path, rec.Body.String())
		}
	}
}
