package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSetupThenHomeRenders(t *testing.T) {
	h, _ := newSettingsGroupsHandler(t)
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		h.handleSetup(w, httptest.NewRequest(http.MethodGet, "/setup", nil))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<form") {
			t.Fatalf("setup page: status=%d body=%s", w.Code, w.Body.String())
		}
	}
	form := url.Values{"username": {"admin"}, "password": {"synthetic-test-password"}, "confirm": {"synthetic-test-password"}}
	req := httptest.NewRequest(http.MethodPost, "/setup/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.handleSetupSave(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("setup save: status=%d body=%s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.handleIndex(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<html") {
		t.Fatalf("home after setup: status=%d body=%s", w.Code, w.Body.String())
	}
}
