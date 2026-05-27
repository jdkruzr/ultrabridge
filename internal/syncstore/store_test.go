package syncstore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

const (
	siteA = "0000000000000000000000000A"
	siteB = "0000000000000000000000000B"
	nb1   = "00000000000000000000000NB1"
	pg1   = "00000000000000000000000PG1"
	st1   = "00000000000000000000000ST1"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)",
		filepath.Join(t.TempDir(), "sync.db"))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return New(db)
}

func notebookOp(site string, opSeq, wall int64, name string, deletedAt any) Op {
	return Op{
		Table: "notebook", PK: nb1, SiteID: site, OpSeq: opSeq, WallTS: wall,
		Cols: map[string]any{"name": name, "sort_order": float64(0), "created_at": float64(1000), "deleted_at": deletedAt, "folder_id": nil},
	}
}

func TestApplyBatch_MaterializesAndProvenance(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.ApplyBatch(ctx, siteA, []Op{notebookOp(siteA, 1, 1000, "Journal", nil)}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var name string
	var lwwOp int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT name, lww_op_seq FROM fn_notebook WHERE id = ?`, nb1).Scan(&name, &lwwOp); err != nil {
		t.Fatalf("query: %v", err)
	}
	if name != "Journal" || lwwOp != 1 {
		t.Errorf("got name=%q lww_op_seq=%d, want Journal/1", name, lwwOp)
	}
}

func TestApplyBatch_LWWHigherWins(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, err := s.ApplyBatch(ctx, siteA, []Op{
		notebookOp(siteA, 1, 1000, "Old", nil),
		notebookOp(siteA, 2, 2000, "New", nil),
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var name string
	s.db.QueryRowContext(ctx, `SELECT name FROM fn_notebook WHERE id = ?`, nb1).Scan(&name)
	if name != "New" {
		t.Errorf("got %q, want New (higher wall_ts wins)", name)
	}
}

func TestApplyBatch_Idempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	batch := []Op{notebookOp(siteA, 1, 1000, "Journal", nil), notebookOp(siteA, 2, 2000, "Renamed", nil)}

	r1, err := s.ApplyBatch(ctx, siteA, batch)
	if err != nil {
		t.Fatalf("apply1: %v", err)
	}
	if r1.AcceptedThrough != 2 {
		t.Fatalf("first apply: want accepted_through 2, got %d", r1.AcceptedThrough)
	}

	r2, err := s.ApplyBatch(ctx, siteA, batch) // replay
	if err != nil {
		t.Fatalf("apply2: %v", err)
	}
	if r2.AcceptedThrough != 2 {
		t.Errorf("replay: want accepted_through 2 (deduped), got %d", r2.AcceptedThrough)
	}
	// no duplicate changelog rows
	var n int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_ops`).Scan(&n)
	if n != 2 {
		t.Errorf("want 2 sync_ops rows after replay, got %d", n)
	}
}

func TestApplyBatch_RejectsMalformed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	bad := []Op{
		{Table: "bogus", PK: nb1, SiteID: siteA, OpSeq: 1, WallTS: 1, Cols: map[string]any{}},               // unknown table
		{Table: "notebook", PK: "short", SiteID: siteA, OpSeq: 2, WallTS: 1, Cols: map[string]any{}},        // bad pk
		{Table: "notebook", PK: nb1, SiteID: siteA, OpSeq: 0, WallTS: 1, Cols: map[string]any{}},            // op_seq 0
		{Table: "notebook", PK: nb1, SiteID: siteA, OpSeq: 3, WallTS: 1, Cols: map[string]any{"name": "x"}}, // missing cols
		notebookOp(siteA, 4, 1000, "Good", nil),                                                             // valid
	}
	res, err := s.ApplyBatch(ctx, siteA, bad)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.Rejected) != 4 {
		t.Errorf("want 4 rejected, got %d: %+v", len(res.Rejected), res.Rejected)
	}
	// op_seqs 1,2,3 rejected, 4 applied → contiguous high-water is 4 (poison ops
	// don't wedge the water).
	if res.AcceptedThrough != 4 {
		t.Errorf("want accepted_through 4, got %d", res.AcceptedThrough)
	}
}

