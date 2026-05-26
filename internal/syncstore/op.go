// Package syncstore is the server-side mirror, op changelog, and deterministic
// merge for device sync (ForestNote today; protocol-generic by design). The wire
// contract and merge rule are specified in docs/sync/forestnote-sync-protocol.md;
// reconcile.go MUST agree with that spec and with docs/sync/vectors/.
package syncstore

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Op is one full-row UPSERT on the wire (spec §3). Its identity is
// (SiteID, OpSeq) — globally unique, the dedup key.
type Op struct {
	Table  string         `json:"table"`
	PK     string         `json:"pk"`
	SiteID string         `json:"site_id"`
	OpSeq  int64          `json:"op_seq"`
	WallTS int64          `json:"wall_ts"`
	Cols   map[string]any `json:"cols"`
}

// knownCols lists the materialized columns per table (spec §3.1). It is also the
// basis for the schema_hash (§6) — see SchemaHash. Keep alphabetical within a
// table to match the canonical schema string.
var knownCols = map[string][]string{
	"folder":   {"created_at", "deleted_at", "name", "parent_folder_id", "sort_order"},
	"notebook": {"created_at", "deleted_at", "folder_id", "name", "sort_order"},
	"page":     {"created_at", "deleted_at", "notebook_id", "sort_order", "template", "template_pitch_mm"},
	"stroke":   {"color", "created_at", "deleted_at", "page_id", "pen_width_max", "pen_width_min", "points", "z"},
}

// tableOrder is the fixed table order for the canonical schema string (§6).
var tableOrder = []string{"folder", "notebook", "page", "stroke"}

// canonicalSchema builds the deterministic schema string (spec §6): tables in
// fixed order, columns alphabetical within each table, `table:col,col;table:...`,
// no spaces, no trailing newline. knownCols is already alphabetical per table.
func canonicalSchema() string {
	parts := make([]string, len(tableOrder))
	for i, t := range tableOrder {
		parts[i] = t + ":" + strings.Join(knownCols[t], ",")
	}
	return strings.Join(parts, ";")
}

// SchemaHash is the lowercase hex SHA-256 of the canonical schema string (§6).
// A client whose hash the server does not recognize is rejected (409) so it
// cannot corrupt the mirror. The published v1 value is asserted in the tests.
func SchemaHash() string {
	sum := sha256.Sum256([]byte(canonicalSchema()))
	return hex.EncodeToString(sum[:])
}

const ulidAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford base32, uppercase (§2.1)

// IsULID reports whether s is a canonical 26-char uppercase Crockford ULID. The
// server validates ForestNote ULIDs but never mints them (it is a relay).
func IsULID(s string) bool {
	if len(s) != 26 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(ulidAlphabet, s[i]) < 0 {
			return false
		}
	}
	return true
}

// Normalize returns a copy of op whose Cols contains only columns known for its
// table (spec §3.2 — unknown columns are dropped on materialize). Unknown tables
// yield an empty column set; callers validate the table separately.
func Normalize(op Op) Op {
	known := knownCols[op.Table]
	cols := make(map[string]any, len(known))
	for _, c := range known {
		if v, ok := op.Cols[c]; ok {
			cols[c] = v
		}
	}
	out := op
	out.Cols = cols
	return out
}
