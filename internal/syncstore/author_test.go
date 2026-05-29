package syncstore

import (
	"context"
	"database/sql"
	"testing"
)

// authorNotebookOp builds a full-row notebook upsert for AuthorOps. SiteID/OpSeq/
// WallTS are left zero on purpose — AuthorOps fills them; pre-setting them is a
// caller error.
func authorNotebookOp(name string, deletedAt any) Op {
	return Op{
		Table: "notebook", PK: nb1,
		Cols: map[string]any{"name": name, "sort_order": float64(0), "created_at": float64(1000), "deleted_at": deletedAt, "folder_id": nil},
	}
}

func TestNewULIDIsValid(t *testing.T) {
	for i := 0; i < 1000; i++ {
		id := newULID()
		if !IsULID(id) {
			t.Fatalf("newULID produced non-ULID %q", id)
		}
	}
}

func TestMigrateSeedsStableSiteID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id, err := s.SiteID(ctx)
	if err != nil {
		t.Fatalf("SiteID: %v", err)
	}
	if !IsULID(id) {
		t.Fatalf("seeded site_id %q is not a ULID", id)
	}
	// Re-running Migrate must not re-mint (INSERT OR IGNORE keeps the first).
	if err := Migrate(ctx, s.db); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	id2, err := s.SiteID(ctx)
	if err != nil {
		t.Fatalf("SiteID after re-migrate: %v", err)
	}
	if id2 != id {
		t.Errorf("site_id changed across migrate: %q -> %q", id, id2)
	}
}

// AuthorOps must materialize the mirror AND record a relayable op carrying UB's
// own ULID site_id, so a device (any site != UB) pulls it while UB itself does not.
func TestAuthorOps_MaterializesAndRelays(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	ubSite, err := s.SiteID(ctx)
	if err != nil {
		t.Fatalf("SiteID: %v", err)
	}

	changed, err := s.AuthorOps(ctx, []Op{authorNotebookOp("Authored", nil)})
	if err != nil {
		t.Fatalf("AuthorOps: %v", err)
	}
	_ = changed // notebook ops report no changed page

	// Mirror materialized with UB provenance.
	var name, lwwSite string
	var lwwOp int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT name, lww_op_seq, lww_site_id FROM fn_notebook WHERE id = ?`, nb1).
		Scan(&name, &lwwOp, &lwwSite); err != nil {
		t.Fatalf("query mirror: %v", err)
	}
	if name != "Authored" || lwwOp != 1 || lwwSite != ubSite {
		t.Errorf("mirror = name=%q op=%d site=%q, want Authored/1/%s", name, lwwOp, lwwSite, ubSite)
	}

	// A device pulling (excludeSite = some other site) sees the op.
	ops, _, _, err := s.OpsSince(ctx, 0, siteA, 500)
	if err != nil {
		t.Fatalf("OpsSince device: %v", err)
	}
	if len(ops) != 1 || ops[0].SiteID != ubSite || ops[0].Table != "notebook" || ops[0].PK != nb1 {
		t.Fatalf("device pull = %+v, want one notebook op authored by %s", ops, ubSite)
	}

	// UB pulling its own relay (excludeSite = UB) sees nothing.
	self, _, _, err := s.OpsSince(ctx, 0, ubSite, 500)
	if err != nil {
		t.Fatalf("OpsSince self: %v", err)
	}
	if len(self) != 0 {
		t.Errorf("UB self-pull = %d ops, want 0 (own ops excluded)", len(self))
	}
}

// The op_seq counter must increment monotonically and persist across calls so a
// crash can neither reuse nor skip a sequence number.
func TestAuthorOps_OpSeqMonotonicAcrossCalls(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.AuthorOps(ctx, []Op{
		authorNotebookOp("v1", nil),
		authorNotebookOp("v2", nil),
	}); err != nil {
		t.Fatalf("AuthorOps call1: %v", err)
	}
	if _, err := s.AuthorOps(ctx, []Op{authorNotebookOp("v3", nil)}); err != nil {
		t.Fatalf("AuthorOps call2: %v", err)
	}

	ubSite, _ := s.SiteID(ctx)
	rows, err := s.db.QueryContext(ctx,
		`SELECT op_seq FROM sync_ops WHERE site_id = ? ORDER BY op_seq`, ubSite)
	if err != nil {
		t.Fatalf("query op_seqs: %v", err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, n)
	}
	want := []int64{1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("op_seqs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("op_seqs = %v, want %v", got, want)
		}
	}

	// Counter persisted at high-water 3.
	var last int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT last_op_seq FROM sync_site WHERE id = 1`).Scan(&last); err != nil {
		t.Fatalf("read last_op_seq: %v", err)
	}
	if last != 3 {
		t.Errorf("persisted last_op_seq = %d, want 3", last)
	}
}

