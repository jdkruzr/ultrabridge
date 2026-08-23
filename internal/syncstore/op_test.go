package syncstore

import (
	"testing"
	"time"
)

// schemaHashV5 is the published CURRENT schema hash. If this assertion fails, either knownCols changed (a wire-breaking schema
// change that needs a coordinated bump + a new vN constant) or the spec doc is stale. The frozen
// prior values (schemaHashV3, schemaHashV2, schemaHashV1) live in op.go.
const schemaHashV5 = "ed367ffd86b24c3b53f7a85b4f46b7f0cb69e0c6fbd0e1048289a659b4c967dd"

func TestSchemaHashMatchesSpec(t *testing.T) {
	if got := SchemaHash(); got != schemaHashV5 {
		t.Errorf("schema hash drift:\n got: %s\nwant: %s\ncanonical: %s",
			got, schemaHashV5, canonicalSchema())
	}
}

// AcceptsSchemaHash is the rollout grace window: it must admit BOTH the current schema
// (v4) and the frozen prior schema (v3), and reject anything else — including the retired
// v2 (pre-aspect_long_axis... actually pre-page_text_*) and v1, whose grace windows closed.
func TestAcceptsSchemaHash_GraceWindow(t *testing.T) {
	if !AcceptsSchemaHash(SchemaHash()) {
		t.Error("current schema hash (v5) must be accepted")
	}
	if !AcceptsSchemaHash(schemaHashV4) {
		t.Error("frozen v4 schema hash must still be accepted during the grace window")
	}
	if AcceptsSchemaHash(schemaHashV3) {
		t.Error("retired v3 schema hash must no longer be accepted")
	}
	if AcceptsSchemaHash(schemaHashV1) {
		t.Error("retired v1 schema hash must no longer be accepted")
	}
	if AcceptsSchemaHash("0000000000000000000000000000000000000000000000000000000000000000") {
		t.Error("an unknown schema hash must be rejected")
	}
}

func TestWithV5DefaultsCompletesLegacyRowsWithoutMutatingInput(t *testing.T) {
	legacy := Op{Table: "stroke", PK: "01KTEST", Cols: map[string]any{"color": float64(-16777216)}}
	completed := withV5Defaults(legacy)
	if _, leaked := legacy.Cols["brush_kind"]; leaked {
		t.Fatal("legacy input map was mutated")
	}
	if completed.Cols["brush_kind"] != "fountain" || completed.Cols["brush_version"] != float64(1) {
		t.Fatalf("defaults = %#v", completed.Cols)
	}
	if v, ok := completed.Cols["point_dynamics"]; !ok || v != nil {
		t.Fatalf("point_dynamics = %#v, want present null", v)
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
