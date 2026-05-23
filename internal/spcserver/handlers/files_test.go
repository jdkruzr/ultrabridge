package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/spcserver/capacity"
	"github.com/sysop/ultrabridge/internal/spcserver/fileids"
)

// newFileHandler builds a FileHandler over a temp root with a fresh in-memory registry.
func newFileHandler(t *testing.T, root string) *FileHandler {
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
	return &FileHandler{
		Root:  root,
		Reg:   fileids.New(db, root),
		Meter: capacity.New(root, 1<<40),
	}
}

// decodeMap runs a handler and unmarshals its JSON response into a generic map
// (postJSON, returning the recorder, lives in login_test.go).
func decodeMap(t *testing.T, fn http.HandlerFunc, body string) map[string]any {
	t.Helper()
	rec := postJSON(t, fn, body)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal response %q: %v", rec.Body.String(), err)
	}
	return out
}

// TestSynchronousStartSynTypeTrue: a root with a top-level folder → synType true.
func TestSynchronousStartSynTypeTrue(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Note"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newFileHandler(t, root)
	out := decodeMap(t, h.SynchronousStart, `{"equipmentNo":"SN078"}`)

	if out["success"] != true {
		t.Errorf("success = %v; want true", out["success"])
	}
	if out["equipmentNo"] != "SN078" {
		t.Errorf("equipmentNo = %v; want SN078", out["equipmentNo"])
	}
	if out["synType"] != true {
		t.Errorf("synType = %v; want true (root has a folder)", out["synType"])
	}
}

// TestSynchronousStartSynTypeFalse: an empty root → synType false.
func TestSynchronousStartSynTypeFalse(t *testing.T) {
	h := newFileHandler(t, t.TempDir())
	out := decodeMap(t, h.SynchronousStart, `{"equipmentNo":"SN078"}`)
	if out["success"] != true {
		t.Errorf("success = %v; want true", out["success"])
	}
	if out["synType"] != false {
		t.Errorf("synType = %v; want false (empty root)", out["synType"])
	}
}

// TestSynchronousEnd echoes equipmentNo with success.
func TestSynchronousEnd(t *testing.T) {
	h := newFileHandler(t, t.TempDir())
	out := decodeMap(t, h.SynchronousEnd, `{"equipmentNo":"SN078","flag":"1"}`)
	if out["success"] != true || out["equipmentNo"] != "SN078" {
		t.Errorf("got %v; want success:true equipmentNo:SN078", out)
	}
}

