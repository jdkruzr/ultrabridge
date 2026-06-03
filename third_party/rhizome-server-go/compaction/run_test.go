package compaction

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/jdkruzr/rhizome/server-go/registry"
	"github.com/jdkruzr/rhizome/server-go/syncstore"
)

var fnKnownCols = registry.ForestNote().KnownCols()

// strokeTombstones is the table→tombstone-column map derived from the ForestNote registry.
var strokeTombstones = func() map[string]string {
	out := map[string]string{}
	for _, t := range registry.ForestNote().Tables {
		if t.Tombstone != "" {
			out[t.Name] = t.Tombstone
		}
	}
	return out
}()

func pad26(prefix string) string {
	const fill = "0000000000000000000000000000"
	return (prefix + fill)[:26]
}

// strokeOp is a fully-populated stroke op; deleted is the raw JSON for deleted_at (`null` = live).
func strokeOp(pk, site string, opSeq, opTs int64, deleted string) syncstore.Op {
	return syncstore.Op{
		Table: "stroke", PK: pk, SiteID: site, OpSeq: opSeq, OpTs: opTs,
		Cols: map[string]json.RawMessage{
			"page_id":       json.RawMessage(`"` + pad26("PAGE") + `"`),
			"color":         json.RawMessage(`4278190080`),
			"pen_width_min": json.RawMessage(`2`),
			"pen_width_max": json.RawMessage(`8`),
			"points":        json.RawMessage(`"AAEC"`),
			"z":             json.RawMessage(`5`),
			"created_at":    json.RawMessage(`100`),
			"deleted_at":    json.RawMessage(deleted),
		},
	}
}

func TestRunCollapsesChurnInStore(t *testing.T) {
	s := syncstore.NewStore(fnKnownCols)
	site, puller := pad26("AAAA"), pad26("BBBB")
	pk := pad26("S1")
	// Three edits of the same row → three log entries at seq 1,2,3.
	s.ApplyBatch(site, []syncstore.Op{strokeOp(pk, site, 1, 100, "null")})
	s.ApplyBatch(site, []syncstore.Op{strokeOp(pk, site, 2, 200, "null")})
	s.ApplyBatch(site, []syncstore.Op{strokeOp(pk, site, 3, 300, "null")})

	stats := Run(s, strokeTombstones, 0)
	if stats.CollapsedSuperseded != 2 {
		t.Fatalf("collapsed = %d, want 2", stats.CollapsedSuperseded)
	}
	// A fresh cursor=0 replica still reconstructs exact state: one op, the latest version.
	ops, _, _ := s.OpsSince(0, puller, 100)
	if len(ops) != 1 || ops[0].OpTs != 300 {
		t.Fatalf("cursor=0 re-pull = %+v, want single op_ts=300 winner", ops)
	}
	if s.LastSeq() != 3 {
		t.Fatalf("LastSeq = %d, want 3 (no renumbering of the high-water)", s.LastSeq())
	}
}

func TestRunKeepsTombstoneBelowWatermark_NoZombie(t *testing.T) {
	s := syncstore.NewStore(fnKnownCols)
	site, puller := pad26("AAAA"), pad26("BBBB")
	pk := pad26("S1")
	s.ApplyBatch(site, []syncstore.Op{strokeOp(pk, site, 1, 100, "null")})          // seq 1 live
	s.ApplyBatch(site, []syncstore.Op{strokeOp(pk, site, 2, 200, "1700000000000")}) // seq 2 tombstone

	// Watermark 1: a replica is still behind the death → tombstone MUST survive so the
	// fresh puller learns of the delete (no resurrection).
	stats := Run(s, strokeTombstones, 1)
	if stats.PurgedTombstones != 0 {
		t.Fatalf("purged = %d, want 0 (below watermark)", stats.PurgedTombstones)
	}
	ops, _, _ := s.OpsSince(0, puller, 100)
	if len(ops) != 1 {
		t.Fatalf("tombstone must remain pullable; got %d ops", len(ops))
	}
}

