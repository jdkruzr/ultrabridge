package digesttomb

import (
	"context"
	"testing"

	"github.com/sysop/ultrabridge/internal/notedb"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	db, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return New(db)
}

// TestEnqueueDrainAck covers the lifecycle: enqueue → Pending drains + marks
// sent → Ack deletes the sent rows → Pending is then empty.
func TestEnqueueDrainAck(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.Enqueue(ctx, 42, 7, "2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(ctx, 42, 8, "1"); err != nil {
		t.Fatal(err)
	}

	got, err := s.Pending(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Pending = %d rows, want 2: %+v", len(got), got)
	}
	// Verify the ids/dataTypes came back.
	seen := map[int64]string{}
	for _, tb := range got {
		seen[tb.DigestID] = tb.DataType
	}
	if seen[7] != "2" || seen[8] != "1" {
		t.Errorf("drained payloads wrong: %+v", seen)
	}

	// Ack removes the drained (sent) rows.
	if err := s.Ack(ctx, 42); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Pending(ctx, 42)
	if len(after) != 0 {
		t.Errorf("after Ack, Pending = %d, want 0: %+v", len(after), after)
	}
}

// TestPendingScopedByUser: Pending only drains the requested user's rows.
func TestPendingScopedByUser(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_ = s.Enqueue(ctx, 1, 100, "2")
	_ = s.Enqueue(ctx, 2, 200, "2")

	got, _ := s.Pending(ctx, 1)
	if len(got) != 1 || got[0].DigestID != 100 {
		t.Fatalf("user 1 drain = %+v, want just digest 100", got)
	}
	// User 2 untouched.
	got2, _ := s.Pending(ctx, 2)
	if len(got2) != 1 || got2[0].DigestID != 200 {
		t.Fatalf("user 2 drain = %+v, want just digest 200", got2)
	}
}

// TestEnqueueIdempotent: enqueuing the same (user, digest) twice keeps one row.
func TestEnqueueIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_ = s.Enqueue(ctx, 5, 9, "2")
	_ = s.Enqueue(ctx, 5, 9, "2")
	got, _ := s.Pending(ctx, 5)
	if len(got) != 1 {
		t.Fatalf("idempotent enqueue = %d rows, want 1", len(got))
	}
}

// TestAckLeavesUnsent: a row enqueued after a drain (still unsent) survives Ack.
func TestAckLeavesUnsent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_ = s.Enqueue(ctx, 3, 1, "2")
	_, _ = s.Pending(ctx, 3)      // marks digest 1 sent
	_ = s.Enqueue(ctx, 3, 2, "2") // new, unsent

	if err := s.Ack(ctx, 3); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Pending(ctx, 3)
	if len(got) != 1 || got[0].DigestID != 2 {
		t.Errorf("Ack should keep the unsent row (digest 2), got %+v", got)
	}
}

// TestSweep deletes rows older than the cutoff.
func TestSweep(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	s.now = func() int64 { return 1000 }
	_ = s.Enqueue(ctx, 1, 1, "2")
	s.now = func() int64 { return 50000 }
	_ = s.Enqueue(ctx, 1, 2, "2")

	n, err := s.Sweep(ctx, 40000) // delete created_at < 40000
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("Sweep removed %d, want 1", n)
	}
	got, _ := s.Pending(ctx, 1)
	if len(got) != 1 || got[0].DigestID != 2 {
		t.Errorf("sweep should leave the newer row, got %+v", got)
	}
}