func TestApplyBatch_ChangedPagesFromStroke(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	stroke := Op{
		Table: "stroke", PK: st1, SiteID: siteA, OpSeq: 1, WallTS: 1000,
		Cols: map[string]any{
			"page_id": pg1, "color": float64(4278190080), "pen_width_min": float64(2),
			"pen_width_max": float64(6), "points": "MgAAADwAAADIAAAAAAAAAAEAAAA=",
			"z": float64(0), "created_at": float64(1000), "deleted_at": nil,
		},
	}
	res, err := s.ApplyBatch(ctx, siteA, []Op{stroke})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.ChangedPages) != 1 || res.ChangedPages[0].PK != pg1 {
		t.Errorf("want ChangedPages [page %s], got %+v", pg1, res.ChangedPages)
	}
}

func TestApplyBatch_TextBoxMaterializes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const tb1 = "00000000000000000000000TB1"
	box := Op{
		Table: "text_box", PK: tb1, SiteID: siteA, OpSeq: 1, WallTS: 1000,
		Cols: map[string]any{
			"page_id": pg1, "x": float64(100), "y": float64(200), "width": float64(3000),
			"height": float64(1500), "text": "hello world", "font_name": "Roboto-Regular.ttf",
			"font_size": float64(320), "color": float64(4278190080), // unsigned ARGB black, like stroke
			"weight": float64(400), "border_width": float64(2), "z": float64(1),
			"created_at": float64(1000), "deleted_at": nil,
		},
	}
	res, err := s.ApplyBatch(ctx, siteA, []Op{box})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// The text box's page is reported as changed (drives re-render/re-index).
	if len(res.ChangedPages) != 1 || res.ChangedPages[0].PK != pg1 {
		t.Errorf("ChangedPages = %+v, want [page %s]", res.ChangedPages, pg1)
	}
	// Row materialized with the right values, color stored verbatim (unsigned).
	var text, font string
	var x, color, z int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT text, font_name, x, color, z FROM fn_text_box WHERE id = ?`, tb1).
		Scan(&text, &font, &x, &color, &z); err != nil {
		t.Fatalf("query text_box: %v", err)
	}
	if text != "hello world" || font != "Roboto-Regular.ttf" || x != 100 || color != 4278190080 || z != 1 {
		t.Errorf("got text=%q font=%q x=%d color=%d z=%d", text, font, x, color, z)
	}
}

func TestApplyBatch_TextBoxTombstoneLWW(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const tb1 = "00000000000000000000000TB1"
	mk := func(opSeq, wall int64, text string, deletedAt any) Op {
		return Op{
			Table: "text_box", PK: tb1, SiteID: siteA, OpSeq: opSeq, WallTS: wall,
			Cols: map[string]any{
				"page_id": pg1, "x": float64(0), "y": float64(0), "width": float64(10),
				"height": float64(10), "text": text, "font_name": "", "font_size": float64(100),
				"color": float64(4278190080), "weight": float64(400), "border_width": float64(0),
				"z": float64(0), "created_at": float64(1000), "deleted_at": deletedAt,
			},
		}
	}
	if _, err := s.ApplyBatch(ctx, siteA, []Op{
		mk(1, 1000, "live", nil),
		mk(2, 2000, "live", float64(2000)), // tombstone wins (newer)
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var del sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT deleted_at FROM fn_text_box WHERE id = ?`, tb1).Scan(&del); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !del.Valid || del.Int64 != 2000 {
		t.Errorf("deleted_at = %+v, want 2000 (newer tombstone wins LWW)", del)
	}
}

