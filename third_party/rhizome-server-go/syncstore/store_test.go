package syncstore

import (
	"encoding/json"
	"testing"

	"github.com/jdkruzr/rhizome/server-go/registry"
)

var knownCols = registry.ForestNote().KnownCols()

// A minimal but valid stroke op for site/seq. cols carry every known stroke column.
func strokeOp(t *testing.T, site, pk string, opSeq, opTs int64) Op {
	t.Helper()
	cols := map[string]json.RawMessage{
		"page_id":       json.RawMessage(`"` + pad26("PAGE") + `"`),
		"color":         json.RawMessage(`4278190080`),
		"pen_width_min": json.RawMessage(`2`),
		"pen_width_max": json.RawMessage(`8`),
		"points":        json.RawMessage(`"AAEC"`),
		"z":             json.RawMessage(`5`),
		"created_at":    json.RawMessage(`100`),
		"deleted_at":    json.RawMessage(`null`),
	}
	return Op{Table: "stroke", PK: pk, SiteID: site, OpSeq: opSeq, OpTs: opTs, Cols: cols}
}

// pad26 makes a deterministic 26-char uppercase Crockford ULID-shaped string for tests.
func pad26(prefix string) string {
	const fill = "0000000000000000000000000000"
	s := prefix + fill
	return s[:26]
}

func TestUlidHelpers(t *testing.T) {
	id := NewULID()
	if !IsULID(id) {
		t.Fatalf("NewULID produced non-ULID %q", id)
	}
	if IsULID("not-a-ulid") || IsULID("") {
		t.Fatalf("IsULID accepted an invalid string")
	}
}

func TestApplyBatchSequencesAndRelaysToOtherSites(t *testing.T) {
	s := NewStore(knownCols)
	siteA, siteB := pad26("AAAA"), pad26("BBBB")

	res := s.ApplyBatch(siteA, []Op{
		strokeOp(t, siteA, pad26("S1"), 1, 100),
		strokeOp(t, siteA, pad26("S2"), 2, 100),
	})
	if res.AcceptedThrough != 2 {
		t.Fatalf("accepted_through = %d, want 2", res.AcceptedThrough)
	}
	if len(res.Rejected) != 0 {
		t.Fatalf("unexpected rejects: %v", res.Rejected)
	}
	if s.LastSeq() != 2 {
		t.Fatalf("global seq = %d, want 2", s.LastSeq())
	}

	// Author excluded from its own pull; site B sees both.
	own, cur, more := s.OpsSince(0, siteA, 100)
	if len(own) != 0 {
		t.Fatalf("author should not pull its own ops, got %d", len(own))
	}
	got, cur, more := s.OpsSince(0, siteB, 100)
	if len(got) != 2 || more {
		t.Fatalf("site B should see 2 ops, has_more=false; got %d more=%v", len(got), more)
	}
	if cur != 2 {
		t.Fatalf("newCursor = %d, want 2", cur)
	}
	_ = cur
}

func TestApplyBatchRejectsInvalidOps(t *testing.T) {
	s := NewStore(knownCols)
	site := pad26("AAAA")

	bad := []Op{
		{Table: "nope", PK: pad26("X1"), SiteID: site, OpSeq: 1, OpTs: 1, Cols: nil},   // unknown table
		{Table: "stroke", PK: "short-pk", SiteID: site, OpSeq: 2, OpTs: 1, Cols: nil},  // pk not ULID
		strokeMissingCol(t, site, pad26("X3"), 3),                                      // missing column
		{Table: "stroke", PK: pad26("X4"), SiteID: site, OpSeq: 0, OpTs: 1, Cols: nil}, // op_seq <= 0
	}
	res := s.ApplyBatch(site, bad)
	if len(res.Rejected) != 4 {
		t.Fatalf("want 4 rejects, got %d: %v", len(res.Rejected), res.Rejected)
	}
	if s.LastSeq() != 0 {
		t.Fatalf("no op should have been logged, seq=%d", s.LastSeq())
	}
}

func strokeMissingCol(t *testing.T, site, pk string, opSeq int64) Op {
	op := strokeOp(t, site, pk, opSeq, 1)
	delete(op.Cols, "z") // a required known column is absent
	return op
}

func TestDedupIsIdempotentAndStillAcks(t *testing.T) {
	s := NewStore(knownCols)
	site := pad26("AAAA")
	op := strokeOp(t, site, pad26("S1"), 1, 100)

	s.ApplyBatch(site, []Op{op})
	res := s.ApplyBatch(site, []Op{op}) // re-deliver the same op
	if s.LastSeq() != 1 {
		t.Fatalf("re-delivered op must not be appended again; seq=%d", s.LastSeq())
	}
	if res.AcceptedThrough != 1 {
		t.Fatalf("dedup must still settle the op; accepted_through=%d", res.AcceptedThrough)
	}
}

func TestAcceptedThroughIsContiguousAndSkipsRejected(t *testing.T) {
	s := NewStore(knownCols)
	site := pad26("AAAA")

	// op_seq 1 ok, 2 invalid (rejected), 3 ok. accepted_through must walk past the rejected 2 to 3.
	res := s.ApplyBatch(site, []Op{
		strokeOp(t, site, pad26("S1"), 1, 100),
		{Table: "stroke", PK: "bad", SiteID: site, OpSeq: 2, OpTs: 100, Cols: nil},
		strokeOp(t, site, pad26("S3"), 3, 100),
	})
	if res.AcceptedThrough != 3 {
		t.Fatalf("accepted_through = %d, want 3 (rejected #2 counted as settled)", res.AcceptedThrough)
	}
}

func TestOpsSinceHasMoreAndCursor(t *testing.T) {
	s := NewStore(knownCols)
	author := pad26("AAAA")
	puller := pad26("BBBB")
	for i := int64(1); i <= 5; i++ {
		s.ApplyBatch(author, []Op{strokeOp(t, author, pad26("S"+padN(i)), i, 100)})
	}
	got, cur, more := s.OpsSince(0, puller, 2)
	if len(got) != 2 || !more {
		t.Fatalf("want 2 ops + has_more; got %d more=%v", len(got), more)
	}
	if cur != 2 {
		t.Fatalf("newCursor = %d, want 2", cur)
	}
	rest, cur2, more2 := s.OpsSince(cur, puller, 100)
	if len(rest) != 3 || more2 {
		t.Fatalf("want 3 remaining, no more; got %d more=%v", len(rest), more2)
	}
	if cur2 != 5 {
		t.Fatalf("final cursor = %d, want 5", cur2)
	}
}

func padN(i int64) string {
	d := string(rune('0' + i))
	return d
}
