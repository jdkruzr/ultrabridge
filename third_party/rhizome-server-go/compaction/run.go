package compaction

import "github.com/jdkruzr/rhizome/server-go/syncstore"

// Run compacts a live relay log in place. It applies Sweep (collapse-superseded + watermark-gated
// tombstone purge) to store's log under store's own lock, so it is atomic and safe to run
// concurrently with ApplyBatch/OpsSince. It returns the reclamation stats for logging.
//
// watermark is min(last_pull_seq) over the non-evicted sites (compute it with Watermark);
// tombstoneCols maps a table to its tombstone column (derive it from the registry's Table.Tombstone).
func Run(store *syncstore.Store, tombstoneCols map[string]string, watermark int64) Stats {
	var stats Stats
	store.CompactLog(func(snap []syncstore.SeqOp) []syncstore.SeqOp {
		entries := make([]Entry, len(snap))
		for i, so := range snap {
			entries[i] = Entry{Seq: so.Seq, Op: so.Op}
		}
		kept, st := Sweep(entries, tombstoneCols, watermark)
		stats = st
		out := make([]syncstore.SeqOp, len(kept))
		for i, e := range kept {
			out[i] = syncstore.SeqOp{Seq: e.Seq, Op: e.Op}
		}
		return out
	})
	return stats
}