func TestOpsSince_ExcludesSelfAndPages(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// site A authors 3 notebook revisions
	_, err := s.ApplyBatch(ctx, siteA, []Op{
		notebookOp(siteA, 1, 1000, "v1", nil),
		notebookOp(siteA, 2, 2000, "v2", nil),
		notebookOp(siteA, 3, 3000, "v3", nil),
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	// site B pulls everything
	ops, cur, more, err := s.OpsSince(ctx, 0, siteB, 500)
	if err != nil {
		t.Fatalf("opssince B: %v", err)
	}
	if len(ops) != 3 || more || cur != 3 {
		t.Errorf("B pull: got %d ops more=%v cur=%d, want 3/false/3", len(ops), more, cur)
	}

	// site A excludes its own ops
	ops, _, _, err = s.OpsSince(ctx, 0, siteA, 500)
	if err != nil {
		t.Fatalf("opssince A: %v", err)
	}
	if len(ops) != 0 {
		t.Errorf("A pull: want 0 (self-excluded), got %d", len(ops))
	}

	// paging: limit 2 → 2 ops, has_more, cursor at 2nd seq
	ops, cur, more, err = s.OpsSince(ctx, 0, siteB, 2)
	if err != nil {
		t.Fatalf("opssince paged: %v", err)
	}
	if len(ops) != 2 || !more || cur != 2 {
		t.Errorf("paged pull: got %d ops more=%v cur=%d, want 2/true/2", len(ops), more, cur)
	}
}

// poison op mid-batch: 1 and 3 valid, 2 missing a column → 2 rejected. The water
// must reach 3 (1 present, 2 rejected, 3 present) — not stall at 1, not skip 2.
func TestAcceptedThrough_PoisonOpDoesNotWedge(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	poison := Op{Table: "notebook", PK: nb1, SiteID: siteA, OpSeq: 2, WallTS: 2000,
		Cols: map[string]any{"name": "x"}} // missing sort_order/created_at/deleted_at
	res, err := s.ApplyBatch(ctx, siteA, []Op{
		notebookOp(siteA, 1, 1000, "v1", nil),
		poison,
		notebookOp(siteA, 3, 3000, "v3", nil),
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.Rejected) != 1 || res.Rejected[0].OpSeq != 2 {
		t.Fatalf("want op 2 rejected, got %+v", res.Rejected)
	}
	if res.AcceptedThrough != 3 {
		t.Errorf("want accepted_through 3 (poison op 2 counted), got %d", res.AcceptedThrough)
	}
}

// a genuine gap (op never sent, never rejected) caps the water so the device
// keeps resending: send 1 and 3, skip 2 → water stops at 1.
func TestAcceptedThrough_GapCapsWater(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	res, err := s.ApplyBatch(ctx, siteA, []Op{
		notebookOp(siteA, 1, 1000, "v1", nil),
		notebookOp(siteA, 3, 3000, "v3", nil), // op_seq 2 missing
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.AcceptedThrough != 1 {
		t.Errorf("want accepted_through 1 (gap at 2 caps water), got %d", res.AcceptedThrough)
	}
}

// the high-water persists across calls: call 1 settles 1..2, call 2 sends 3 and
// must advance to 3 (not recompute from a non-existent op 1 in this batch).
func TestAcceptedThrough_PersistsAcrossCalls(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.ApplyBatch(ctx, siteA, []Op{
		notebookOp(siteA, 1, 1000, "v1", nil),
		notebookOp(siteA, 2, 2000, "v2", nil),
	}); err != nil {
		t.Fatalf("call1: %v", err)
	}
	res, err := s.ApplyBatch(ctx, siteA, []Op{notebookOp(siteA, 3, 3000, "v3", nil)})
	if err != nil {
		t.Fatalf("call2: %v", err)
	}
	if res.AcceptedThrough != 3 {
		t.Errorf("want accepted_through 3 across calls, got %d", res.AcceptedThrough)
	}
}

func TestLastSeq(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if seq, _ := s.LastSeq(ctx); seq != 0 {
		t.Errorf("fresh LastSeq = %d, want 0", seq)
	}
	s.ApplyBatch(ctx, siteA, []Op{notebookOp(siteA, 1, 1000, "x", nil)})
	if seq, _ := s.LastSeq(ctx); seq != 1 {
		t.Errorf("LastSeq after 1 op = %d, want 1", seq)
	}
}
