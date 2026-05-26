package syncstore

import "testing"

// schemaHashV1 is the published v1 schema hash (docs/sync/forestnote-sync-protocol.md
// §6). If this assertion fails, either knownCols changed (a wire-breaking schema
// change that needs a coordinated bump) or the spec doc is stale.
const schemaHashV1 = "9b807dc88cd0465d171892bb17e65ad94190eda058594e207caad3368eb1f2fe"

func TestSchemaHashMatchesSpec(t *testing.T) {
	if got := SchemaHash(); got != schemaHashV1 {
		t.Errorf("schema hash drift:\n got: %s\nwant: %s\ncanonical: %s",
			got, schemaHashV1, canonicalSchema())
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
