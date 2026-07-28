package syncstore

import (
	"context"
	"testing"
	"time"
)

func cursorRow(t *testing.T, s *Store, siteID string) (lastPull int64, name string, found bool) {
	t.Helper()
	err := s.db.QueryRowContext(context.Background(),
		`SELECT last_pull_seq, device_name FROM sync_cursors WHERE site_id = ?`, siteID).
		Scan(&lastPull, &name)
	if err != nil {
		return 0, "", false
	}
	return lastPull, name, true
}

func TestRecordCursor_DeviceNameSemantics(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// First sync carries a name → stored.
	if err := s.RecordCursor(ctx, siteA, 1, "Viwoods AiPaper"); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	if _, name, _ := cursorRow(t, s, siteA); name != "Viwoods AiPaper" {
		t.Errorf("after named sync: name = %q, want Viwoods AiPaper", name)
	}

	// An empty name (old client) updates the cursor but preserves the stored name.
	if err := s.RecordCursor(ctx, siteA, 2, ""); err != nil {
		t.Fatalf("record 2: %v", err)
	}
	if pull, name, _ := cursorRow(t, s, siteA); name != "Viwoods AiPaper" || pull != 2 {
		t.Errorf("after unnamed sync: (pull=%d, name=%q), want (2, Viwoods AiPaper)", pull, name)
	}

	// A new name replaces the old one (client rename propagates).
	if err := s.RecordCursor(ctx, siteA, 3, "Living Room Tablet"); err != nil {
		t.Fatalf("record 3: %v", err)
	}
	if _, name, _ := cursorRow(t, s, siteA); name != "Living Room Tablet" {
		t.Errorf("after rename: name = %q, want Living Room Tablet", name)
	}

	// A device that never sent a name has the '' default.
	if err := s.RecordCursor(ctx, siteB, 1, ""); err != nil {
		t.Fatalf("record siteB: %v", err)
	}
	if _, name, found := cursorRow(t, s, siteB); !found || name != "" {
		t.Errorf("never-named device: (name=%q, found=%v), want (\"\", true)", name, found)
	}
}

