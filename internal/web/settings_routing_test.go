package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// get issues a GET against the handler, optionally as an HTMX request.
func getSettingsPath(t *testing.T, h http.Handler, path string, hx bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if hx {
		req.Header.Set("HX-Request", "true")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestSettingsGroupRouting covers the deep-linkable Settings group routes.
// sync-model-and-settings-ia.AC4.2 (mechanism): groups render into
// #main-content with pushed URLs. AC4.3: the active group is highlighted.
// AC4.5: unknown groups fall back cleanly.
func TestSettingsGroupRouting(t *testing.T) {
	h := newTestHandler()

	// AC4.2: a non-HX GET renders the full page through the layout (the swap
	// target #main-content present), and an HX GET returns just the fragment.
	full := getSettingsPath(t, h, "/settings/ai", false)
	if full.Code != http.StatusOK {
		t.Fatalf("GET /settings/ai = %d, want 200", full.Code)
	}
	fullBody := full.Body.String()
	if !strings.Contains(fullBody, `id="main-content"`) {
		t.Error("non-HX group response missing #main-content swap target")
	}
	if !strings.Contains(fullBody, "<!DOCTYPE") {
		t.Error("non-HX group response is not a full page")
	}

	frag := getSettingsPath(t, h, "/settings/ai", true)
	if frag.Code != http.StatusOK {
		t.Fatalf("HX GET /settings/ai = %d, want 200", frag.Code)
	}
	if strings.Contains(frag.Body.String(), "<!DOCTYPE") {
		t.Error("HX group response rendered the full layout, want fragment")
	}

	// AC4.2: the sidebar group links push their URL (bookmarkable swaps).
	for _, g := range []string{"devices", "ai", "integrations", "system"} {
		link := `href="/settings/` + g + `" hx-get="/settings/` + g + `" hx-target="#main-content" hx-push-url="true"`
		if !strings.Contains(strings.Join(strings.Fields(fullBody), " "), link) {
			t.Errorf("sidebar missing push-url nav link for group %s", g)
		}
	}

	// AC4.3: exactly the requested group's nav-sub link is highlighted.
	for _, g := range []string{"devices", "ai", "integrations", "system"} {
		w := getSettingsPath(t, h, "/settings/"+g, false)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /settings/%s = %d, want 200", g, w.Code)
		}
		body := strings.Join(strings.Fields(w.Body.String()), " ")
		for _, other := range []string{"devices", "ai", "integrations", "system"} {
			active := `nav-item nav-sub active" href="/settings/` + other + `"`
			has := strings.Contains(body, active)
			if other == g && !has {
				t.Errorf("/settings/%s: its own nav link not marked active", g)
			}
			if other != g && has {
				t.Errorf("/settings/%s: nav link for %s wrongly marked active", g, other)
			}
		}
	}

	// AC4.5: unknown group falls back to the default with a clean 303.
	bogus := getSettingsPath(t, h, "/settings/bogus", false)
	if bogus.Code != http.StatusSeeOther || bogus.Header().Get("Location") != "/settings/devices" {
		t.Errorf("GET /settings/bogus = %d → %q, want 303 → /settings/devices",
			bogus.Code, bogus.Header().Get("Location"))
	}
}

// TestSyncDevicePruneRedirectTarget pins the non-HX prune redirect to the
// Devices settings group (no hash hack). sync-model-and-settings-ia.AC4.4.
// (Compact's redirect target is pinned in TestSyncDeviceCompact.)
func TestSyncDevicePruneRedirectTarget(t *testing.T) {
	h := newTestHandler()
	h.SetSyncDeviceService(&fakeSyncDeviceService{})

	form := url.Values{"site_id": {testSiteID}}
	req := httptest.NewRequest("POST", "/settings/sync-devices/prune", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/settings/devices" {
		t.Errorf("non-HX prune = %d → %q, want 303 → /settings/devices", w.Code, w.Header().Get("Location"))
	}
}
