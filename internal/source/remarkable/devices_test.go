package remarkable

import (
	"context"
	"testing"
)

func labelTestStore(t *testing.T) *store {
	t.Helper()
	ctx := context.Background()
	db := testDB(t)
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return newStore(db, t.TempDir())
}

// TestSetDeviceLabel_SurvivesHeartbeat is the whole reason operator_label is its
// own column: touchDevice assigns device_desc = excluded.device_desc on every
// single check-in, so an operator's name stored there would not last a minute.
func TestSetDeviceLabel_SurvivesHeartbeat(t *testing.T) {
	ctx := context.Background()
	st := labelTestStore(t)

	if err := st.touchDevice(ctx, "dev-1", "reMarkable Paper Pro"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	found, err := st.setDeviceLabel(ctx, "dev-1", "Desk tablet")
	if err != nil || !found {
		t.Fatalf("setDeviceLabel = (%v, %v), want (true, nil)", found, err)
	}

	// The device checks in again, reporting a different description.
	if err := st.touchDevice(ctx, "dev-1", "rM2 (factory reset)"); err != nil {
		t.Fatalf("touch 2: %v", err)
	}

	devs, err := st.listDevices(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("got %d devices, want 1", len(devs))
	}
	if devs[0].Label != "Desk tablet" {
		t.Errorf("label = %q after a heartbeat, want it untouched by touchDevice", devs[0].Label)
	}
	if devs[0].DeviceDesc != "rM2 (factory reset)" {
		t.Errorf("device_desc = %q, want the reported value still to land in its own field", devs[0].DeviceDesc)
	}
}

func TestSetDeviceLabel_ClearAndUnknownDevice(t *testing.T) {
	ctx := context.Background()
	st := labelTestStore(t)
	if err := st.touchDevice(ctx, "dev-1", "Reported Name"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if _, err := st.setDeviceLabel(ctx, "dev-1", "My Tablet"); err != nil {
		t.Fatalf("set: %v", err)
	}

	// Empty clears it — display falls back to the device-reported description.
	if found, err := st.setDeviceLabel(ctx, "dev-1", ""); err != nil || !found {
		t.Fatalf("clear = (%v, %v), want (true, nil)", found, err)
	}
	devs, _ := st.listDevices(ctx)
	if len(devs) != 1 || devs[0].Label != "" {
		t.Errorf("label after clear = %+v, want empty", devs)
	}

	// An unpaired id is a miss, never an insert: only the pairing handshake may
	// create a device row.
	found, err := st.setDeviceLabel(ctx, "never-paired", "Ghost")
	if err != nil {
		t.Fatalf("set unknown: %v", err)
	}
	if found {
		t.Error("labeling an unpaired device_id reported success")
	}
	if devs, _ := st.listDevices(ctx); len(devs) != 1 {
		t.Errorf("labeling an unpaired device_id created a row: %+v", devs)
	}
}
