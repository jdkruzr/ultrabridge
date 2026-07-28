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
	renamed    map[string]string // site_id -> label
	renameErr  error
	compacted  int
	compactRes service.SyncCompactResult
}

type fakeRemarkableDeviceService struct {
	devices   []service.RemarkableDevice
	documents []service.RemarkableDocument
	renamed   map[string]string // device_id -> label
	renameErr error
}

func (f *fakeRemarkableDeviceService) RenameDevice(_ context.Context, deviceID, label string) error {
	if f.renameErr != nil {
		return f.renameErr
	}
	if f.renamed == nil {
		f.renamed = map[string]string{}
	}
	f.renamed[deviceID] = label
	return nil
}

func (f *fakeRemarkableDeviceService) ListDevices(context.Context) ([]service.RemarkableDevice, error) {
	return f.devices, nil
}

func (f *fakeRemarkableDeviceService) ListDocuments(context.Context) ([]service.RemarkableDocument, error) {
	return f.documents, nil
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

func (f *fakeSyncDeviceService) RenameSyncDevice(_ context.Context, siteID, label string) error {
	if f.renameErr != nil {
		return f.renameErr
	}
	if f.renamed == nil {
		f.renamed = map[string]string{}
	}
	f.renamed[siteID] = label
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
		{SiteID: testSiteID, Name: "Viwoods AiPaper", Label: "Studio tablet", LastSeen: 1700000000000, PendingOps: 3},
		{SiteID: "01HZXM5K8PQRSTVWXYZ0123457", Name: "", Stale: true},
	}})

	req := httptest.NewRequest("GET", "/settings/devices", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /settings/devices = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Devices registered with the ForestNote sync server",
		`value="Studio tablet"`,         // the operator's label, editable in place
		"reports itself as",             // ...with the device's own name kept visible
		"Viwoods AiPaper",               //
		"Name this device",              // placeholder for the row with neither name
		"/settings/sync-devices/rename", // the rename form posts somewhere real
		"01HZXM5K", "Stale", "Compact Relay Log",
	} {
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
	if strings.Contains(w.Body.String(), "Devices registered with the ForestNote sync server") {
		t.Error("sync device registry rendered with no SyncDeviceService wired")
	}
}

func TestSettings_RemarkableDevicesCard(t *testing.T) {
	h := newTestHandler()
	h.SetRemarkableDeviceService(&fakeRemarkableDeviceService{devices: []service.RemarkableDevice{
		{DeviceID: "rm-device-001", Name: "reMarkable Paper Pro", FirstSeen: 1700000000000, LastSeen: 1700001000000},
	}})

	req := httptest.NewRequest("GET", "/settings/devices", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /settings/devices = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"reMarkable", "reMarkable Paper Pro", "rm-devic"} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
}

