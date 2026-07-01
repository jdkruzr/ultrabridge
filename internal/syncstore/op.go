// Package syncstore is the server-side mirror, op changelog, and deterministic
// merge for device sync (ForestNote today; protocol-generic by design). The wire
// contract and merge rule are specified in docs/sync/forestnote-sync-protocol.md;
// reconcile.go MUST agree with that spec and with docs/sync/vectors/.
package syncstore

import (
	"crypto/rand"
	"encoding/json"
	"strings"
	"time"

	"github.com/jdkruzr/rhizome/server-go/registry"
)

// Op is one full-row UPSERT on the wire (spec §3). Its identity is
// (SiteID, OpSeq) — globally unique, the dedup key.
//
// The ordering key is WallTS, serialized as `op_ts` (the RhizomeSync wire name; the value is an HLC
// int64 whose high bits are wall-clock ms — see hlc). The Go field keeps the name WallTS and the DB
// column stays `wall_ts`; only the JSON tag changed at the cutover. UnmarshalJSON also accepts the
// legacy `wall_ts` key so historical sync_ops payloads (written before the rename) still relay with
// their timestamp intact.
type Op struct {
	Table  string         `json:"table"`
	PK     string         `json:"pk"`
	SiteID string         `json:"site_id"`
	OpSeq  int64          `json:"op_seq"`
	WallTS int64          `json:"op_ts"`
	Cols   map[string]any `json:"cols"`
}

// UnmarshalJSON reads the ordering timestamp from `op_ts` (current wire) and falls back to the
// legacy `wall_ts` key when `op_ts` is absent — so a sync_ops payload marshaled before the cutover
// still populates WallTS instead of decoding to 0 (which would make it lose every LWW conflict).
func (o *Op) UnmarshalJSON(data []byte) error {
	type opAlias Op // strip methods to avoid recursing into this UnmarshalJSON
	var v struct {
		opAlias
		LegacyWallTS *int64 `json:"wall_ts"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*o = Op(v.opAlias)
	if o.WallTS == 0 && v.LegacyWallTS != nil {
		o.WallTS = *v.LegacyWallTS
	}
	return nil
}

// fnReg is the RhizomeSync registry declaration of ForestNote's synced schema, the single source of
// truth for the wire schema after the Phase 8 cutover. UB's hand-coded knownCols/tableOrder/SHA were
// replaced by forwards into it; the parity tests pin that it reproduces the live v3 hash byte-for-byte.
var fnReg = registry.ForestNote()

// knownCols lists the materialized columns per table (spec §3.1), derived from the registry. It
// drives Normalize/validateOp; the registry guarantees alphabetical order within each table.
var knownCols = fnReg.KnownCols()

// schemaHashCurrent is the lowercase hex SHA-256 of the canonical schema string (§6) — the CURRENT
// schema the server advertises — computed once from the registry at init.
var schemaHashCurrent = fnReg.SchemaHash()

// canonicalSchema returns the deterministic schema string (spec §6) from the registry. Retained as a
// thin forward for the parity/spec tests that compare it.
func canonicalSchema() string { return fnReg.Canonical() }

// SchemaHash is the CURRENT schema hash the server advertises. A client whose hash the server does
// not accept is rejected (409) so it cannot corrupt the mirror (see AcceptsSchemaHash).
func SchemaHash() string { return schemaHashCurrent }

// schemaHashV1 is the FROZEN historical schema hash (folder/notebook/page/stroke,
// no text_box). It is hardcoded — not derived — because once knownCols gains a
// table the derivation yields v2; we must still recognize a v1 client during the
// rollout grace window (see AcceptsSchemaHash). Keep this literal forever.
const schemaHashV1 = "9b807dc88cd0465d171892bb17e65ad94190eda058594e207caad3368eb1f2fe"

// schemaHashV2 is the FROZEN prior schema hash (folder/notebook/page/stroke/text_box,
// before the page_text_* tables). Hardcoded, not derived. Its grace window CLOSED with the
// v4 (notebook.aspect_long_axis) rollout — kept as a literal for the historical record, but
// no longer admitted by AcceptsSchemaHash.
const schemaHashV2 = "bc1953e2b85e766a572329e7023b4582b768094b4d27e28a632e21bedb776874"

// schemaHashV3 is the FROZEN prior schema hash (folder/notebook/page/page_text_from_client/
// page_text_from_server/stroke/text_box, before notebook.aspect_long_axis). Hardcoded, not
// derived: once the registry gains aspect_long_axis SchemaHash() yields v4, but a not-yet-updated
// v3 client must keep syncing during the rollout grace window. Keep this literal forever.
const schemaHashV3 = "724411eb845ad3487393a77cb5559690e69332c35fdb5ee3e85c1767bf71f3fe"

// AcceptsSchemaHash reports whether the server will sync with a client advertising
// hash h. It accepts the current schema (SchemaHash, now v4 — folder/notebook[+aspect_long_axis]/
// page/page_text_*/stroke/text_box) AND the frozen prior schema (schemaHashV3), so a not-yet-updated
// v3 client keeps syncing while the matching client release rolls out — instead of a hard cutover
// that 409s every old client the instant the server adds aspect_long_axis. A v3 client never sends
// the new column and silently ignores it on relayed rows, so admitting it is safe; once all clients
// update, drop schemaHashV3 from this set. v2/v1 are retired (their grace windows closed at the
// page_text_* and text_box rollouts). Generalizes to every future schema bump: add the new hash,
// keep the prior one for one release, then retire it.
func AcceptsSchemaHash(h string) bool {
	return h == SchemaHash() || h == schemaHashV3
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

// ULIDTime decodes the 48-bit millisecond Unix timestamp embedded in a ULID's
// first 10 characters (the inverse of encodeULID's timestamp half). For a
// device site_id this is the moment the client minted it — i.e. when that
// install first enabled sync — which the device-management UI surfaces as
// "first seen" without needing a stored column. Returns ok=false for a
// non-ULID input.
func ULIDTime(s string) (unixMs int64, ok bool) {
	if !IsULID(s) {
		return 0, false
	}
	var ms int64
	for i := 0; i < 10; i++ {
		ms = ms<<5 | int64(strings.IndexByte(ulidAlphabet, s[i]))
	}
	return ms, true
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
