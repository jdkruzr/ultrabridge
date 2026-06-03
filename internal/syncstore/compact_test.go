package syncstore

import (
	"context"
	"testing"

	"github.com/jdkruzr/rhizome/server-go/compaction"
)

// countOps returns the number of rows currently in the durable sync_ops relay log.
func countOps(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sync_ops`).Scan(&n); err != nil {
		t.Fatalf("count sync_ops: %v", err)
	}
	return n
}

// TestCompact_CollapsesSupersededChurn: three edits of the same row collapse to the single LWW
// winner, the log shrinks, and a fresh cursor=0 pull still reconstructs the latest state. The global
// high-water (sync_seq.last_seq) is NOT renumbered.
func TestCompact_CollapsesSupersededChurn(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.ApplyBatch(ctx, siteA, []Op{
		notebookOp(siteA, 1, 1000, "v1", nil),
		notebookOp(siteA, 2, 2000, "v2", nil),
		notebookOp(siteA, 3, 3000, "v3", nil),
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := countOps(t, s); got != 3 {
		t.Fatalf("pre-compact sync_ops = %d, want 3", got)
	}

	stats, err := s.Compact(ctx, TombstoneCols(), 0) // watermark 0 → collapse only, keep tombstones
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if stats.CollapsedSuperseded != 2 || stats.PurgedTombstones != 0 {
		t.Fatalf("stats = %+v, want 2 collapsed / 0 purged", stats)
	}
	if got := countOps(t, s); got != 1 {
		t.Fatalf("post-compact sync_ops = %d, want 1", got)
	}

	// A fresh replica (cursor=0, different site) still reconstructs the exact current state.
	ops, _, _, err := s.OpsSince(ctx, 0, siteB, 100)
	if err != nil {
		t.Fatalf("opsSince: %v", err)
	}
	if len(ops) != 1 || ops[0].Cols["name"] != "v3" {
		t.Fatalf("cursor=0 re-pull = %+v, want single op name=v3", ops)
	}

	// High-water is untouched (no renumbering): the next op must still get seq 4.
	last, err := s.LastSeq(ctx)
	if err != nil {
		t.Fatalf("lastSeq: %v", err)
	}
	if last != 3 {
		t.Fatalf("LastSeq = %d, want 3 (seq never renumbered)", last)
	}
}

// TestCompact_KeepsTombstoneAboveWatermark_NoZombie: a delete whose seq is above the watermark MUST
// survive the sweep, or a replica still behind it would never learn of the delete (resurrection).
func TestCompact_KeepsTombstoneAboveWatermark_NoZombie(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.ApplyBatch(ctx, siteA, []Op{
		notebookOp(siteA, 1, 1000, "live", nil),           // seq 1
		notebookOp(siteA, 2, 2000, "gone", float64(2500)), // seq 2 tombstone
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	stats, err := s.Compact(ctx, TombstoneCols(), 1) // watermark 1: a replica is still behind the delete
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if stats.PurgedTombstones != 0 {
		t.Fatalf("purged = %d, want 0 (tombstone above watermark)", stats.PurgedTombstones)
	}
	// The live seq-1 version is superseded by the tombstone and collapses; the tombstone remains.
	if stats.CollapsedSuperseded != 1 {
		t.Fatalf("collapsed = %d, want 1", stats.CollapsedSuperseded)
	}
	ops, _, _, err := s.OpsSince(ctx, 0, siteB, 100)
	if err != nil {
		t.Fatalf("opsSince: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("tombstone must remain pullable; got %d ops", len(ops))
	}
}

// TestCompact_PurgesTombstoneAtWatermark: once every device has pulled past the delete (seq <=
// watermark), the tombstone is reclaimed and the log empties.
func TestCompact_PurgesTombstoneAtWatermark(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.ApplyBatch(ctx, siteA, []Op{
		notebookOp(siteA, 1, 1000, "live", nil),
		notebookOp(siteA, 2, 2000, "gone", float64(2500)),
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	stats, err := s.Compact(ctx, TombstoneCols(), 2) // every site has pulled past seq 2
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if stats.PurgedTombstones != 1 || stats.CollapsedSuperseded != 1 {
		t.Fatalf("stats = %+v, want 1 purged / 1 collapsed", stats)
	}
	if got := countOps(t, s); got != 0 {
		t.Fatalf("post-compact sync_ops = %d, want 0", got)
	}
	ops, _, _, err := s.OpsSince(ctx, 0, siteB, 100)
	if err != nil {
		t.Fatalf("opsSince: %v", err)
	}
	if len(ops) != 0 {
		t.Fatalf("purged tombstone + collapsed live → empty log; got %d ops", len(ops))
	}
}

// TestCompact_NoOpWhenNothingToReclaim: a log with one op per distinct row and no tombstones is
// already minimal — Compact reclaims nothing and leaves the log intact.
func TestCompact_NoOpWhenNothingToReclaim(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.ApplyBatch(ctx, siteA, []Op{notebookOp(siteA, 1, 1000, "only", nil)}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	stats, err := s.Compact(ctx, TombstoneCols(), 100)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if stats != (compaction.Stats{}) {
		t.Fatalf("stats = %+v, want zero", stats)
	}
	if got := countOps(t, s); got != 1 {
		t.Fatalf("sync_ops = %d, want 1 (untouched)", got)
	}
}

// TestComputeWatermark_MinOverActiveSitesEvictsStale: the watermark is min(last_pull_seq) over
// devices seen within the horizon; a device unseen longer than the horizon is evicted from the min
// (and reported) so it cannot pin the watermark forever.
func TestComputeWatermark_MinOverActiveSitesEvictsStale(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := int64(1_000_000_000_000)

	// Two active devices (recent updated_at) at pull cursors 7 and 4, and one stale device far behind.
	mustExec(t, s, `INSERT INTO sync_cursors (site_id, last_pull_seq, updated_at) VALUES (?, ?, ?)`,
		siteA, 7, now-1000)
	mustExec(t, s, `INSERT INTO sync_cursors (site_id, last_pull_seq, updated_at) VALUES (?, ?, ?)`,
		siteB, 4, now-2000)
	stale := "0000000000000000000000STAL"
	mustExec(t, s, `INSERT INTO sync_cursors (site_id, last_pull_seq, updated_at) VALUES (?, ?, ?)`,
		stale, 0, now-1_000_000)

	horizonMs := int64(10_000) // 10s: the stale device (1000s old) is evicted, the two recent ones aren't
	wm, evicted, err := s.ComputeWatermark(ctx, now, horizonMs)
	if err != nil {
		t.Fatalf("computeWatermark: %v", err)
	}
	if wm != 4 {
		t.Fatalf("watermark = %d, want 4 (min over the two active devices)", wm)
	}
	if len(evicted) != 1 || evicted[0] != stale {
		t.Fatalf("evicted = %v, want [%s]", evicted, stale)
	}
}

// TestComputeWatermark_NoActiveSitesIsZero: with every device stale (or none known), the watermark
// is 0 so nothing is purged — there is no active reader to protect.
func TestComputeWatermark_NoActiveSitesIsZero(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := int64(1_000_000_000_000)
	mustExec(t, s, `INSERT INTO sync_cursors (site_id, last_pull_seq, updated_at) VALUES (?, ?, ?)`,
		siteA, 9, now-1_000_000)

	wm, evicted, err := s.ComputeWatermark(ctx, now, 1000)
	if err != nil {
		t.Fatalf("computeWatermark: %v", err)
	}
	if wm != 0 {
		t.Fatalf("watermark = %d, want 0 (no active site)", wm)
	}
	if len(evicted) != 1 {
		t.Fatalf("evicted = %v, want the one stale site", evicted)
	}
}

func mustExec(t *testing.T, s *Store, q string, args ...any) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
