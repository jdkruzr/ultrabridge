package syncstore

import "encoding/json"

// reconcile.go is the pure, side-effect-free row-level Last-Writer-Wins merge (spec/merge.md). It
// MUST agree with the Kotlin client's Merge and pass every `merge`/`normalize` vector in
// /conformance/vectors. The winner for a (table, pk) is the op with the greatest key.

// Less reports whether a's LWW key is strictly less than b's: compare (OpTs, OpSeq, SiteID)
// lexicographically (SiteID as its ASCII/ULID string). The op with the GREATER key wins.
func Less(a, b Op) bool {
	if a.OpTs != b.OpTs {
		return a.OpTs < b.OpTs
	}
	if a.OpSeq != b.OpSeq {
		return a.OpSeq < b.OpSeq
	}
	return a.SiteID < b.SiteID
}

// Normalize returns a copy of op whose Cols contains only columns known for its table (unknown
// columns are dropped on materialize — forward-compat). An unknown table yields an empty col set.
func Normalize(op Op, knownCols map[string][]string) Op {
	known := knownCols[op.Table]
	cols := make(map[string]json.RawMessage, len(known))
	for _, c := range known {
		if v, ok := op.Cols[c]; ok {
			cols[c] = v
		}
	}
	out := op
	out.Cols = cols
	return out
}

// Merge reduces a batch of ops to the winning (normalized) op per (table, pk) under the LWW total
// order. The selection is arrival-order-independent, so every replica that has seen the same ops
// converges to the same winners (the convergence property).
func Merge(ops []Op, knownCols map[string][]string) map[TablePK]Op {
	winners := make(map[TablePK]Op)
	for _, raw := range ops {
		n := Normalize(raw, knownCols)
		k := n.Key()
		if cur, ok := winners[k]; !ok || Less(cur, n) {
			winners[k] = n
		}
	}
	return winners
}
