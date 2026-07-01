package syncstore

import (
	"testing"
	"time"
)

// schemaHashV4 is the published CURRENT schema hash (docs/sync/forestnote-sync-protocol.md
// §6) — folder/notebook[+aspect_long_axis]/page/page_text_from_client/page_text_from_server/
// stroke/text_box. If this assertion fails, either knownCols changed (a wire-breaking schema
// change that needs a coordinated bump + a new vN constant) or the spec doc is stale. The frozen
// prior values (schemaHashV3, schemaHashV2, schemaHashV1) live in op.go.
const schemaHashV4 = "74e6b5d790c919290d0e1fca3462800a5dc4abb288042dda2b48d4eb0482bbf2"

func TestSchemaHashMatchesSpec(t *testing.T) {
	if got := SchemaHash(); got != schemaHashV4 {
		t.Errorf("schema hash drift:\n got: %s\nwant: %s\ncanonical: %s",
			got, schemaHashV4, canonicalSchema())
	}
}

// AcceptsSchemaHash is the rollout grace window: it must admit BOTH the current schema
// (v4) and the frozen prior schema (v3), and reject anything else — including the retired
// v2 (pre-aspect_long_axis... actually pre-page_text_*) and v1, whose grace windows closed.
func TestAcceptsSchemaHash_GraceWindow(t *testing.T) {
	if !AcceptsSchemaHash(SchemaHash()) {
		t.Error("current schema hash (v4) must be accepted")
	}
	if !AcceptsSchemaHash(schemaHashV3) {
		t.Error("frozen v3 schema hash must still be accepted during the grace window")
	}
	if AcceptsSchemaHash(schemaHashV2) {
		t.Error("retired v2 schema hash must no longer be accepted")
	}
	if AcceptsSchemaHash(schemaHashV1) {
		t.Error("retired v1 schema hash must no longer be accepted")
	}
	if AcceptsSchemaHash("0000000000000000000000000000000000000000000000000000000000000000") {
		t.Error("an unknown schema hash must be rejected")
	}
}

func TestIsULID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"00000000000000000000000NB1", true},
		{"0000000000000000000000000A", true},
		{"0000000000000000000000000", false},   // 25 chars
		{"0000000000000000000000000AA", false}, // 27 chars
		{"0000000000000000000000000I", false},  // I not in Crockford
		{"0000000000000000000000000a", false},  // lowercase
	}
	for _, c := range cases {
		if got := IsULID(c.in); got != c.want {
			t.Errorf("IsULID(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestULIDTime(t *testing.T) {
	// Round-trip: a freshly minted ULID decodes to "now" (within the mint window).
	before := time.Now().UnixMilli()
	ms, ok := ULIDTime(newULID())
	after := time.Now().UnixMilli()
	if !ok {
		t.Fatal("ULIDTime rejected a newULID() value")
	}
	if ms < before || ms > after {
		t.Errorf("decoded ms %d outside mint window [%d, %d]", ms, before, after)
	}

	// Fixed vector: encode a known timestamp, decode it back exactly.
	const wantMs = int64(1717200000000) // 2024-06-01T00:00:00Z
	var b [16]byte
	u := uint64(wantMs)
	b[0], b[1], b[2], b[3], b[4], b[5] = byte(u>>40), byte(u>>32), byte(u>>24), byte(u>>16), byte(u>>8), byte(u)
	if got, ok := ULIDTime(encodeULID(b)); !ok || got != wantMs {
		t.Errorf("ULIDTime(fixed) = (%d, %v), want (%d, true)", got, ok, wantMs)
	}

	// All-zero timestamp half (the test-suite site_id convention) decodes to 0.
	if got, ok := ULIDTime("0000000000000000000000000A"); !ok || got != 0 {
		t.Errorf("ULIDTime(zero ULID) = (%d, %v), want (0, true)", got, ok)
	}

	// Non-ULID input is rejected.
	if _, ok := ULIDTime("not-a-ulid"); ok {
		t.Error("ULIDTime accepted a non-ULID string")
	}
}

func TestNormalizeDropsUnknownCols(t *testing.T) {
	op := Op{
		Table: "notebook",
		Cols:  map[string]any{"name": "x", "sort_order": 0, "created_at": 1, "deleted_at": nil, "archived": true},
	}
	n := Normalize(op)
	if _, ok := n.Cols["archived"]; ok {
		t.Errorf("unknown column 'archived' not dropped: %v", n.Cols)
	}
	if len(n.Cols) != 4 {
		t.Errorf("expected 4 known cols, got %d: %v", len(n.Cols), n.Cols)
	}
}

func TestLessTotalOrder(t *testing.T) {
	base := Op{WallTS: 100, OpSeq: 5, SiteID: "0000000000000000000000000A"}
	// higher wall_ts wins regardless of lower op_seq
	if !Less(base, Op{WallTS: 200, OpSeq: 1, SiteID: "0000000000000000000000000A"}) {
		t.Error("wall_ts should dominate op_seq")
	}
	// equal wall_ts: higher op_seq wins
	if !Less(base, Op{WallTS: 100, OpSeq: 6, SiteID: "0000000000000000000000000A"}) {
		t.Error("op_seq should break wall_ts tie")
	}
	// equal wall_ts+op_seq: greater site_id wins
	if !Less(base, Op{WallTS: 100, OpSeq: 5, SiteID: "0000000000000000000000000B"}) {
		t.Error("site_id should break wall_ts+op_seq tie")
	}
}
