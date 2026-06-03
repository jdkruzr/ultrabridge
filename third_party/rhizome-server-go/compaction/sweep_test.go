package compaction

import (
	"encoding/json"
	"testing"

	"github.com/jdkruzr/rhizome/server-go/syncstore"
)

// tombstoneCols for a single "note" table whose soft-delete column is deleted_at.
var noteTombstones = map[string]string{"note": "deleted_at"}

// noteOp builds a note op. deleted is the raw JSON for deleted_at (`null` = live).
func noteOp(pk, site string, opSeq, opTs int64, deleted string) syncstore.Op {
	return syncstore.Op{
		Table: "note", PK: pk, SiteID: site, OpSeq: opSeq, OpTs: opTs,
		Cols: map[string]json.RawMessage{
			"text":       json.RawMessage(`"t"`),
			"deleted_at": json.RawMessage(deleted),
		},
	}
}

// keptKeys returns (seq, pk) pairs of kept entries in order, for compact assertions.
func keptSeqs(entries []Entry) []int64 {
	out := make([]int64, len(entries))
	for i, e := range entries {
		out[i] = e.Seq
	}
	return out
}

func TestCollapseSupersededKeepsLatestAtOriginalSeq(t *testing.T) {
	// Three live versions of the same row; LWW winner is the greatest op_ts (seq 3).
	log := []Entry{
		{Seq: 1, Op: noteOp("R1", "A", 1, 100, "null")},
		{Seq: 2, Op: noteOp("R1", "A", 2, 200, "null")},
		{Seq: 3, Op: noteOp("R1", "A", 3, 300, "null")},
	}
	kept, stats := Sweep(log, noteTombstones, 0)
	if got := keptSeqs(kept); len(got) != 1 || got[0] != 3 {
		t.Fatalf("kept seqs = %v, want [3] (winner at its original seq)", got)
	}
	if stats.CollapsedSuperseded != 2 {
		t.Fatalf("collapsed = %d, want 2", stats.CollapsedSuperseded)
	}
}

func TestDistinctRowsAllKept(t *testing.T) {
	log := []Entry{
		{Seq: 1, Op: noteOp("R1", "A", 1, 100, "null")},
		{Seq: 2, Op: noteOp("R2", "A", 2, 100, "null")},
	}
	kept, stats := Sweep(log, noteTombstones, 0)
	if len(kept) != 2 {
		t.Fatalf("kept %d distinct rows, want 2", len(kept))
	}
	if stats.CollapsedSuperseded != 0 {
		t.Fatalf("collapsed = %d, want 0", stats.CollapsedSuperseded)
	}
}

func TestWinnerChosenByLwwKeyNotSeq(t *testing.T) {
	// A later-sequenced op carries an EARLIER op_ts (e.g. relayed out of clock order).
	// LWW keeps the greater op_ts (seq 1), at its original seq — not the latest seq.
	log := []Entry{
		{Seq: 1, Op: noteOp("R1", "A", 1, 300, "null")},
		{Seq: 2, Op: noteOp("R1", "B", 1, 100, "null")},
	}
	kept, _ := Sweep(log, noteTombstones, 0)
	if got := keptSeqs(kept); len(got) != 1 || got[0] != 1 {
		t.Fatalf("kept seqs = %v, want [1] (greater LWW key wins, kept at its seq)", got)
	}
}

func TestTombstonePurgedAtOrBelowWatermark(t *testing.T) {
	// The surviving op for R1 is a tombstone at seq 5; every site has pulled past 5.
	log := []Entry{
		{Seq: 4, Op: noteOp("R1", "A", 1, 100, "null")},
		{Seq: 5, Op: noteOp("R1", "A", 2, 200, "1700000000000")}, // deleted_at non-null
	}
	kept, stats := Sweep(log, noteTombstones, 5)
	if len(kept) != 0 {
		t.Fatalf("tombstone at/below watermark must be purged; kept %v", keptSeqs(kept))
	}
	if stats.PurgedTombstones != 1 {
		t.Fatalf("purged = %d, want 1", stats.PurgedTombstones)
	}
}

func TestTombstoneKeptAboveWatermark_NoZombie(t *testing.T) {
	// Same tombstone at seq 5, but a site is still behind (watermark 4): MUST keep it,
	// or that stale replica resurrects the row.
	log := []Entry{
		{Seq: 5, Op: noteOp("R1", "A", 2, 200, "1700000000000")},
	}
	kept, stats := Sweep(log, noteTombstones, 4)
	if got := keptSeqs(kept); len(got) != 1 || got[0] != 5 {
		t.Fatalf("tombstone above watermark must be kept (no zombie); kept %v", got)
	}
	if stats.PurgedTombstones != 0 {
		t.Fatalf("purged = %d, want 0", stats.PurgedTombstones)
	}
}

func TestLiveWinnerNeverPurged(t *testing.T) {
	// A live winner is current state — never purged no matter how high the watermark.
	log := []Entry{
		{Seq: 2, Op: noteOp("R1", "A", 1, 100, "null")},
	}
	kept, _ := Sweep(log, noteTombstones, 1000)
	if got := keptSeqs(kept); len(got) != 1 || got[0] != 2 {
		t.Fatalf("live winner must survive; kept %v", got)
	}
}

func TestEmptyLog(t *testing.T) {
	kept, stats := Sweep(nil, noteTombstones, 0)
	if len(kept) != 0 || stats != (Stats{}) {
		t.Fatalf("empty log → empty result; got %v %+v", keptSeqs(kept), stats)
	}
}
