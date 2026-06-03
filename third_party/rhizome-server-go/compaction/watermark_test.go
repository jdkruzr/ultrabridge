package compaction

import (
	"reflect"
	"testing"
)

func TestWatermarkIsMinOfActiveSites(t *testing.T) {
	sites := map[string]Site{
		"A": {LastPullSeq: 100, LastSeenUnixMs: 1000},
		"B": {LastPullSeq: 50, LastSeenUnixMs: 1000},
	}
	wm, evicted := Watermark(sites, 1000, 10_000)
	if wm != 50 {
		t.Fatalf("watermark = %d, want 50 (the trailing site pins it)", wm)
	}
	if len(evicted) != 0 {
		t.Fatalf("no site is stale; evicted = %v", evicted)
	}
}

func TestStaleSiteEvictedAndExcludedFromWatermark(t *testing.T) {
	// B was last seen 20s ago with a 10s horizon → evicted, so A (100) sets the watermark
	// instead of B's stale cursor (5) pinning it forever.
	sites := map[string]Site{
		"A": {LastPullSeq: 100, LastSeenUnixMs: 100_000},
		"B": {LastPullSeq: 5, LastSeenUnixMs: 80_000},
	}
	wm, evicted := Watermark(sites, 100_000, 10_000)
	if wm != 100 {
		t.Fatalf("watermark = %d, want 100 (stale B excluded)", wm)
	}
	if !reflect.DeepEqual(evicted, []string{"B"}) {
		t.Fatalf("evicted = %v, want [B]", evicted)
	}
}

func TestEvictionBoundaryIsStrict(t *testing.T) {
	// Exactly at the horizon (now - lastSeen == horizon) is NOT yet stale.
	sites := map[string]Site{
		"A": {LastPullSeq: 7, LastSeenUnixMs: 90_000},
	}
	wm, evicted := Watermark(sites, 100_000, 10_000)
	if wm != 7 || len(evicted) != 0 {
		t.Fatalf("at-horizon site must stay active; wm=%d evicted=%v", wm, evicted)
	}
}

func TestAllSitesStaleWatermarkZero(t *testing.T) {
	// Nobody active → no reader to protect, but be conservative: watermark 0 purges no
	// tombstone. All stale sites are reported (sorted) so the caller can log the eviction.
	sites := map[string]Site{
		"B": {LastPullSeq: 5, LastSeenUnixMs: 0},
		"A": {LastPullSeq: 9, LastSeenUnixMs: 0},
	}
	wm, evicted := Watermark(sites, 100_000, 10_000)
	if wm != 0 {
		t.Fatalf("watermark = %d, want 0 when no active site", wm)
	}
	if !reflect.DeepEqual(evicted, []string{"A", "B"}) {
		t.Fatalf("evicted = %v, want sorted [A B]", evicted)
	}
}

func TestEmptySites(t *testing.T) {
	wm, evicted := Watermark(map[string]Site{}, 100_000, 10_000)
	if wm != 0 || len(evicted) != 0 {
		t.Fatalf("no sites → wm 0, no evictions; got wm=%d evicted=%v", wm, evicted)
	}
}