func TestListDevices_FieldsStalenessAndPinning(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// siteA authors three ops; siteB authors none but pulls everything.
	if _, err := s.ApplyBatch(ctx, siteA, []Op{
		notebookOp(siteA, 1, 1000, "v1", nil),
		notebookOp(siteA, 2, 2000, "v2", nil),
		notebookOp(siteA, 3, 3000, "v3", nil),
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// siteA has pulled nothing (last_pull_seq 0); siteB is fully caught up.
	if err := s.RecordCursor(ctx, siteA, 0, "Laggard Tablet"); err != nil {
		t.Fatalf("cursor A: %v", err)
	}
	if err := s.RecordCursor(ctx, siteB, 3, ""); err != nil {
		t.Fatalf("cursor B: %v", err)
	}

	now := time.Now().UnixMilli()
	const horizon = int64(1000 * 60)
	devs, err := s.ListDevices(ctx, now, horizon)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(devs) != 2 {
		t.Fatalf("got %d devices, want 2", len(devs))
	}
	byID := map[string]DeviceRow{}
	for _, d := range devs {
		byID[d.SiteID] = d
	}

	a, b := byID[siteA], byID[siteB]
	if a.Name != "Laggard Tablet" || b.Name != "" {
		t.Errorf("names: a=%q b=%q, want (Laggard Tablet, \"\")", a.Name, b.Name)
	}
	// siteA's own 3 ops are excluded from its pending count; siteB has pulled past them.
	if a.PendingOps != 0 || b.PendingOps != 0 {
		t.Errorf("pending: a=%d b=%d, want (0, 0) — own ops never count as pending", a.PendingOps, b.PendingOps)
	}
	if a.Stale || b.Stale {
		t.Errorf("freshly seen devices flagged stale: a=%v b=%v", a.Stale, b.Stale)
	}
	// siteA (pull 0) is the active laggard holding the watermark below siteB (pull 3).
	if !a.PinsWatermark {
		t.Error("siteA should pin the watermark (active, strictly behind siteB)")
	}
	if b.PinsWatermark {
		t.Error("siteB is fully caught up; it must not be flagged as pinning")
	}
	// The all-zero test site_ids carry a zero ULID timestamp.
	if a.FirstSeenMs != 0 {
		t.Errorf("FirstSeenMs for zero-ULID = %d, want 0", a.FirstSeenMs)
	}
	if a.LastSeenMs == 0 || b.LastSeenMs == 0 {
		t.Error("LastSeenMs should be populated from updated_at")
	}
	if a.AckedOpSeq != 3 {
		t.Errorf("siteA AckedOpSeq = %d, want 3", a.AckedOpSeq)
	}

	// Once siteA goes stale it stops pinning, and nothing else pins (siteB is alone at the top).
	devs, err = s.ListDevices(ctx, now+horizon*2, horizon)
	if err != nil {
		t.Fatalf("list 2: %v", err)
	}
	for _, d := range devs {
		if !d.Stale {
			t.Errorf("%s should be stale at now+2*horizon", d.SiteID)
		}
		if d.PinsWatermark {
			t.Errorf("%s: a stale device must not pin the watermark", d.SiteID)
		}
	}
}

// TestSetDeviceLabel_SurvivesSync is the whole reason operator_label is its own
// column. device_name is refreshed from the request envelope on every sync, so
// a hand-set name stored there would be silently overwritten the moment the
// (still-owed) client half starts sending one.
func TestSetDeviceLabel_SurvivesSync(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.RecordCursor(ctx, siteA, 1, "sync-v1-client"); err != nil {
		t.Fatalf("record: %v", err)
	}
	found, err := s.SetDeviceLabel(ctx, siteA, "Boox Go 10.3 II")
	if err != nil || !found {
		t.Fatalf("SetDeviceLabel = (%v, %v), want (true, nil)", found, err)
	}

	// The device syncs again, reporting a different name of its own.
	if err := s.RecordCursor(ctx, siteA, 2, "Renamed By Client"); err != nil {
		t.Fatalf("record 2: %v", err)
	}

	devs, err := s.ListDevices(ctx, time.Now().UnixMilli(), 1000*60)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("got %d devices, want 1", len(devs))
	}
	if devs[0].Label != "Boox Go 10.3 II" {
		t.Errorf("label = %q after a sync, want it untouched by RecordCursor", devs[0].Label)
	}
	if devs[0].Name != "Renamed By Client" {
		t.Errorf("device_name = %q, want the wire value still to land in its own field", devs[0].Name)
	}
}

func TestSetDeviceLabel_ClearAndUnknownDevice(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.RecordCursor(ctx, siteA, 1, "Reported Name"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := s.SetDeviceLabel(ctx, siteA, "My Tablet"); err != nil {
		t.Fatalf("set: %v", err)
	}

	// Empty clears it — display falls back to the device-reported name.
	if found, err := s.SetDeviceLabel(ctx, siteA, ""); err != nil || !found {
		t.Fatalf("clear = (%v, %v), want (true, nil)", found, err)
	}
	devs, _ := s.ListDevices(ctx, time.Now().UnixMilli(), 1000*60)
	if len(devs) != 1 || devs[0].Label != "" {
		t.Errorf("label after clear = %+v, want empty", devs)
	}

	// An unregistered site is a miss, never an insert: a fabricated cursor row
	// would sit at seq 0 and pin the relay log against compaction forever.
	found, err := s.SetDeviceLabel(ctx, siteB, "Ghost")
	if err != nil {
		t.Fatalf("set unknown: %v", err)
	}
	if found {
		t.Error("labeling an unregistered site_id reported success")
	}
	if _, _, exists := cursorRow(t, s, siteB); exists {
		t.Error("labeling an unregistered site_id created a cursor row")
	}
}

func TestListDevices_SoleDeviceNeverPins(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.RecordCursor(ctx, siteA, 5, "Only Tablet"); err != nil {
		t.Fatalf("cursor: %v", err)
	}
	devs, err := s.ListDevices(ctx, time.Now().UnixMilli(), 1000*60)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(devs) != 1 || devs[0].PinsWatermark {
		t.Errorf("a sole active device must not show as pinning: %+v", devs)
	}
}

func TestDeleteDevice(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.RecordCursor(ctx, siteA, 1, "Tablet"); err != nil {
		t.Fatalf("cursor: %v", err)
	}

	found, err := s.DeleteDevice(ctx, siteA)
	if err != nil || !found {
		t.Fatalf("delete existing: (found=%v, err=%v), want (true, nil)", found, err)
	}
	if _, _, exists := cursorRow(t, s, siteA); exists {
		t.Error("cursor row survived DeleteDevice")
	}

	found, err = s.DeleteDevice(ctx, siteA)
	if err != nil || found {
		t.Errorf("delete missing: (found=%v, err=%v), want (false, nil)", found, err)
	}
}

// TestAdvanceAccepted_ReseedAfterPrune is the prune-safety regression: deleting a
// device's cursor row (device management) erases its persisted acked_op_seq, and
// compaction may have reclaimed sync_ops rows below its high-water. The contiguous
// walk must reseed from MAX(op_seq) rather than wedging at the first hole — the
// client has already discarded acked outbox ops, so it can never refill the gap.
func TestAdvanceAccepted_ReseedAfterPrune(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Device syncs three versions of the same notebook row.
	r, err := s.ApplyBatch(ctx, siteA, []Op{
		notebookOp(siteA, 1, 1000, "v1", nil),
		notebookOp(siteA, 2, 2000, "v2", nil),
		notebookOp(siteA, 3, 3000, "v3", nil),
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if r.AcceptedThrough != 3 {
		t.Fatalf("setup: accepted_through = %d, want 3", r.AcceptedThrough)
	}

	// Prune the device, and simulate compaction rule 1 having collapsed the two
	// superseded versions out of the relay log (op_seq 1 and 2 are now holes).
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sync_cursors WHERE site_id = ?`, siteA); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM sync_ops WHERE site_id = ? AND op_seq IN (1, 2)`, siteA); err != nil {
		t.Fatalf("simulate compaction: %v", err)
	}

	// The device re-registers with an empty batch (its outbox is long gone).
	r, err = s.ApplyBatch(ctx, siteA, nil)
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if r.AcceptedThrough != 3 {
		t.Errorf("after prune + compaction holes: accepted_through = %d, want 3 (wedged below the device's high-water)", r.AcceptedThrough)
	}

	// And its next ops advance normally from there.
	r, err = s.ApplyBatch(ctx, siteA, []Op{notebookOp(siteA, 4, 4000, "v4", nil)})
	if err != nil {
		t.Fatalf("post-rejoin apply: %v", err)
	}
	if r.AcceptedThrough != 4 {
		t.Errorf("post-rejoin: accepted_through = %d, want 4", r.AcceptedThrough)
	}
}
