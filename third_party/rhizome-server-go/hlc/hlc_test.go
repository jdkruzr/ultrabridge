package hlc

import "testing"

// wall is a mutable injectable clock.
type wall struct{ now int64 }

func (w *wall) fn() int64 { return w.now }

func TestLocalTracksWallWhenAdvancing(t *testing.T) {
	w := &wall{now: 1000}
	c := New(0, w.fn)
	if got := c.LocalEvent(); got != 1000 {
		t.Fatalf("got %d, want 1000", got)
	}
	w.now = 2000
	if got := c.LocalEvent(); got != 2000 {
		t.Fatalf("got %d, want 2000 (tracks advancing wall)", got)
	}
}

func TestLocalTicksWhenSameMillisecond(t *testing.T) {
	w := &wall{now: 1000}
	c := New(0, w.fn)
	c.LocalEvent()
	if got := c.LocalEvent(); got != 1001 {
		t.Fatalf("got %d, want 1001 (same-ms tick)", got)
	}
}

func TestLocalIsMonotonicWhenClockGoesBackward(t *testing.T) {
	w := &wall{now: 5000}
	c := New(0, w.fn)
	c.LocalEvent()
	w.now = 3000
	if got := c.LocalEvent(); got != 5001 {
		t.Fatalf("got %d, want 5001 (never goes backward)", got)
	}
}

func TestReceiveJumpsPastRemote(t *testing.T) {
	w := &wall{now: 1000}
	c := New(0, w.fn)
	c.LocalEvent()
	if got := c.ReceiveEvent(5000); got != 5001 {
		t.Fatalf("got %d, want 5001 (strictly past remote)", got)
	}
	if got := c.LocalEvent(); got != 5002 {
		t.Fatalf("got %d, want 5002 (local sorts after absorbed remote)", got)
	}
}

func TestReceiveTracksWallWhenItLeadsBoth(t *testing.T) {
	w := &wall{now: 9000}
	c := New(1000, w.fn)
	if got := c.ReceiveEvent(2000); got != 9000 {
		t.Fatalf("got %d, want 9000", got)
	}
}

func TestCausalInversionIsPrevented(t *testing.T) {
	aw := &wall{now: 10000}
	bw := &wall{now: 3000} // slow
	a := New(0, aw.fn)
	b := New(0, bw.fn)

	tA := a.LocalEvent()
	b.ReceiveEvent(tA)
	tB := b.LocalEvent()

	if tB <= tA {
		t.Fatalf("B's causally-later edit must sort after A's: tB=%d tA=%d", tB, tA)
	}
}

func TestLegacyRawWallTsInteroperates(t *testing.T) {
	w := &wall{now: 1_700_000_000_000}
	c := New(0, w.fn)
	if got := c.ReceiveEvent(1_700_000_000_005); got != 1_700_000_000_006 {
		t.Fatalf("got %d, want 1700000000006 (legacy ms is just another op_ts)", got)
	}
}
