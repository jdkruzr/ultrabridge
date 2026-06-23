package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/appconfig"
	"github.com/sysop/ultrabridge/internal/logging"
	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/service"
)

// newDevicesGroupHandler builds a Handler with a real notedb (Config present),
// active Supernote + Boox sources, and a wired SyncDeviceService with one
// registered device — the full Devices-group surface.
func newDevicesGroupHandler(t *testing.T) *Handler {
	t.Helper()
	ctx := context.Background()
	testDB, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("notedb open: %v", err)
	}
	t.Cleanup(func() { testDB.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	notes := &mockNoteService{pipelineConfigured: true, booxEnabled: true}
	cfgService := service.NewConfigService(testDB, &appconfig.Config{})
	h := NewHandler(nil, notes, nil, cfgService, testDB, "", "", logger, logging.NewLogBroadcaster())
	h.SetSyncDeviceService(&fakeSyncDeviceService{devices: []service.SyncDevice{
		{SiteID: testSiteID, Name: "Viwoods AiPaper", LastSeen: 1700000000000, PendingOps: 3},
	}})
	h.SetRemarkableDeviceService(&fakeRemarkableDeviceService{devices: []service.RemarkableDevice{
		{DeviceID: "rm-device-001", Name: "reMarkable Paper Pro", LastSeen: 1700000000000},
	}})
	return h
}

// deviceSections renders /settings/devices (fragment) and splits it into the
// per-source <section class="device-source"> chunks, in document order.
func deviceSections(t *testing.T, h http.Handler) (body string, sections []string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/settings/devices", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /settings/devices = %d, want 200", w.Code)
	}
	body = w.Body.String()
	parts := strings.Split(body, `<section class="device-source`)
	return body, parts[1:]
}

// TestDevicesGroupUniformSections covers the per-source section frame.
// sync-model-and-settings-ia.AC6.1–AC6.5.
func TestDevicesGroupUniformSections(t *testing.T) {
	h := newDevicesGroupHandler(t)
	body, sections := deviceSections(t, h)

	// AC6.1: Sources card first, then three structurally identical sections.
	if len(sections) != 4 {
		t.Fatalf("device-source section count = %d, want 4", len(sections))
	}
	srcIdx := strings.Index(body, "<h2>Sources</h2>")
	firstSection := strings.Index(body, `<section class="device-source`)
	if srcIdx < 0 || firstSection < 0 || srcIdx > firstSection {
		t.Error("Sources card does not precede the device sections")
	}
	names := []string{"Supernote", "ForestNote", "reMarkable", "Boox"}
	for i, sec := range sections {
		if !strings.Contains(sec, "<h2>"+names[i]+"</h2>") {
			t.Errorf("section %d missing <h2>%s</h2>", i, names[i])
		}
		for _, subhead := range []string{">Configuration</h3>", ">Device management</h3>"} {
			if !strings.Contains(sec, subhead) {
				t.Errorf("%s section missing subhead %q", names[i], subhead)
			}
		}
		// AC6.2: exactly one banner per section — the same partial the Files
		// tabs render (shared block name "_sync_model_banner").
		if n := strings.Count(sec, "sync-model-banner"); n != 1 {
			t.Errorf("%s section has %d sync-model banners, want 1", names[i], n)
		}
	}

	sn, fn, rm, bx := sections[0], sections[1], sections[2], sections[3]

	// AC6.2: glyphs derive from each source's Direction.
	if !strings.Contains(sn, "⇅") || !strings.Contains(fn, "⇅") {
		t.Error("Supernote/ForestNote sections missing the ⇅ two-way glyph")
	}
	if !strings.Contains(bx, "⬇") {
		t.Error("Boox section missing the ⬇ receive-only glyph")
	}

	// AC6.3: ForestNote device management renders the registered device with
	// name, last-seen, and the prune + compact controls.
	for _, want := range []string{
		"Viwoods AiPaper",
		"2023-11-14", // formatTimestamp of LastSeen 1700000000000
		"/settings/sync-devices/prune",
		"/settings/sync-devices/compact",
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("ForestNote section missing %q", want)
		}
	}

	// AC6.4: Boox slot is the receive-only note, with no device controls.
	if !strings.Contains(bx, "No device registry — receive-only") {
		t.Error("Boox section missing the receive-only note")
	}
	for _, not := range []string{"/settings/sync-devices/prune", "/settings/sync-devices/compact"} {
		if strings.Contains(bx, not) {
			t.Errorf("Boox section wrongly contains %q", not)
		}
	}

	// AC6.5: Supernote slot is the reserved spc_devices placeholder, never
	// merged with ForestNote's list.
	if !strings.Contains(sn, "spc_devices") {
		t.Error("Supernote section missing the reserved spc_devices placeholder")
	}
	if strings.Contains(sn, "/settings/sync-devices/prune") {
		t.Error("Supernote section wrongly contains the ForestNote prune control")
	}

	for _, want := range []string{"reMarkable Paper Pro", "rm-devic"} {
		if !strings.Contains(rm, want) {
			t.Errorf("reMarkable section missing %q", want)
		}
	}
}
