package mapping

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/spcserver/fileids"
)

func newReg(t *testing.T, root string) *fileids.Registry {
	t.Helper()
	ctx := context.Background()
	db, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := fileids.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return fileids.New(db, root)
}

// TestEntryForFile verifies all EntriesVO fields for a regular file.
func TestEntryForFile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Note"), 0o755); err != nil {
		t.Fatal(err)
	}
	fp := filepath.Join(root, "Note", "foo.note")
	if err := os.WriteFile(fp, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := newReg(t, root)

	e, err := EntryFor(ctx, root, fp, reg)
	if err != nil {
		t.Fatalf("EntryFor: %v", err)
	}
	if e.Tag != "file" {
		t.Errorf("Tag = %q; want file", e.Tag)
	}
	if e.Name != "foo.note" {
		t.Errorf("Name = %q; want foo.note", e.Name)
	}
	if e.Size != 5 {
		t.Errorf("Size = %d; want 5", e.Size)
	}
	if !e.IsDownloadable {
		t.Errorf("IsDownloadable = false; want true for a file")
	}
	if e.PathDisplay != "/Note/foo.note" {
		t.Errorf("PathDisplay = %q; want /Note/foo.note", e.PathDisplay)
	}
	if e.ParentPath != "/Note" {
		t.Errorf("ParentPath = %q; want /Note", e.ParentPath)
	}
	if e.ContentHash == "" {
		t.Errorf("ContentHash empty; want MD5 for a file")
	}
	if e.LastUpdateTime <= 0 {
		t.Errorf("LastUpdateTime = %d; want >0", e.LastUpdateTime)
	}
	if e.ID == "" || e.ID == "0" {
		t.Errorf("ID = %q; want a positive registry id", e.ID)
	}
}

// TestEntryForFolder verifies a directory entry: size 0, not downloadable, empty hash.
func TestEntryForFolder(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dir := filepath.Join(root, "Note")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := newReg(t, root)

	e, err := EntryFor(ctx, root, dir, reg)
	if err != nil {
		t.Fatalf("EntryFor: %v", err)
	}
	if e.Tag != "folder" {
		t.Errorf("Tag = %q; want folder", e.Tag)
	}
	if e.Size != 0 {
		t.Errorf("Size = %d; want 0 for a folder", e.Size)
	}
	if e.IsDownloadable {
		t.Errorf("IsDownloadable = true; want false for a folder")
	}
	if e.ContentHash != "" {
		t.Errorf("ContentHash = %q; want empty for a folder", e.ContentHash)
	}
	if e.PathDisplay != "/Note" || e.ParentPath != "/" {
		t.Errorf("PathDisplay/ParentPath = %q/%q; want /Note and /", e.PathDisplay, e.ParentPath)
	}
}

// TestEntryForRoot verifies the root maps to path_display "/".
func TestEntryForRoot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	reg := newReg(t, root)
	e, err := EntryFor(ctx, root, root, reg)
	if err != nil {
		t.Fatalf("EntryFor root: %v", err)
	}
	if e.Tag != "folder" || e.PathDisplay != "/" {
		t.Errorf("root entry Tag/PathDisplay = %q/%q; want folder and /", e.Tag, e.PathDisplay)
	}
}

// TestSafeResolveCleansAndContains verifies double-slash tolerance and ..-escape rejection.
func TestSafeResolveCleansAndContains(t *testing.T) {
	root := t.TempDir()

	clean, err := SafeResolve(root, "/Note/Personal")
	if err != nil {
		t.Fatalf("SafeResolve clean: %v", err)
	}
	dirty, err := SafeResolve(root, "/Note//Personal")
	if err != nil {
		t.Fatalf("SafeResolve dirty: %v", err)
	}
	if clean != dirty {
		t.Errorf("double-slash resolved differently: %q vs %q", clean, dirty)
	}
	if clean != filepath.Join(root, "Note", "Personal") {
		t.Errorf("SafeResolve = %q; want %q", clean, filepath.Join(root, "Note", "Personal"))
	}

	if _, err := SafeResolve(root, "/../escape"); err == nil {
		t.Errorf("SafeResolve(../escape) should reject traversal, got nil error")
	}
	if _, err := SafeResolve(root, "/Note/../../escape"); err == nil {
		t.Errorf("SafeResolve nested traversal should reject, got nil error")
	}

	// Root itself resolves to the root.
	got, err := SafeResolve(root, "/")
	if err != nil || got != filepath.Clean(root) {
		t.Errorf("SafeResolve(/) = %q, %v; want %q", got, err, filepath.Clean(root))
	}
}
