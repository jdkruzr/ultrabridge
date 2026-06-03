package syncstore

import (
	"sync"
)

// RejectedOp identifies a permanently refused op. It is returned to the client verbatim and is
// counted as settled by accepted_through (so a poison op never wedges the high-water).
type RejectedOp struct {
	SiteID string `json:"site_id"`
	OpSeq  int64  `json:"op_seq"`
	Reason string `json:"reason"`
}

// ApplyResult reports the outcome of an ApplyBatch for the requesting device. AcceptedThrough is
// the contiguous per-site high-water (spec/protocol.md §I.6); Rejected is returned to the client.
type ApplyResult struct {
	AcceptedThrough int64
	Rejected        []RejectedOp
}

// Store is the in-memory relay: it sequences the global op log and serves ops to other sites, but
// never adjudicates conflicts (every replica runs the identical deterministic merge). It is the
// reference relay backend; a durable (e.g. SQLite) backend implements the same algorithm. Safe for
// concurrent use.
type Store struct {
	mu        sync.Mutex
	knownCols map[string][]string
	seq       int64
	log       []logRec
	seen      map[opID]struct{}
	acked     map[string]int64
}

type logRec struct {
	seq int64
	op  Op
}

type opID struct {
	siteID string
	opSeq  int64
}

// NewStore builds an empty relay validating ops against knownCols (typically registry.KnownCols()).
func NewStore(knownCols map[string][]string) *Store {
	return &Store{
		knownCols: knownCols,
		seen:      make(map[opID]struct{}),
		acked:     make(map[string]int64),
	}
}

// ApplyBatch validates, dedups, and appends each op to the relay log, then computes the requesting
// device's contiguous accepted_through. siteID is the authenticated device. A losing op is still
// appended for relay completeness — the relay sequences but never adjudicates (LWW runs per replica).
func (s *Store) ApplyBatch(siteID string, ops []Op) ApplyResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	var res ApplyResult
	rejectedSeqs := make(map[int64]bool) // op_seqs of siteID permanently rejected this call
	for _, op := range ops {
		if reason := s.validateOp(op); reason != "" {
			res.Rejected = append(res.Rejected, RejectedOp{SiteID: op.SiteID, OpSeq: op.OpSeq, Reason: reason})
			if op.SiteID == siteID {
				rejectedSeqs[op.OpSeq] = true
			}
			continue
		}
		id := opID{siteID: op.SiteID, opSeq: op.OpSeq}
		if _, dup := s.seen[id]; dup {
			continue // already in the log → already settled, no re-append
		}
		s.seq++
		s.log = append(s.log, logRec{seq: s.seq, op: op})
		s.seen[id] = struct{}{}
	}
	res.AcceptedThrough = s.advanceAccepted(siteID, rejectedSeqs)
	return res
}

// validateOp returns "" if op is structurally acceptable, else a permanent rejection reason. Value
// types are not checked here (the relay carries cols opaquely; a mirror validates on materialize).
func (s *Store) validateOp(op Op) string {
	known, ok := s.knownCols[op.Table]
	if !ok {
		return "unknown table"
	}
	if !IsULID(op.PK) {
		return "pk is not a ULID"
	}
	if !IsULID(op.SiteID) {
		return "site_id is not a ULID"
	}
	if op.OpSeq <= 0 {
		return "op_seq must be > 0"
	}
	for _, c := range known {
		if _, present := op.Cols[c]; !present {
			return "missing column: " + c
		}
	}
	return ""
}

// advanceAccepted walks siteID's contiguous high-water from its prior value: the greatest N such
// that every op_seq 1..N is settled — present in the log (applied this call or a prior one) OR
// permanently rejected this call. Counting a rejected op once here means a poison op neither
// wedges the water nor is silently lost.
func (s *Store) advanceAccepted(siteID string, rejectedSeqs map[int64]bool) int64 {
	h := s.acked[siteID]
	for {
		next := h + 1
		if rejectedSeqs[next] {
			h = next
			continue
		}
		if _, ok := s.seen[opID{siteID: siteID, opSeq: next}]; ok {
			h = next
			continue
		}
		break
	}
	s.acked[siteID] = h
	return h
}

// OpsSince returns log ops with seq > cursor authored by some OTHER site, ascending, capped at
// limit. newCursor is the seq of the last returned op (or cursor if none); hasMore is true iff
// more lie beyond the cap.
func (s *Store) OpsSince(cursor int64, excludeSite string, limit int) (ops []Op, newCursor int64, hasMore bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 500
	}
	newCursor = cursor
	for _, rec := range s.log { // log is appended in seq order
		if rec.seq <= cursor || rec.op.SiteID == excludeSite {
			continue
		}
		if len(ops) == limit {
			return ops, newCursor, true
		}
		ops = append(ops, rec.op)
		newCursor = rec.seq
	}
	return ops, newCursor, false
}

// LastSeq returns the current global high-water.
func (s *Store) LastSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}