// A losing pre-set field must not corrupt provenance: AuthorOps overwrites any
// caller-supplied SiteID/OpSeq/WallTS with UB's own.
func TestAuthorOps_OverwritesCallerProvenance(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ubSite, _ := s.SiteID(ctx)

	op := authorNotebookOp("x", nil)
	op.SiteID = siteA // caller mistake
	op.OpSeq = 999
	op.WallTS = 1
	if _, err := s.AuthorOps(ctx, []Op{op}); err != nil {
		t.Fatalf("AuthorOps: %v", err)
	}

	var site string
	var opSeq, wall int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT site_id, op_seq, wall_ts FROM sync_ops WHERE table_name = 'notebook'`).
		Scan(&site, &opSeq, &wall); err != nil {
		t.Fatalf("query: %v", err)
	}
	if site != ubSite || opSeq != 1 || wall == 1 {
		t.Errorf("recorded site=%q op_seq=%d wall=%d, want UB/1/now (not caller values)", site, opSeq, wall)
	}
}

// Phase 1 round-trip plumbing: a UB-side notebook delete must AUTHOR tombstones
// (notebook + pages + strokes + each page's recognized-text row, deleted_at set)
// that the relay then carries to a device. This proves the wire path; the actual
// device apply is a hardware test.
func TestSoftDeleteNotebook_RelaysTombstones(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ubSite, _ := s.SiteID(ctx)

	// A device (siteA) creates a notebook with a page and a stroke.
	if _, err := s.ApplyBatch(ctx, siteA, []Op{
		nbInFolder(1, 1000, nbA, "NB", "", nil),
		pageOp(2, 2000, pgA, nbA, 0, nil),
		strokeOnPage(3, 2010, st1, pgA, nil),
	}); err != nil {
		t.Fatalf("device apply: %v", err)
	}

	// Everything the device authored sits at global seq 1..3.
	cursorBefore, err := s.LastSeq(ctx)
	if err != nil {
		t.Fatalf("last seq: %v", err)
	}

	if _, err := s.SoftDeleteNotebook(ctx, nbA); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	// The device pulls everything authored AFTER its own ops: exactly the four
	// tombstones (notebook, page, stroke, and the page's recognized-text row), all
	// authored by UB, all with deleted_at set.
	ops, _, _, err := s.OpsSince(ctx, cursorBefore, siteA, 500)
	if err != nil {
		t.Fatalf("OpsSince device: %v", err)
	}
	byTable := map[string]Op{}
	for _, op := range ops {
		if op.SiteID != ubSite {
			t.Errorf("relayed op authored by %q, want UB %q", op.SiteID, ubSite)
		}
		if op.Cols["deleted_at"] == nil {
			t.Errorf("%s/%s relayed without deleted_at set: %+v", op.Table, op.PK, op.Cols)
		}
		byTable[op.Table] = op
	}
	if len(ops) != 4 || byTable["notebook"].PK != nbA || byTable["page"].PK != pgA ||
		byTable["stroke"].PK != st1 || byTable["page_text_from_server"].PK != pgA {
		t.Fatalf("relayed tombstones = %+v, want one each of notebook/page/stroke/page_text_from_server", ops)
	}
	// The stroke tombstone carries a full row (points round-tripped, not dropped).
	if _, ok := byTable["stroke"].Cols["points"].(string); !ok {
		t.Errorf("stroke tombstone missing base64 points: %+v", byTable["stroke"].Cols)
	}
}

// A server-authored text-box edit must update the mirror AND emit a relayable op
// (UB site_id) carrying the new text, so devices pull the change.
func TestEditTextBoxText_AuthorsAndUpdates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ubSite, _ := s.SiteID(ctx)
	const tb1 = "00000000000000000000000TB1"

	// A device creates a page + a text box.
	if _, err := s.ApplyBatch(ctx, siteA, []Op{
		pageOp(1, 1000, pgA, nbA, 0, nil),
		{Table: "text_box", PK: tb1, SiteID: siteA, OpSeq: 2, WallTS: 1010,
			Cols: map[string]any{
				"page_id": pgA, "x": float64(1), "y": float64(2), "width": float64(100), "height": float64(50),
				"text": "before", "font_name": "Roboto-Regular.ttf", "font_size": float64(200),
				"color": float64(4278190080), "weight": float64(400), "border_width": float64(2),
				"z": float64(0), "created_at": float64(1000), "deleted_at": nil,
			}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cursorBefore, _ := s.LastSeq(ctx)

	pageID, err := s.EditTextBoxText(ctx, tb1, "after")
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if pageID != pgA {
		t.Errorf("page id = %q, want %q", pageID, pgA)
	}

	// Mirror updated, other columns preserved.
	var text, font string
	var width int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT text, font_name, width FROM fn_text_box WHERE id = ?`, tb1).Scan(&text, &font, &width); err != nil {
		t.Fatalf("query: %v", err)
	}
	if text != "after" || font != "Roboto-Regular.ttf" || width != 100 {
		t.Errorf("mirror = text=%q font=%q width=%d, want after/Roboto/100", text, font, width)
	}

	// A device pulls exactly the UB-authored edit op carrying the new text.
	ops, _, _, err := s.OpsSince(ctx, cursorBefore, siteA, 500)
	if err != nil {
		t.Fatalf("OpsSince: %v", err)
	}
	if len(ops) != 1 || ops[0].SiteID != ubSite || ops[0].Table != "text_box" || ops[0].Cols["text"] != "after" {
		t.Fatalf("relayed = %+v, want one UB text_box op with text=after", ops)
	}
}

