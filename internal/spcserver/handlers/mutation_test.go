package handlers

import (
	"context"
	"fmt"
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

// recordingIndex records the calls made to each IndexStore method and optionally
// returns an error, to verify the handler drives the index seams best-effort.
type recordingIndex struct {
	deleted []string
	renamed [][2]string
	copied  [][2]string
	err     error
}

func (r *recordingIndex) Delete(_ context.Context, path string) error {
	r.deleted = append(r.deleted, path)
	return r.err
}
func (r *recordingIndex) Rename(_ context.Context, oldPath, newPath string) error {
	r.renamed = append(r.renamed, [2]string{oldPath, newPath})
	return r.err
}
func (r *recordingIndex) Copy(_ context.Context, srcPath, dstPath string) error {
	r.copied = append(r.copied, [2]string{srcPath, dstPath})
	return r.err
}

// recordingFileMover records RenameFile calls.
type recordingFileMover struct {
	renamed [][2]string
	err     error
}

func (r *recordingFileMover) RenameFile(_ context.Context, oldPath, newPath string) error {
	r.renamed = append(r.renamed, [2]string{oldPath, newPath})
	return r.err
}

// A successful delete also de-indexes: the handler calls both the FTS and the
// embedding deleter with the file's resolved absolute path.
func TestDeleteFolderDeindexes(t *testing.T) {
	root := t.TempDir()
	abs := writeFile(t, root, "Note/indexed.note", []byte("searchable"))
	h, reg := newMutationHandler(t, root)
	content := &recordingIndex{}
	embed := &recordingIndex{}
	h.ContentIndex = content
	h.EmbedIndex = embed
	id, err := reg.IDFor(context.Background(), abs)
	if err != nil {
		t.Fatal(err)
	}

	out := decodeMap(t, h.DeleteFolder, `{"equipmentNo":"SN078","id":"`+strconv.FormatInt(id, 10)+`"}`)
	if out["success"] != true {
		t.Fatalf("success = %v (%v)", out["success"], out)
	}
	if len(content.deleted) != 1 || content.deleted[0] != abs {
		t.Fatalf("content deleted = %v, want [%q]", content.deleted, abs)
	}
	if len(embed.deleted) != 1 || embed.deleted[0] != abs {
		t.Fatalf("embed deleted = %v, want [%q]", embed.deleted, abs)
	}
}

// De-index is best-effort: a deleter error is logged, not propagated, so the
// device still gets a success envelope (the file is already recycled).
func TestDeleteFolderDeindexErrorIsBestEffort(t *testing.T) {
	root := t.TempDir()
	abs := writeFile(t, root, "Note/indexed.note", []byte("searchable"))
	h, reg := newMutationHandler(t, root)
	h.ContentIndex = &recordingIndex{err: errDeindexBoom}
	h.EmbedIndex = &recordingIndex{err: errDeindexBoom}
	id, _ := reg.IDFor(context.Background(), abs)

	out := decodeMap(t, h.DeleteFolder, `{"equipmentNo":"SN078","id":"`+strconv.FormatInt(id, 10)+`"}`)
	if out["success"] != true {
		t.Fatalf("de-index failure must not fail the delete; got %v", out)
	}
}

var errDeindexBoom = fmt.Errorf("boom")

// move_v3 repoints the search/RAG index + notes inventory from the old path to
// the new path (it does not delete or copy).
func TestMoveReindexes(t *testing.T) {
	root := t.TempDir()
	src := writeFile(t, root, "Note/m.note", []byte("move me"))
	if err := os.MkdirAll(filepath.Join(root, "Document"), 0o755); err != nil {
		t.Fatal(err)
	}
	h, reg := newMutationHandler(t, root)
	content := &recordingIndex{}
	embed := &recordingIndex{}
	files := &recordingFileMover{}
	h.ContentIndex, h.EmbedIndex, h.FileRecords = content, embed, files
	id, _ := reg.IDFor(context.Background(), src)
	dst := filepath.Join(root, "Document", "m.note")

	out := decodeMap(t, h.Move,
		`{"equipmentNo":"SN078","id":"`+strconv.FormatInt(id, 10)+`","to_path":"/Document/m.note"}`)
	if out["success"] != true {
		t.Fatalf("move success = %v (%v)", out["success"], out)
	}
	want := [2]string{src, dst}
	for name, got := range map[string][][2]string{"content": content.renamed, "embed": embed.renamed, "files": files.renamed} {
		if len(got) != 1 || got[0] != want {
			t.Fatalf("%s renamed = %v, want [[%q %q]]", name, got, src, dst)
		}
	}
	if len(content.deleted) != 0 || len(content.copied) != 0 {
		t.Fatalf("move must not delete/copy the index: deleted=%v copied=%v", content.deleted, content.copied)
	}
}

// copy_v3 duplicates the source's index entries to the destination so the copy
// is searchable (it does not rename or delete).
func TestCopyReindexes(t *testing.T) {
	root := t.TempDir()
	src := writeFile(t, root, "Note/c.note", []byte("copy me"))
	if err := os.MkdirAll(filepath.Join(root, "Document"), 0o755); err != nil {
		t.Fatal(err)
	}
	h, reg := newMutationHandler(t, root)
	content := &recordingIndex{}
	embed := &recordingIndex{}
	h.ContentIndex, h.EmbedIndex = content, embed
	id, _ := reg.IDFor(context.Background(), src)
	dst := filepath.Join(root, "Document", "c.note")

	out := decodeMap(t, h.Copy,
		`{"equipmentNo":"SN078","id":"`+strconv.FormatInt(id, 10)+`","to_path":"/Document/c.note"}`)
	if out["success"] != true {
		t.Fatalf("copy success = %v (%v)", out["success"], out)
	}
	want := [2]string{src, dst}
	if len(content.copied) != 1 || content.copied[0] != want {
		t.Fatalf("content copied = %v, want [[%q %q]]", content.copied, src, dst)
	}
	if len(embed.copied) != 1 || embed.copied[0] != want {
		t.Fatalf("embed copied = %v, want [[%q %q]]", embed.copied, src, dst)
	}
	if len(content.renamed) != 0 || len(content.deleted) != 0 {
		t.Fatalf("copy must not rename/delete: renamed=%v deleted=%v", content.renamed, content.deleted)
	}
}

// De-index failures on move/copy are best-effort: the device still gets success.
func TestMoveCopyReindexBestEffort(t *testing.T) {
	root := t.TempDir()
	src := writeFile(t, root, "Note/x.note", []byte("x"))
	if err := os.MkdirAll(filepath.Join(root, "Document"), 0o755); err != nil {
		t.Fatal(err)
	}
	h, reg := newMutationHandler(t, root)
	h.ContentIndex = &recordingIndex{err: errDeindexBoom}
	h.EmbedIndex = &recordingIndex{err: errDeindexBoom}
	h.FileRecords = &recordingFileMover{err: errDeindexBoom}
	id, _ := reg.IDFor(context.Background(), src)
	out := decodeMap(t, h.Move,
		`{"equipmentNo":"SN078","id":"`+strconv.FormatInt(id, 10)+`","to_path":"/Document/x.note"}`)
	if out["success"] != true {
		t.Fatalf("reindex failure must not fail the move; got %v", out)
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

// TestDeleteFolderPrunesEmptiedUserFolder: deleting a folder's last file removes
// the now-empty user folder too, so it can't re-sync ("zombie") back to the
// device (the device deletes folder *contents* by file id and never sends a
// folder delete; hardware-confirmed 2026-05-26). Native buckets are preserved.
func TestDeleteFolderPrunesEmptiedUserFolder(t *testing.T) {
	root := t.TempDir()
	abs := writeFile(t, root, "NOTE/Note/Moffitt/note.note", []byte("bye"))
	h, reg := newMutationHandler(t, root)
	id, _ := reg.IDFor(context.Background(), abs)

	out := decodeMap(t, h.DeleteFolder, `{"equipmentNo":"SN078","id":"`+strconv.FormatInt(id, 10)+`"}`)
	if out["success"] != true {
		t.Fatalf("success = %v (%v)", out["success"], out)
	}
	// The emptied user folder is gone...
	if _, err := os.Stat(filepath.Join(root, "NOTE", "Note", "Moffitt")); !os.IsNotExist(err) {
		t.Errorf("emptied user folder should be pruned, stat err=%v", err)
	}
	// ...but the native bucket subtree is preserved.
	if _, err := os.Stat(filepath.Join(root, "NOTE", "Note")); err != nil {
		t.Errorf("native bucket NOTE/Note must be preserved, stat err=%v", err)
	}
}

// TestDeleteFolderKeepsNonEmptyFolder: deleting one of several files leaves the
// (still non-empty) folder in place.
func TestDeleteFolderKeepsNonEmptyFolder(t *testing.T) {
	root := t.TempDir()
	a := writeFile(t, root, "NOTE/Note/Moffitt/a.note", []byte("a"))
	writeFile(t, root, "NOTE/Note/Moffitt/b.note", []byte("b"))
	h, reg := newMutationHandler(t, root)
	id, _ := reg.IDFor(context.Background(), a)

	decodeMap(t, h.DeleteFolder, `{"equipmentNo":"SN078","id":"`+strconv.FormatInt(id, 10)+`"}`)
	if _, err := os.Stat(filepath.Join(root, "NOTE", "Note", "Moffitt")); err != nil {
		t.Errorf("non-empty folder must be kept, stat err=%v", err)
	}
}

// TestDeleteFolderDoesNotPruneBucket: deleting a file directly under a native
// bucket subdir (NOTE/Note) must NOT prune that bucket.
func TestDeleteFolderDoesNotPruneBucket(t *testing.T) {
	root := t.TempDir()
	abs := writeFile(t, root, "NOTE/Note/loose.note", []byte("x"))
	h, reg := newMutationHandler(t, root)
	id, _ := reg.IDFor(context.Background(), abs)

	decodeMap(t, h.DeleteFolder, `{"equipmentNo":"SN078","id":"`+strconv.FormatInt(id, 10)+`"}`)
	if _, err := os.Stat(filepath.Join(root, "NOTE", "Note")); err != nil {
		t.Errorf("native bucket NOTE/Note must never be pruned, stat err=%v", err)
	}
}

// TestMovePrunesEmptiedSourceFolder: moving the last note out of a user folder
// prunes the emptied source folder (same zombie risk as delete). Native buckets
// are preserved.
func TestMovePrunesEmptiedSourceFolder(t *testing.T) {
	root := t.TempDir()
	src := writeFile(t, root, "NOTE/Note/Src/m.note", []byte("move me"))
	h, reg := newMutationHandler(t, root)
	id, _ := reg.IDFor(context.Background(), src)

	out := decodeMap(t, h.Move,
		`{"equipmentNo":"SN078","id":"`+strconv.FormatInt(id, 10)+`","to_path":"/NOTE/Note/m.note"}`)
	if out["success"] != true {
		t.Fatalf("move success = %v (%v)", out["success"], out)
	}
	if _, err := os.Stat(filepath.Join(root, "NOTE", "Note", "Src")); !os.IsNotExist(err) {
		t.Errorf("emptied source folder should be pruned, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "NOTE", "Note", "m.note")); err != nil {
		t.Errorf("moved note should be at destination, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "NOTE", "Note")); err != nil {
		t.Errorf("native bucket NOTE/Note must be preserved, stat err=%v", err)
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

	// to_path is the FULL destination path (incl. filename), not a parent dir.
	out := decodeMap(t, h.Move,
		`{"equipmentNo":"SN078","id":"`+strconv.FormatInt(id, 10)+`","to_path":"/Document/m.note"}`)
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
		`{"equipmentNo":"SN078","id":"`+strconv.FormatInt(id, 10)+`","to_path":"/Document/c.note"}`)
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

	// autorename=false → E0322 (to_path is the full destination path)
	out := decodeMap(t, h.Copy, `{"equipmentNo":"SN078","id":"`+idStr+`","to_path":"/Document/dup.note","autorename":false}`)
	if out["success"] != false || out["errorCode"] != errSameNameCode {
		t.Fatalf("collision w/o autorename: want E0322, got %v", out)
	}
	// autorename=true → "dup (1).note"
	out = decodeMap(t, h.Copy, `{"equipmentNo":"SN078","id":"`+idStr+`","to_path":"/Document/dup.note","autorename":true}`)
	if out["success"] != true {
		t.Fatalf("collision w/ autorename should succeed, got %v", out)
	}
	if e := entryOf(t, out); e["path_display"] != "/Document/dup (1).note" {
		t.Fatalf("autorename path_display = %v, want /Document/dup (1).note", e["path_display"])
	}
}
