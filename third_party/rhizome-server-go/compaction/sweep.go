// Package compaction reclaims the relay log per spec/compaction.md. It is server-side only: no
// client change, no protocol change. The log grows by one full-row-snapshot op per edit forever;
// under LWW, superseded ops are dead weight. Sweep is the pure decision (conformance-tested via the
// `compaction` vectors); Run applies it atomically to a live *syncstore.Store.
package compaction

import (
	"bytes"

	"github.com/jdkruzr/rhizome/server-go/syncstore"
)

// Entry is a sequenced op in the relay log: the global seq paired with the op authored at it.
type Entry struct {
	Seq int64
	Op  syncstore.Op
}

// Stats reports what a Sweep reclaimed, for logging (silent caps are forbidden — see the spec).
type Stats struct {
	CollapsedSuperseded int // older versions dropped because a greater-LWW-key op exists
	PurgedTombstones    int // surviving tombstones reclaimed at/below the watermark
}

// Sweep applies the three compaction rules to a seq-ordered log and returns the entries to keep
// (still in seq order, original seqs preserved — rule 3, no renumbering) plus reclamation stats.
//
//	rule 1 — collapse superseded: for each (table, pk) keep only the LWW winner, at its own seq.
//	rule 2 — reclaim tombstones: a surviving op whose tombstone column is non-null is purged
//	         entirely, but ONLY if its seq <= watermark (every known site has pulled past it);
//	         above the watermark it is kept, or a stale replica resurrects the row (zombie).
//	rule 3 — no seq renumbering: surviving entries keep their original seq; holes are fine.
//
// tombstoneCols maps a table to its tombstone column name; a table absent from the map (or a row
// whose tombstone column is missing/null) is treated as live. Sweep is pure and idempotent:
// Sweep(Sweep(log)) == Sweep(log).
func Sweep(entries []Entry, tombstoneCols map[string]string, watermark int64) (kept []Entry, stats Stats) {
	// Winner per (table, pk) by LWW key; remember the index of that winning entry.
	winnerIdx := make(map[syncstore.TablePK]int)
	for i, e := range entries {
		k := e.Op.Key()
		if w, ok := winnerIdx[k]; !ok || syncstore.Less(entries[w].Op, e.Op) {
			winnerIdx[k] = i
		}
	}

	// A row contributes either its winner (collapse), or nothing (purged tombstone). Everything
	// not the winner of its key is a superseded version.
	winners := make(map[int]struct{}, len(winnerIdx))
	for _, i := range winnerIdx {
		winners[i] = struct{}{}
	}

	for i, e := range entries {
		if _, isWinner := winners[i]; !isWinner {
			stats.CollapsedSuperseded++
			continue
		}
		if isTombstone(e.Op, tombstoneCols) && e.Seq <= watermark {
			stats.PurgedTombstones++
			continue
		}
		kept = append(kept, e)
	}
	return kept, stats
}

// isTombstone reports whether op's designated tombstone column is present and non-null.
func isTombstone(op syncstore.Op, tombstoneCols map[string]string) bool {
	col, ok := tombstoneCols[op.Table]
	if !ok {
		return false
	}
	raw, present := op.Cols[col]
	if !present {
		return false
	}
	return !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