func TestRunPurgesTombstoneAtWatermark(t *testing.T) {
	s := syncstore.NewStore(fnKnownCols)
	site, puller := pad26("AAAA"), pad26("BBBB")
	pk := pad26("S1")
	s.ApplyBatch(site, []syncstore.Op{strokeOp(pk, site, 1, 100, "null")})
	s.ApplyBatch(site, []syncstore.Op{strokeOp(pk, site, 2, 200, "1700000000000")})

	stats := Run(s, strokeTombstones, 2) // every site has pulled past seq 2
	if stats.PurgedTombstones != 1 || stats.CollapsedSuperseded != 1 {
		t.Fatalf("stats = %+v, want 1 purged + 1 collapsed", stats)
	}
	ops, _, _ := s.OpsSince(0, puller, 100)
	if len(ops) != 0 {
		t.Fatalf("purged tombstone + collapsed live → empty log; got %d ops", len(ops))
	}
}

func TestRunIsIdempotent(t *testing.T) {
	s := syncstore.NewStore(fnKnownCols)
	site := pad26("AAAA")
	pk := pad26("S1")
	s.ApplyBatch(site, []syncstore.Op{strokeOp(pk, site, 1, 100, "null")})
	s.ApplyBatch(site, []syncstore.Op{strokeOp(pk, site, 2, 200, "null")})

	Run(s, strokeTombstones, 0)
	second := Run(s, strokeTombstones, 0)
	if second != (Stats{}) {
		t.Fatalf("second sweep must reclaim nothing; got %+v", second)
	}
}

func TestPurgedOpStaysDedupedNotReAppended(t *testing.T) {
	s := syncstore.NewStore(fnKnownCols)
	site, puller := pad26("AAAA"), pad26("BBBB")
	pk := pad26("S1")
	tomb := strokeOp(pk, site, 2, 200, "1700000000000")
	s.ApplyBatch(site, []syncstore.Op{strokeOp(pk, site, 1, 100, "null")})
	s.ApplyBatch(site, []syncstore.Op{tomb})

	Run(s, strokeTombstones, 2) // purges the tombstone
	// Re-delivering the purged op must NOT resurrect it into the log (seen is retained).
	s.ApplyBatch(site, []syncstore.Op{tomb})
	ops, _, _ := s.OpsSince(0, puller, 100)
	if len(ops) != 0 {
		t.Fatalf("re-delivered purged op must stay deduped; got %d ops", len(ops))
	}
}

func TestConcurrentAppendDuringCompactionLosesNothing(t *testing.T) {
	s := syncstore.NewStore(fnKnownCols)
	puller := pad26("ZZZZ")
	const sites = 8
	var wg sync.WaitGroup
	for i := 0; i < sites; i++ {
		site := pad26(string(rune('A'+i)) + string(rune('A'+i)))
		wg.Add(1)
		go func(site string) {
			defer wg.Done()
			for j := int64(1); j <= 5; j++ {
				pk := pad26(site[:2] + string(rune('0'+j)))
				s.ApplyBatch(site, []syncstore.Op{strokeOp(pk, site, j, 100+j, "null")})
			}
		}(site)
	}
	// Hammer compaction concurrently with the appends.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for k := 0; k < 20; k++ {
			Run(s, strokeTombstones, 0)
		}
	}()
	wg.Wait()
	Run(s, strokeTombstones, 0) // final collapse

	// Every distinct (site,pk) is a distinct live row → all must survive exactly once.
	ops, _, _ := s.OpsSince(0, puller, 10_000)
	seen := map[syncstore.TablePK]bool{}
	for _, op := range ops {
		if seen[op.Key()] {
			t.Fatalf("duplicate surviving op for %v", op.Key())
		}
		seen[op.Key()] = true
	}
	if len(seen) != sites*5 {
		t.Fatalf("surviving live rows = %d, want %d (nothing lost to a concurrent sweep)", len(seen), sites*5)
	}
}
