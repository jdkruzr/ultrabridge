package syncstore

import (
	"context"
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