func TestAPIv1RemarkableDevices(t *testing.T) {
	h := newTestHandler()
	fake := &fakeRemarkableDeviceService{devices: []service.RemarkableDevice{
		{DeviceID: "rm-device-001", Name: "reMarkable 2", FirstSeen: 1700000000000, LastSeen: 1700000001000},
	}}
	h.SetRemarkableDeviceService(fake)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/remarkable/devices", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/remarkable/devices = %d", w.Code)
	}
	var body struct {
		Devices []service.RemarkableDevice `json:"devices"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Devices) != 1 || body.Devices[0].DeviceID != "rm-device-001" {
		t.Fatalf("devices = %+v", body.Devices)
	}

	t.Run("rename", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/remarkable/devices/rm-device-001",
			strings.NewReader(`{"label":"Desk tablet"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if got := fake.renamed["rm-device-001"]; got != "Desk tablet" {
			t.Errorf("label passthrough = %q", got)
		}
	})

	t.Run("rename missing device", func(t *testing.T) {
		fake.renameErr = service.ErrRemarkableDeviceNotFound
		defer func() { fake.renameErr = nil }()
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/remarkable/devices/ghost",
			strings.NewReader(`{"label":"X"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("status=%d, want 404", w.Code)
		}
	})
}

func TestAPIv1RemarkableDevices404WhenUnwired(t *testing.T) {
	h := newTestHandler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/remarkable/devices", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET /api/v1/remarkable/devices = %d, want 404", w.Code)
	}
}

func TestAPIv1RemarkableDocuments(t *testing.T) {
	h := newTestHandler()
	h.SetRemarkableDeviceService(&fakeRemarkableDeviceService{documents: []service.RemarkableDocument{
		{ID: "folder-1", Name: "Notebooks", Type: "folder", Parent: ""},
		{ID: "doc-1", Name: "Project Plan", Type: "document", Parent: "folder-1", PageCount: 5},
	}})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/remarkable/documents", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/remarkable/documents = %d", w.Code)
	}
	var body struct {
		Documents []service.RemarkableDocument `json:"documents"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Documents) != 2 {
		t.Fatalf("documents = %+v", body.Documents)
	}
	if body.Documents[1].ID != "doc-1" || body.Documents[1].PageCount != 5 {
		t.Fatalf("doc-1 = %+v, want PageCount 5", body.Documents[1])
	}
}

func TestAPIv1RemarkableDocuments404WhenUnwired(t *testing.T) {
	h := newTestHandler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/remarkable/documents", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET /api/v1/remarkable/documents = %d, want 404", w.Code)
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

func TestSyncDeviceRename(t *testing.T) {
	h := newTestHandler()
	fake := &fakeSyncDeviceService{}
	h.SetSyncDeviceService(fake)

	post := func(siteID, label string, hx bool) *httptest.ResponseRecorder {
		form := url.Values{"site_id": {siteID}, "label": {label}}
		req := httptest.NewRequest("POST", "/settings/sync-devices/rename", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if hx {
			req.Header.Set("HX-Request", "true")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	if w := post(testSiteID, "Boox Go 10.3 II", false); w.Code != http.StatusSeeOther {
		t.Errorf("rename = %d, want 303", w.Code)
	}
	if got := fake.renamed[testSiteID]; got != "Boox Go 10.3 II" {
		t.Errorf("label passthrough = %q", got)
	}

	// On HX the Devices group re-renders in place, showing the new name.
	fake.devices = []service.SyncDevice{{SiteID: testSiteID, Label: "Boox Go 10.3 II"}}
	w := post(testSiteID, "Boox Go 10.3 II", true)
	if w.Code != http.StatusOK {
		t.Fatalf("rename (HX) = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Boox Go 10.3 II") {
		t.Error("HX re-render does not show the new label")
	}

	if w := post("not-a-ulid", "X", false); w.Code != http.StatusBadRequest {
		t.Errorf("rename invalid ULID = %d, want 400", w.Code)
	}

	fake.renameErr = service.ErrSyncDeviceNotFound
	if w := post(testSiteID, "X", false); w.Code != http.StatusNotFound {
		t.Errorf("rename missing device = %d, want 404", w.Code)
	}
}

func TestRemarkableDeviceRename(t *testing.T) {
	h := newTestHandler()
	fake := &fakeRemarkableDeviceService{}
	h.SetRemarkableDeviceService(fake)

	post := func(deviceID, label string) *httptest.ResponseRecorder {
		form := url.Values{"device_id": {deviceID}, "label": {label}}
		req := httptest.NewRequest("POST", "/settings/remarkable-devices/rename", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	if w := post("rm-device-001", "Desk tablet"); w.Code != http.StatusSeeOther {
		t.Errorf("rename = %d, want 303", w.Code)
	}
	if got := fake.renamed["rm-device-001"]; got != "Desk tablet" {
		t.Errorf("label passthrough = %q", got)
	}

	// reMarkable ids have no guaranteed shape, so blank is the only rejection.
	if w := post("  ", "X"); w.Code != http.StatusBadRequest {
		t.Errorf("rename blank device_id = %d, want 400", w.Code)
	}

	fake.renameErr = service.ErrRemarkableDeviceNotFound
	if w := post("rm-device-001", "X"); w.Code != http.StatusNotFound {
		t.Errorf("rename missing device = %d, want 404", w.Code)
	}
}

func TestSyncDeviceRoutes404WhenUnwired(t *testing.T) {
	h := newTestHandler()
	for _, path := range []string{"/settings/sync-devices/prune", "/settings/sync-devices/rename", "/settings/sync-devices/compact"} {
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

	patch := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	t.Run("rename", func(t *testing.T) {
		fake.renameErr = nil
		w := patch("/api/v1/sync/devices/"+testSiteID, `{"label":"Boox Go 6 II"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if got := fake.renamed[testSiteID]; got != "Boox Go 6 II" {
			t.Errorf("label passthrough = %q", got)
		}
		if !strings.Contains(w.Body.String(), `"label":"Boox Go 6 II"`) {
			t.Errorf("rename body = %s", w.Body.String())
		}
	})

	t.Run("rename with empty label clears", func(t *testing.T) {
		if w := patch("/api/v1/sync/devices/"+testSiteID, `{"label":""}`); w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if got, ok := fake.renamed[testSiteID]; !ok || got != "" {
			t.Errorf("cleared label = %q (present=%v), want empty", got, ok)
		}
	})

	t.Run("rename without a label field is a 400", func(t *testing.T) {
		// An omitted field is indistinguishable from a typo'd one; failing loudly
		// beats silently clearing the operator's name.
		if w := patch("/api/v1/sync/devices/"+testSiteID, `{}`); w.Code != http.StatusBadRequest {
			t.Errorf("status=%d, want 400", w.Code)
		}
		if w := patch("/api/v1/sync/devices/"+testSiteID, `not json`); w.Code != http.StatusBadRequest {
			t.Errorf("malformed body status=%d, want 400", w.Code)
		}
	})

	t.Run("rename invalid id", func(t *testing.T) {
		if w := patch("/api/v1/sync/devices/nope", `{"label":"X"}`); w.Code != http.StatusBadRequest {
			t.Errorf("status=%d, want 400", w.Code)
		}
	})

	t.Run("rename missing device", func(t *testing.T) {
		fake.renameErr = service.ErrSyncDeviceNotFound
		if w := patch("/api/v1/sync/devices/"+testSiteID, `{"label":"X"}`); w.Code != http.StatusNotFound {
			t.Errorf("status=%d, want 404", w.Code)
		}
		fake.renameErr = nil
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
		{http.MethodPatch, "/api/v1/sync/devices/" + testSiteID},
		{http.MethodPost, "/api/v1/sync/compact"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s with no service = %d, want 404", c.method, c.path, w.Code)
		}
	}
}
