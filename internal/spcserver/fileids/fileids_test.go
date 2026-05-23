package fileids

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sysop/ultrabridge/internal/notedb"
)

// TestMigrateIdempotent verifies Migrate can run twice without error.
func TestMigrateIdempotent(t *testing.T) {
	ctx := context.Background()
	db, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

// TestIDForStableAndDistinct: same path → same positive id; distinct paths → distinct ids.
func TestIDForStableAndDistinct(t *testing.T) {
	ctx := context.Background()
	db, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	reg := New(db, "/root")

	a1, err := reg.IDFor(ctx, "/root/Note/foo.note")
	if err != nil {
		t.Fatalf("IDFor a: %v", err)
	}
	a2, err := reg.IDFor(ctx, "/root/Note/foo.note")
	if err != nil {
		t.Fatalf("IDFor a again: %v", err)
	}
	if a1 != a2 {
		t.Errorf("same path got different ids: %d vs %d", a1, a2)
	}
	if a1 <= 0 {
		t.Errorf("expected positive id, got %d", a1)
	}
	b, err := reg.IDFor(ctx, "/root/Document/bar.pdf")
	if err != nil {
		t.Fatalf("IDFor b: %v", err)
	}
	if b == a1 {
		t.Errorf("distinct paths got same id: %d", b)
	}
}

// TestIDForCleansPath: a non-clean path (double slash) maps to the same id as its cleaned form.
func TestIDForCleansPath(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	reg := New(db, "/root")

	clean, _ := reg.IDFor(ctx, "/root/Note/Personal")
	dirty, _ := reg.IDFor(ctx, "/root/Note//Personal")
	if clean != dirty {
		t.Errorf("double-slash path got a different id: clean=%d dirty=%d", clean, dirty)
	}
}

// TestPathForRoundtrip: PathFor(IDFor(p)) == p; unknown id → found=false.
func TestPathForRoundtrip(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	reg := New(db, "/root")

	want := "/root/Note/foo.note"
	id, _ := reg.IDFor(ctx, want)
	got, found, err := reg.PathFor(ctx, id)
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	if !found || got != want {
		t.Errorf("PathFor(%d) = %q, %v; want %q, true", id, got, found, want)
	}

	if _, found, err := reg.PathFor(ctx, 999999); err != nil || found {
		t.Errorf("PathFor(unknown) = found=%v err=%v; want found=false err=nil", found, err)
	}
}

// TestNewRegistersRoot: the root path is assigned an id at construction and resolvable.
func TestNewRegistersRoot(t *testing.T) {
	ctx := context.Background()
	db, _ := notedb.Open(ctx, ":memory:")
	defer db.Close()
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	reg := New(db, "/root")
	rid, err := reg.RootID(ctx)
	if err != nil {
		t.Fatalf("RootID: %v", err)
	}
	got, found, _ := reg.PathFor(ctx, rid)
	if !found || got != "/root" {
		t.Errorf("RootID %d resolves to %q (found=%v); want /root", rid, got, found)
	}
}

// TestIDForPersistsAcrossReopen: ids survive a fresh Registry over the same DB file (AC1.2).
func TestIDForPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ids.db")

	db1, err := notedb.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	if err := Migrate(ctx, db1); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	reg1 := New(db1, "/root")
	id1, _ := reg1.IDFor(ctx, "/root/Note/foo.note")
	db1.Close()

	db2, err := notedb.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	defer db2.Close()
	reg2 := New(db2, "/root")
	id2, _ := reg2.IDFor(ctx, "/root/Note/foo.note")
	if id1 != id2 {
		t.Errorf("id changed across reopen: %d vs %d", id1, id2)
	}
}