func TestEditTextBoxText_MissingAndDeleted(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const tb1 = "00000000000000000000000TB1"

	if _, err := s.EditTextBoxText(ctx, tb1, "x"); err == nil {
		t.Error("editing a missing box should error")
	}

	// Create then delete the box; editing a tombstone must error.
	if _, err := s.ApplyBatch(ctx, siteA, []Op{
		pageOp(1, 1000, pgA, nbA, 0, nil),
		{Table: "text_box", PK: tb1, SiteID: siteA, OpSeq: 2, WallTS: 1010,
			Cols: map[string]any{
				"page_id": pgA, "x": float64(0), "y": float64(0), "width": float64(10), "height": float64(10),
				"text": "t", "font_name": "", "font_size": float64(100), "color": float64(4278190080),
				"weight": float64(400), "border_width": float64(0), "z": float64(0),
				"created_at": float64(1000), "deleted_at": float64(2000), // already deleted
			}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.EditTextBoxText(ctx, tb1, "x"); err == nil {
		t.Error("editing a deleted box should error")
	}
}

func TestListNotebookTextBoxes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const tb1, tb2 = "00000000000000000000000TB1", "00000000000000000000000TB2"
	box := func(seq, wall int64, pk, page, text string, del any) Op {
		return Op{Table: "text_box", PK: pk, SiteID: siteA, OpSeq: seq, WallTS: wall,
			Cols: map[string]any{
				"page_id": page, "x": float64(0), "y": float64(0), "width": float64(10), "height": float64(10),
				"text": text, "font_name": "", "font_size": float64(100), "color": float64(4278190080),
				"weight": float64(400), "border_width": float64(0), "z": float64(0),
				"created_at": float64(1000), "deleted_at": del,
			}}
	}
	if _, err := s.ApplyBatch(ctx, siteA, []Op{
		pageOp(1, 1000, pgA, nbA, 0, nil),
		box(2, 1010, tb1, pgA, "live one", nil),
		box(3, 1020, tb2, pgA, "deleted one", float64(1030)), // deleted → excluded
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	refs, err := s.ListNotebookTextBoxes(ctx, nbA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(refs) != 1 || refs[0].ID != tb1 || refs[0].PageID != pgA || refs[0].Text != "live one" {
		t.Fatalf("refs = %+v, want one live box tb1/pgA/'live one'", refs)
	}
}

// A malformed op (missing a known column) is rejected before anything is written.
func TestAuthorOps_RejectsMalformed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	bad := Op{Table: "notebook", PK: nb1, Cols: map[string]any{"name": "x"}} // missing cols
	if _, err := s.AuthorOps(ctx, []Op{bad}); err == nil {
		t.Fatal("AuthorOps accepted a malformed op, want error")
	}
	// Nothing committed: no op recorded, counter untouched.
	var nOps, last int64
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_ops`).Scan(&nOps)
	s.db.QueryRowContext(ctx, `SELECT last_op_seq FROM sync_site WHERE id = 1`).Scan(&last)
	if nOps != 0 || last != 0 {
		t.Errorf("after rejected batch: sync_ops=%d last_op_seq=%d, want 0/0 (rolled back)", nOps, last)
	}
}

// AuthorPageText must author a page_text_from_server op (UB site_id), materialize the
// mirror row, and relay it; a later re-OCR overwrites the text by LWW (op_seq breaks the
// same-ms wall_ts tie); a tombstone sets deleted_at. And authoring page text must NOT
// report any changed pages — that empty result is what keeps the bridge from re-OCRing
// in a loop (page_text rows are not page render input).
func TestAuthorPageText_AuthorMaterializeRelayReocrTombstone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ubSite, _ := s.SiteID(ctx)

	if err := s.AuthorPageText(ctx, pgA, "first pass", 1000, "modelX"); err != nil {
		t.Fatalf("author page text: %v", err)
	}

	readRow := func() (text, model string, del sql.NullInt64) {
		var m sql.NullString
		if err := s.db.QueryRowContext(ctx,
			`SELECT text, model, deleted_at FROM fn_page_text_from_server WHERE id = ?`, pgA).
			Scan(&text, &m, &del); err != nil {
			t.Fatalf("read page-text row: %v", err)
		}
		return text, m.String, del
	}

	if text, model, del := readRow(); text != "first pass" || model != "modelX" || del.Valid {
		t.Errorf("after author: row=(%q,%q,deleted=%v), want first pass/modelX/live", text, model, del.Valid)
	}

	// Relayed to a device under UB's site_id, live (deleted_at null).
	ops, _, _, err := s.OpsSince(ctx, 0, siteA, 500)
	if err != nil {
		t.Fatalf("OpsSince: %v", err)
	}
	if len(ops) != 1 || ops[0].Table != "page_text_from_server" || ops[0].PK != pgA || ops[0].SiteID != ubSite {
		t.Fatalf("relayed = %+v, want one page_text_from_server op for pgA by UB", ops)
	}
	if ops[0].Cols["deleted_at"] != nil || ops[0].Cols["text"] != "first pass" {
		t.Errorf("relayed cols = %+v, want live text 'first pass'", ops[0].Cols)
	}

	// Re-OCR: a second author on the same page wins (higher op_seq) and overwrites.
	if err := s.AuthorPageText(ctx, pgA, "second pass", 2000, "modelY"); err != nil {
		t.Fatalf("re-author page text: %v", err)
	}
	if text, model, del := readRow(); text != "second pass" || model != "modelY" || del.Valid {
		t.Errorf("after re-OCR: row=(%q,%q,deleted=%v), want second pass/modelY/live", text, model, del.Valid)
	}

	// Tombstone sets deleted_at.
	if err := s.AuthorPageTextTombstone(ctx, pgA); err != nil {
		t.Fatalf("tombstone page text: %v", err)
	}
	if _, _, del := readRow(); !del.Valid {
		t.Errorf("after tombstone: deleted_at must be set")
	}
}

// Loop-safety contract: authoring a page_text op returns NO changed pages, so the bridge
// is never re-enqueued for the page whose text was just authored.
func TestAuthorPageText_ReportsNoChangedPages(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	changed, err := s.AuthorOps(ctx, []Op{{
		Table: "page_text_from_server", PK: pgA,
		Cols: pageTextCols("hi", 1000, 1000, "m", nil),
	}})
	if err != nil {
		t.Fatalf("author ops: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("page_text author reported changed pages %+v, want none (loop-safety)", changed)
	}
}
