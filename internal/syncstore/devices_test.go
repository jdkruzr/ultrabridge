package syncstore

import (
	"context"
	"testing"
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
