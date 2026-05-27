// Package syncstore is the server-side mirror, op changelog, and deterministic
// merge for device sync (ForestNote today; protocol-generic by design). The wire
// contract and merge rule are specified in docs/sync/forestnote-sync-protocol.md;
// reconcile.go MUST agree with that spec and with docs/sync/vectors/.
package syncstore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
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

// newULID mints a canonical 26-char uppercase Crockford ULID: a 48-bit
// millisecond timestamp followed by 80 bits of crypto-random, encoded
// most-significant-first (the standard ULID layout). UB needs this because as an
// authoring site its site_id travels on the wire, where the device validates it
// as a ULID — the legacy "ub-web" sentinel is not wire-legal (it is only a
// local-provenance marker). The result always satisfies IsULID.
func newULID() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	// crypto/rand.Read never returns a short read or error on a healthy OS; a
	// failure here would mean the system RNG is broken, so panicking is correct.
	if _, err := rand.Read(b[6:]); err != nil {
		panic("syncstore: crypto/rand failed minting ULID: " + err.Error())
	}
	return encodeULID(b)
}

// encodeULID renders the 16 raw bytes as 26 Crockford base32 chars, reading the
// 128 bits 5 at a time most-significant-first (the canonical ULID byte→char map).
func encodeULID(b [16]byte) string {
	d := make([]byte, 26)
	d[0] = ulidAlphabet[(b[0]&224)>>5]
	d[1] = ulidAlphabet[b[0]&31]
	d[2] = ulidAlphabet[(b[1]&248)>>3]
	d[3] = ulidAlphabet[((b[1]&7)<<2)|((b[2]&192)>>6)]
	d[4] = ulidAlphabet[(b[2]&62)>>1]
	d[5] = ulidAlphabet[((b[2]&1)<<4)|((b[3]&240)>>4)]
	d[6] = ulidAlphabet[((b[3]&15)<<1)|((b[4]&128)>>7)]
	d[7] = ulidAlphabet[(b[4]&124)>>2]
	d[8] = ulidAlphabet[((b[4]&3)<<3)|((b[5]&224)>>5)]
	d[9] = ulidAlphabet[b[5]&31]
	d[10] = ulidAlphabet[(b[6]&248)>>3]
	d[11] = ulidAlphabet[((b[6]&7)<<2)|((b[7]&192)>>6)]
	d[12] = ulidAlphabet[(b[7]&62)>>1]
	d[13] = ulidAlphabet[((b[7]&1)<<4)|((b[8]&240)>>4)]
	d[14] = ulidAlphabet[((b[8]&15)<<1)|((b[9]&128)>>7)]
	d[15] = ulidAlphabet[(b[9]&124)>>2]
	d[16] = ulidAlphabet[((b[9]&3)<<3)|((b[10]&224)>>5)]
	d[17] = ulidAlphabet[b[10]&31]
	d[18] = ulidAlphabet[(b[11]&248)>>3]
	d[19] = ulidAlphabet[((b[11]&7)<<2)|((b[12]&192)>>6)]
	d[20] = ulidAlphabet[(b[12]&62)>>1]
	d[21] = ulidAlphabet[((b[12]&1)<<4)|((b[13]&240)>>4)]
	d[22] = ulidAlphabet[((b[13]&15)<<1)|((b[14]&128)>>7)]
	d[23] = ulidAlphabet[(b[14]&124)>>2]
	d[24] = ulidAlphabet[((b[14]&3)<<3)|((b[15]&224)>>5)]
	d[25] = ulidAlphabet[b[15]&31]
	return string(d)
}

// IsULID reports whether s is a canonical 26-char uppercase Crockford ULID.
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