// buildTree lays out: /Note (dir), /Note/Personal (dir), /Note/foo.note (file),
// /Document (dir), /a.txt (file).
func buildTree(t *testing.T, root string) {
	t.Helper()
	for _, d := range []string{"Note/Personal", "Document"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"Note/foo.note", "a.txt", "Note/Personal/p.note"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// entriesOf decodes a list_folder response's entries into a slice of maps.
func entriesOf(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	raw, ok := out["entries"].([]any)
	if !ok {
		t.Fatalf("entries missing or wrong type: %v", out["entries"])
	}
	var es []map[string]any
	for _, e := range raw {
		es = append(es, e.(map[string]any))
	}
	return es
}

// TestListFolderRootNilID: a null id lists the root, folders before files.
func TestListFolderRootNilID(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	h := newFileHandler(t, root)

	out := decodeMap(t, h.ListFolder, `{"equipmentNo":"SN078"}`)
	if out["success"] != true {
		t.Fatalf("success = %v", out["success"])
	}
	es := entriesOf(t, out)
	// Top-level: Document (folder), Note (folder), a.txt (file) → folders first.
	if len(es) != 3 {
		t.Fatalf("entries = %d; want 3 (%v)", len(es), es)
	}
	if es[0]["tag"] != "folder" || es[1]["tag"] != "folder" || es[2]["tag"] != "file" {
		t.Errorf("expected folders before files, got tags %v/%v/%v", es[0]["tag"], es[1]["tag"], es[2]["tag"])
	}
	if es[2]["name"] != "a.txt" {
		t.Errorf("last entry name = %v; want a.txt", es[2]["name"])
	}
}

// TestListFolderByID: listing a subfolder's id returns its children.
func TestListFolderByID(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	h := newFileHandler(t, root)

	id, err := h.Reg.IDFor(context.Background(), filepath.Join(root, "Note"))
	if err != nil {
		t.Fatal(err)
	}
	out := decodeMap(t, h.ListFolder, `{"equipmentNo":"SN078","id":`+itoa(id)+`}`)
	es := entriesOf(t, out)
	// Note/ contains Personal (folder) + foo.note (file).
	if len(es) != 2 {
		t.Fatalf("entries = %d; want 2 (%v)", len(es), es)
	}
	if es[0]["name"] != "Personal" || es[1]["name"] != "foo.note" {
		t.Errorf("got names %v/%v; want Personal/foo.note", es[0]["name"], es[1]["name"])
	}
}

// TestListFolderRecursive: recursive flattens the whole subtree.
func TestListFolderRecursive(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	h := newFileHandler(t, root)

	out := decodeMap(t, h.ListFolder, `{"equipmentNo":"SN078","recursive":true}`)
	es := entriesOf(t, out)
	// All descendants: Document, Note, Note/Personal, Note/Personal/p.note, Note/foo.note, a.txt = 6.
	if len(es) != 6 {
		t.Fatalf("recursive entries = %d; want 6 (%v)", len(es), names(es))
	}
}

// TestListFolderUnknownID: an unknown id → empty entries, success.
func TestListFolderUnknownID(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	h := newFileHandler(t, root)
	out := decodeMap(t, h.ListFolder, `{"equipmentNo":"SN078","id":999999}`)
	if out["success"] != true {
		t.Errorf("success = %v; want true", out["success"])
	}
	if es, _ := out["entries"].([]any); len(es) != 0 {
		t.Errorf("entries = %v; want empty for unknown id", es)
	}
}

// TestListFolderV3SameLogic: the v3 alias behaves like list_folder.
func TestListFolderV3SameLogic(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	h := newFileHandler(t, root)
	out := decodeMap(t, h.ListFolderV3, `{"equipmentNo":"SN078"}`)
	if len(entriesOf(t, out)) != 3 {
		t.Errorf("v3 root listing should match list_folder (3 entries)")
	}
}

// TestQueryByIDFound: query_v3 with a known id returns the entry.
func TestQueryByIDFound(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	h := newFileHandler(t, root)

	id, _ := h.Reg.IDFor(context.Background(), filepath.Join(root, "Note", "foo.note"))
	out := decodeMap(t, h.QueryByID, `{"equipmentNo":"SN078","id":"`+itoa(id)+`"}`)
	if out["success"] != true {
		t.Fatalf("success = %v", out["success"])
	}
	e, ok := out["entriesVO"].(map[string]any)
	if !ok {
		t.Fatalf("entriesVO missing: %v", out["entriesVO"])
	}
	if e["name"] != "foo.note" || e["tag"] != "file" {
		t.Errorf("entry = %v; want foo.note/file", e)
	}
}

// TestQueryByIDMissing: unknown/unparseable id → success with null entriesVO.
func TestQueryByIDMissing(t *testing.T) {
	h := newFileHandler(t, t.TempDir())
	for _, body := range []string{`{"id":"999999"}`, `{"id":"not-a-number"}`, `{"id":""}`} {
		out := decodeMap(t, h.QueryByID, body)
		if out["success"] != true {
			t.Errorf("body %s: success = %v; want true", body, out["success"])
		}
		if out["entriesVO"] != nil {
			t.Errorf("body %s: entriesVO = %v; want null", body, out["entriesVO"])
		}
	}
}

// TestQueryByPathFoundAndDoubleSlash: by/path_v3 resolves a path, tolerating
// the device's double slashes.
func TestQueryByPathFoundAndDoubleSlash(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	h := newFileHandler(t, root)

	out := decodeMap(t, h.QueryByPath, `{"equipmentNo":"SN078","path":"/Note//Personal/p.note"}`)
	e, ok := out["entriesVO"].(map[string]any)
	if !ok {
		t.Fatalf("entriesVO missing for double-slash path: %v", out["entriesVO"])
	}
	if e["path_display"] != "/Note/Personal/p.note" {
		t.Errorf("path_display = %v; want /Note/Personal/p.note", e["path_display"])
	}
}

// TestQueryByPathMissingAndEscape: a non-existent path and a traversal attempt
// both return success with null entriesVO (never a 500).
func TestQueryByPathMissingAndEscape(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	h := newFileHandler(t, root)

	for _, p := range []string{"/Note/nope.note", "/../escape"} {
		out := decodeMap(t, h.QueryByPath, `{"path":"`+p+`"}`)
		if out["success"] != true {
			t.Errorf("path %s: success = %v; want true", p, out["success"])
		}
		if out["entriesVO"] != nil {
			t.Errorf("path %s: entriesVO = %v; want null", p, out["entriesVO"])
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func names(es []map[string]any) []string {
	var out []string
	for _, e := range es {
		out = append(out, e["path_display"].(string))
	}
	return out
}
