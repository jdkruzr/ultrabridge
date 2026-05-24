package handlers

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/spcserver/fileids"
)

func newMutationHandler(t *testing.T, root string) (*MutationHandler, *fileids.Registry) {
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
	reg := fileids.New(db, root)
	return &MutationHandler{Root: root, Reg: reg}, reg
}

// AC4.1: delete_folder_v3 soft-deletes — the file moves under .recycle/, leaves
// its original path, and the VO reports its metadata.
func TestDeleteFolderSoftDeletes(t *testing.T) {
	root := t.TempDir()
	noteDir := filepath.Join(root, "Note")
	if err := os.MkdirAll(noteDir, 0o755); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(noteDir, "doomed.note")
	if err := os.WriteFile(abs, []byte("bye"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, reg := newMutationHandler(t, root)
	id, err := reg.IDFor(context.Background(), abs)
	if err != nil {
		t.Fatal(err)
	}

	out := decodeMap(t, h.DeleteFolder, `{"equipmentNo":"SN078","id":"`+strconv.FormatInt(id, 10)+`"}`)
	if out["success"] != true {
		t.Fatalf("success = %v (%v)", out["success"], out)
	}
	meta, _ := out["metadata"].(map[string]any)
	if meta == nil || meta["name"] != "doomed.note" || meta["path_display"] != "/Note/doomed.note" {
		t.Fatalf("metadata = %v", meta)
	}

	// Original path is gone.
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Fatalf("original path should be gone, err=%v", err)
	}
	// A copy now lives somewhere under .recycle/.
	recycleRoot := filepath.Join(root, ".recycle")
	var found bool
	_ = filepath.Walk(recycleRoot, func(p string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() && info.Name() == "doomed.note" {
			found = true
		}
		return nil
	})
	if !found {
		t.Fatalf("deleted file not found under .recycle/")
	}
}

// AC4.1: an unknown id → success:false with E0318, never a 500.
func TestDeleteFolderUnknownID(t *testing.T) {
	root := t.TempDir()
	h, _ := newMutationHandler(t, root)
	out := decodeMap(t, h.DeleteFolder, `{"equipmentNo":"SN078","id":"999999"}`)
	if out["success"] != false {
		t.Fatalf("unknown id should fail softly, got %v", out)
	}
	if out["errorCode"] != errDeleteMissingCode {
		t.Fatalf("errorCode = %v, want %s", out["errorCode"], errDeleteMissingCode)
	}
}

func writeFile(t *testing.T, root, rel string, body []byte) string {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return abs
}

func entryOf(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	e, _ := out["entriesVO"].(map[string]any)
	if e == nil {
		t.Fatalf("no entriesVO in %v", out)
	}
	return e
}

// AC4.2: move_v3 relocates the file into to_path, original gone, id stable.
func TestMovePreservesID(t *testing.T) {
	root := t.TempDir()
	src := writeFile(t, root, "Note/m.note", []byte("move me"))
	if err := os.MkdirAll(filepath.Join(root, "Document"), 0o755); err != nil {
		t.Fatal(err)
	}
	h, reg := newMutationHandler(t, root)
	id, err := reg.IDFor(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}

	out := decodeMap(t, h.Move,
		`{"equipmentNo":"SN078","id":"`+strconv.FormatInt(id, 10)+`","to_path":"/Document"}`)
	if out["success"] != true {
		t.Fatalf("move success = %v (%v)", out["success"], out)
	}
	e := entryOf(t, out)
	if e["path_display"] != "/Document/m.note" {
		t.Fatalf("moved path_display = %v, want /Document/m.note", e["path_display"])
	}
	if e["id"] != strconv.FormatInt(id, 10) {
		t.Fatalf("moved id = %v, want stable %d", e["id"], id)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("original should be gone, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Document", "m.note")); err != nil {
		t.Fatalf("moved file missing at new path: %v", err)
	}
	// The stable id now resolves to the new path.
	if p, found, _ := reg.PathFor(context.Background(), id); !found || p != filepath.Join(root, "Document", "m.note") {
		t.Fatalf("id %d resolves to %q (found %v); want new path", id, p, found)
	}
}

// AC4.3: copy_v3 duplicates the file; the copy gets a fresh id; both exist.
func TestCopyFreshID(t *testing.T) {
	root := t.TempDir()
	src := writeFile(t, root, "Note/c.note", []byte("copy me"))
	if err := os.MkdirAll(filepath.Join(root, "Document"), 0o755); err != nil {
		t.Fatal(err)
	}
	h, reg := newMutationHandler(t, root)
	id, err := reg.IDFor(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}

	out := decodeMap(t, h.Copy,
		`{"equipmentNo":"SN078","id":"`+strconv.FormatInt(id, 10)+`","to_path":"/Document"}`)
	if out["success"] != true {
		t.Fatalf("copy success = %v (%v)", out["success"], out)
	}
	e := entryOf(t, out)
	if e["path_display"] != "/Document/c.note" {
		t.Fatalf("copy path_display = %v", e["path_display"])
	}
	if e["id"] == strconv.FormatInt(id, 10) {
		t.Fatalf("copy id should be fresh, got same %v", e["id"])
	}
	// Both files exist.
	for _, p := range []string{src, filepath.Join(root, "Document", "c.note")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected file at %q: %v", p, err)
		}
	}
}

// AC4.x: collision with autorename=false → E0322; with autorename=true → " (1)".
func TestCopyCollision(t *testing.T) {
	root := t.TempDir()
	src := writeFile(t, root, "Note/dup.note", []byte("orig"))
	writeFile(t, root, "Document/dup.note", []byte("existing"))
	h, reg := newMutationHandler(t, root)
	id, _ := reg.IDFor(context.Background(), src)
	idStr := strconv.FormatInt(id, 10)

	// autorename=false → E0322
	out := decodeMap(t, h.Copy, `{"equipmentNo":"SN078","id":"`+idStr+`","to_path":"/Document","autorename":false}`)
	if out["success"] != false || out["errorCode"] != errSameNameCode {
		t.Fatalf("collision w/o autorename: want E0322, got %v", out)
	}
	// autorename=true → "dup (1).note"
	out = decodeMap(t, h.Copy, `{"equipmentNo":"SN078","id":"`+idStr+`","to_path":"/Document","autorename":true}`)
	if out["success"] != true {
		t.Fatalf("collision w/ autorename should succeed, got %v", out)
	}
	if e := entryOf(t, out); e["path_display"] != "/Document/dup (1).note" {
		t.Fatalf("autorename path_display = %v, want /Document/dup (1).note", e["path_display"])
	}
}
