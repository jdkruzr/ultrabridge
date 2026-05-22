package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBindStatus verifies the bind/status stub returns a flat BindStatusVO with
// bindStatus=true and the success envelope fields at the top level.
// Verifies: spc-phase-1.AC1.1, spc-phase-1.AC1.4
func TestBindStatus(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/equipment/bind/status", strings.NewReader("{}"))
	rec := httptest.NewRecorder()

	BindStatus(rec, req)

	const want = `{"success":true,"errorCode":"","errorMsg":"","bindStatus":true}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("bind/status body:\n got  %q\n want %q", got, want)
	}
}
