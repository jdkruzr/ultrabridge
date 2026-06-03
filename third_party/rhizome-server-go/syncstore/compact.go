package syncstore

// SeqOp is a sequenced op in the relay log: the global seq paired with the op authored at it.
// Exported so the compaction package can transform the log atomically without reaching into Store
// internals (Store can't import compaction — that would be an import cycle).
type SeqOp struct {
	Seq int64
	Op  Op
}

// CompactLog atomically rewrites the relay log. It snapshots the current log, calls transform to
// produce the surviving entries, and installs them — all while holding the store lock, so it is
// safe to run concurrently with ApplyBatch/OpsSince: an op appended just before the call is in the
// snapshot; one appended just after lands cleanly after the swap.
//
// The high-water seq (for assigning new ops), the seen-set (dedup), and per-site acked are all
// left untouched: compaction never renumbers (spec/compaction.md rule 3) and keeps seen so a
// re-delivered, already-compacted op is still deduped rather than resurrected into the log.
// transform MUST return a seq-ordered subset of the snapshot.
func (s *Store) CompactLog(transform func(snapshot []SeqOp) []SeqOp) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := make([]SeqOp, len(s.log))
	for i, rec := range s.log {
		snap[i] = SeqOp{Seq: rec.seq, Op: rec.op}
	}
	kept := transform(snap)
	newLog := make([]logRec, len(kept))
	for i, e := range kept {
		newLog[i] = logRec{seq: e.Seq, op: e.Op}
	}
	s.log = newLog
}
