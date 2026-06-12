package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/service"
)

type fakeSyncDeviceService struct {
	devices    []service.SyncDevice
	pruned     []string
	pruneErr   error
	compacted  int
	compactRes service.SyncCompactResult
}

func (f *fakeSyncDeviceService) ListSyncDevices(context.Context) ([]service.SyncDevice, error) {
	return f.devices, nil
}

func (f *fakeSyncDeviceService) PruneSyncDevice(_ context.Context, siteID string) error {
	if f.pruneErr != nil {
		return f.pruneErr
	}
	f.pruned = append(f.pruned, siteID)
	return nil
}

func (f *fakeSyncDeviceService) CompactNow(context.Context) (service.SyncCompactResult, error) {
	f.compacted++
	return f.compactRes, nil
}

const testSiteID = "01HZXM5K8PQRSTVWXYZ0123456"

func TestSettings_SyncDevicesCard(t *testing.T) {
	h := newTestHandler()
	h.SetSyncDeviceService(&fakeSyncDeviceService{devices: []service.SyncDevice{
		{SiteID: testSiteID, Name: "Viwoods AiPaper", LastSeen: 1700000000000, PendingOps: 3},
		{SiteID: "01HZXM5K8PQRSTVWXYZ0123457", Name: "", Stale: true},
	}})

	req := httptest.NewRequest("GET", "/settings/devices", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /settings/devices = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"Sync Devices", "Viwoods AiPaper", "(unnamed)", "01HZXM5K", "Stale", "Compact Relay Log"} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
}

func TestSettings_SyncDevicesCardHiddenWhenUnwired(t *testing.T) {
	h := newTestHandler()
	req := httptest.NewRequest("GET", "/settings/devices", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /settings/devices = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "Sync Devices") {
		t.Error("Sync Devices card rendered with no SyncDeviceService wired")
	}
}

func TestSyncDevicePrune(t *testing.T) {
	h := newTestHandler()
	fake := &fakeSyncDeviceService{}
	h.SetSyncDeviceService(fake)

	post := func(siteID string) *httptest.ResponseRecorder {
		form := url.Values{"site_id": {siteID}}
		req := httptest.NewRequest("POST", "/settings/sync-devices/prune", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	if w := post(testSiteID); w.Code != http.StatusSeeOther {
		t.Errorf("prune existing = %d, want 303", w.Code)
	}
	if len(fake.pruned) != 1 || fake.pruned[0] != testSiteID {
		t.Errorf("prune passthrough: %v", fake.pruned)
	}

	if w := post("not-a-ulid"); w.Code != http.StatusBadRequest {
		t.Errorf("prune invalid ULID = %d, want 400", w.Code)
	}

	fake.pruneErr = service.ErrSyncDeviceNotFound
	if w := post(testSiteID); w.Code != http.StatusNotFound {
		t.Errorf("prune missing device = %d, want 404", w.Code)
	}
}

func TestSyncDeviceRoutes404WhenUnwired(t *testing.T) {
	h := newTestHandler()
	for _, path := range []string{"/settings/sync-devices/prune", "/settings/sync-devices/compact"} {
		req := httptest.NewRequest("POST", path, strings.NewReader("site_id="+testSiteID))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("POST %s with no service = %d, want 404", path, w.Code)
		}
	}
}

func TestSyncDeviceCompact(t *testing.T) {
	h := newTestHandler()
	fake := &fakeSyncDeviceService{compactRes: service.SyncCompactResult{
		Watermark: 42, CollapsedSuperseded: 5, PurgedTombstones: 2, EvictedSites: []string{},
	}}
	h.SetSyncDeviceService(fake)

	// HX request re-renders settings with the result flash.
	req := httptest.NewRequest("POST", "/settings/sync-devices/compact", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("compact = %d", w.Code)
	}
	if fake.compacted != 1 {
		t.Errorf("CompactNow called %d times, want 1", fake.compacted)
	}
	// Collapse whitespace so the assertions don't depend on template indentation.
	body := strings.Join(strings.Fields(w.Body.String()), " ")
	for _, want := range []string{"Compaction pass complete", "collapsed 5", "purged 2", "watermark 42"} {
		if !strings.Contains(body, want) {
			t.Errorf("compact flash missing %q", want)
		}
	}

	// Non-HX redirects back to the Devices settings group.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/settings/sync-devices/compact", nil))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/settings/devices" {
		t.Errorf("non-HX compact = %d → %q, want 303 → /settings/devices", w.Code, w.Header().Get("Location"))
	}
}

func TestAPIv1SyncDevices(t *testing.T) {
	h := newTestHandler()
	fake := &fakeSyncDeviceService{devices: []service.SyncDevice{
		{SiteID: testSiteID, Name: "Tablet", LastSeen: 1700000000000, PendingOps: 2, Stale: true},
	}}
	h.SetSyncDeviceService(fake)

	t.Run("list", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/sync/devices", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var got struct {
			Devices []service.SyncDevice `json:"devices"`
		}
		if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Devices) != 1 || got.Devices[0].SiteID != testSiteID || !got.Devices[0].Stale {
			t.Errorf("devices = %+v", got.Devices)
		}
	})

	t.Run("list empty is [] not null", func(t *testing.T) {
		fake.devices = nil
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/sync/devices", nil))
		if !strings.Contains(w.Body.String(), `"devices":[]`) {
			t.Errorf("empty list body = %s, want \"devices\":[]", w.Body.String())
		}
	})

	t.Run("prune", func(t *testing.T) {
		fake.pruneErr = nil
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/v1/sync/devices/"+testSiteID, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if len(fake.pruned) != 1 || fake.pruned[0] != testSiteID {
			t.Errorf("prune passthrough: %v", fake.pruned)
		}
		if !strings.Contains(w.Body.String(), `"pruned":true`) {
			t.Errorf("prune body = %s", w.Body.String())
		}
	})

	t.Run("prune invalid id", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/v1/sync/devices/nope", nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("status=%d, want 400", w.Code)
		}
	})

	t.Run("prune missing device", func(t *testing.T) {
		fake.pruneErr = service.ErrSyncDeviceNotFound
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/v1/sync/devices/"+testSiteID, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("status=%d, want 404", w.Code)
		}
	})

	t.Run("compact", func(t *testing.T) {
		fake.compactRes = service.SyncCompactResult{Watermark: 9, CollapsedSuperseded: 1, PurgedTombstones: 4, EvictedSites: []string{}}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/sync/compact", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var got service.SyncCompactResult
		if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Watermark != 9 || got.PurgedTombstones != 4 || got.EvictedSites == nil {
			t.Errorf("compact result = %+v", got)
		}
	})
}

func TestAPIv1SyncRoutes404WhenUnwired(t *testing.T) {
	h := newTestHandler()
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/sync/devices"},
		{http.MethodDelete, "/api/v1/sync/devices/" + testSiteID},
		{http.MethodPost, "/api/v1/sync/compact"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s with no service = %d, want 404", c.method, c.path, w.Code)
		}
	}
}
