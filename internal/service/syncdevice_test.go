package service

import (
	"context"
	"errors"
	"testing"

	"github.com/sysop/ultrabridge/internal/syncstore"
)

type fakeSyncAdmin struct {
	devices    []syncstore.DeviceRow
	devicesErr error
	pruned     []string
	pruneFound bool
	pruneErr   error
	outcome    syncstore.CompactOutcome
	compactErr error
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
		SiteID: "0000000000000000000000000A", Name: "Tablet",
		FirstSeenMs: 1, LastSeenMs: 2, LastPullSeq: 3, AckedOpSeq: 4, PendingOps: 5,
		Stale: true, PinsWatermark: true,
	}}}
	got, err := NewSyncDeviceService(admin).ListSyncDevices(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := SyncDevice{
		SiteID: "0000000000000000000000000A", Name: "Tablet",
		FirstSeen: 1, LastSeen: 2, LastPullSeq: 3, AckedOpSeq: 4, PendingOps: 5,
		Stale: true, PinsWatermark: true,
	}
	if len(got) != 1 || got[0] != want {
		t.Errorf("mapped device = %+v, want %+v", got, want)
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
