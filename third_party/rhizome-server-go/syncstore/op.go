// Package syncstore holds the relay log, the op model, and the deterministic LWW merge.
//
// The op model and merge are the half of the contract shared with the Kotlin client; they MUST
// agree byte-for-byte on the vectors in /conformance/vectors (see spec/merge.md). The relay log,
// sequencing, compaction, and HTTP wiring (later phases) build on these types.
package syncstore

import "encoding/json"

// Op is a full-row snapshot: a single change, self-contained, never a diff.
// Identity is (SiteID, OpSeq), globally unique. Cols carries the wire-encoded row.
//
// OpTs is the ordering timestamp (a Hybrid Logical Clock int64 — see spec/hlc.md). It replaces
// the legacy wall_ts; the merge treats it as an opaque int64, so legacy values interoperate.
type Op struct {
	Table  string                     `json:"table"`
	PK     string                     `json:"pk"`
	SiteID string                     `json:"site_id"`
	OpSeq  int64                      `json:"op_seq"`
	OpTs   int64                      `json:"op_ts"`
	Cols   map[string]json.RawMessage `json:"cols"`
}

// TablePK keys a row across the synced data set.
type TablePK struct {
	Table string
	PK    string
}

// Key returns this op's (Table, PK).
func (o Op) Key() TablePK { return TablePK{Table: o.Table, PK: o.PK} }
