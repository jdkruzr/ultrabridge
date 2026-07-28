package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/syncstore"
)

type fakeSyncAdmin struct {
	devices    []syncstore.DeviceRow
	devicesErr error
	pruned     []string
	pruneFound bool
	pruneErr   error
	labeled    map[string]string // site_id -> label, as SetDeviceLabel saw it
	labelFound bool
	labelErr   error
	outcome    syncstore.CompactOutcome
	compactErr error
}

func (f *fakeSyncAdmin) SetDeviceLabel(_ context.Context, siteID, label string) (bool, error) {
	if f.labeled == nil {
		f.labeled = map[string]string{}
	}
	f.labeled[siteID] = label
	return f.labelFound, f.labelErr
}

func (f *fakeSyncAdmin) Devices(ctx context.Context) ([]syncstore.DeviceRow, error) {
	return f.devices, f.devicesErr
}

func (f *fakeSyncAdmin) PruneDevice(ctx context.Context, siteID string) (bool, error) {
	f.pruned = append(f.pruned, siteID)
	return f.pruneFound, f.pruneErr
}

func (f *fakeSyncAdmin) CompactNow(ctx context.Context) (syncstore.CompactOutcome, error) {
	return f.outcome, f.compactErr
}

func TestNewSyncDeviceService_NilAdminYieldsNil(t *testing.T) {
	if svc := NewSyncDeviceService(nil); svc != nil {
		t.Error("nil admin must yield a nil service so the UI card can gate on it")
	}
}

func TestListSyncDevices_MapsAllFields(t *testing.T) {
	admin := &fakeSyncAdmin{devices: []syncstore.DeviceRow{{
		SiteID: "0000000000000000000000000A", Name: "Tablet", Label: "Go 10.3 II",
		FirstSeenMs: 1, LastSeenMs: 2, LastPullSeq: 3, AckedOpSeq: 4, PendingOps: 5,
		Stale: true, PinsWatermark: true,
	}}}
	got, err := NewSyncDeviceService(admin).ListSyncDevices(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := SyncDevice{
		SiteID: "0000000000000000000000000A", Name: "Tablet", Label: "Go 10.3 II",
		FirstSeen: 1, LastSeen: 2, LastPullSeq: 3, AckedOpSeq: 4, PendingOps: 5,
		Stale: true, PinsWatermark: true,
	}
	if len(got) != 1 || got[0] != want {
		t.Errorf("mapped device = %+v, want %+v", got, want)
	}
}

func TestDisplayName_LabelWinsOverDeviceReportedName(t *testing.T) {
	cases := []struct {
		name, label, reported, want string
	}{
		{"label set", "Go 10.3 II", "Tablet", "Go 10.3 II"},
		{"no label falls back to the device", "", "Tablet", "Tablet"},
		{"neither is empty, not a placeholder", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := (SyncDevice{Label: c.label, Name: c.reported}).DisplayName(); got != c.want {
				t.Errorf("SyncDevice.DisplayName() = %q, want %q", got, c.want)
			}
			if got := (RemarkableDevice{Label: c.label, Name: c.reported}).DisplayName(); got != c.want {
				t.Errorf("RemarkableDevice.DisplayName() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRenameSyncDevice(t *testing.T) {
	const site = "0000000000000000000000000A"

	t.Run("normalizes and forwards the label", func(t *testing.T) {
		admin := &fakeSyncAdmin{labelFound: true}
		if err := NewSyncDeviceService(admin).RenameSyncDevice(context.Background(), site, "  Go 10.3 II \n"); err != nil {
			t.Fatalf("rename: %v", err)
		}
		if got := admin.labeled[site]; got != "Go 10.3 II" {
			t.Errorf("stored label = %q, want it trimmed", got)
		}
	})

	t.Run("blank clears the label", func(t *testing.T) {
		admin := &fakeSyncAdmin{labelFound: true}
		if err := NewSyncDeviceService(admin).RenameSyncDevice(context.Background(), site, "   "); err != nil {
			t.Fatalf("rename: %v", err)
		}
		// Unlike the wire's device_name (where empty means "keep what you have"),
		// an operator submitting a blank field means "remove my label".
		if got, ok := admin.labeled[site]; !ok || got != "" {
			t.Errorf("stored label = %q (present=%v), want an empty string", got, ok)
		}
	})

	t.Run("truncates over-long labels rune-wise", func(t *testing.T) {
		admin := &fakeSyncAdmin{labelFound: true}
		long := strings.Repeat("é", MaxOperatorLabelLen+50)
		if err := NewSyncDeviceService(admin).RenameSyncDevice(context.Background(), site, long); err != nil {
			t.Fatalf("rename: %v", err)
		}
		if got := []rune(admin.labeled[site]); len(got) != MaxOperatorLabelLen {
			t.Errorf("stored label = %d runes, want %d (and never a split rune)", len(got), MaxOperatorLabelLen)
		}
	})

	t.Run("unknown device is not found", func(t *testing.T) {
		admin := &fakeSyncAdmin{labelFound: false}
		err := NewSyncDeviceService(admin).RenameSyncDevice(context.Background(), site, "Ghost")
		if !errors.Is(err, ErrSyncDeviceNotFound) {
			t.Errorf("rename error = %v, want ErrSyncDeviceNotFound", err)
		}
	})

	t.Run("propagates store errors", func(t *testing.T) {
		admin := &fakeSyncAdmin{labelErr: errors.New("db down")}
		if err := NewSyncDeviceService(admin).RenameSyncDevice(context.Background(), site, "X"); err == nil || err.Error() != "db down" {
			t.Errorf("rename error = %v, want db down", err)
		}
	})
}

func TestRenameRemarkableDevice_UnknownDevice(t *testing.T) {
	admin := &fakeRemarkableAdmin{labelMissing: true}
	err := NewRemarkableDeviceService(admin).RenameDevice(context.Background(), "dev-x", "Paper Pro")
	if !errors.Is(err, ErrRemarkableDeviceNotFound) {
		t.Errorf("rename error = %v, want ErrRemarkableDeviceNotFound", err)
	}
}

func TestPruneSyncDevice(t *testing.T) {
	admin := &fakeSyncAdmin{pruneFound: true}
	svc := NewSyncDeviceService(admin)
	if err := svc.PruneSyncDevice(context.Background(), "0000000000000000000000000A"); err != nil {
		t.Errorf("prune existing: %v", err)
	}
	if len(admin.pruned) != 1 || admin.pruned[0] != "0000000000000000000000000A" {
		t.Errorf("prune passthrough: %v", admin.pruned)
	}

	admin.pruneFound = false
	if err := svc.PruneSyncDevice(context.Background(), "0000000000000000000000000B"); !errors.Is(err, ErrSyncDeviceNotFound) {
		t.Errorf("prune missing: err = %v, want ErrSyncDeviceNotFound", err)
	}
}

func TestCompactNow_MapsOutcomeAndNeverNullsEvicted(t *testing.T) {
	admin := &fakeSyncAdmin{outcome: syncstore.CompactOutcome{
		Watermark: 7, CollapsedSuperseded: 2, PurgedTombstones: 1,
	}}
	got, err := NewSyncDeviceService(admin).CompactNow(context.Background())
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if got.Watermark != 7 || got.CollapsedSuperseded != 2 || got.PurgedTombstones != 1 {
		t.Errorf("mapped result = %+v", got)
	}
	if got.EvictedSites == nil {
		t.Error("EvictedSites must be [] not nil (JSON surface)")
	}
}
