package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// TestListFolderExcludesDotDirs: UB's own .staging/.recycle dot-dirs under the
// root must never surface to the device (flat or recursive).
func TestListFolderExcludesDotDirs(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	for _, d := range []string{".staging", ".recycle"} {
		if err := os.MkdirAll(filepath.Join(root, d, "junk"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, d, "junk", "x.note"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := newFileHandler(t, root)

	// Flat root listing still has exactly the 3 native top-level entries.
	flat := entriesOf(t, decodeMap(t, h.ListFolder, `{"equipmentNo":"SN078"}`))
	if len(flat) != 3 {
		t.Fatalf("flat entries = %d; want 3 (dot-dirs leaked: %v)", len(flat), names(flat))
	}
	// Recursive listing must not include anything under a dot-dir.
	rec := entriesOf(t, decodeMap(t, h.ListFolder, `{"equipmentNo":"SN078","recursive":true}`))
	for _, e := range rec {
		pd, _ := e["path_display"].(string)
		if strings.Contains(pd, "/.") || strings.HasPrefix(pd, ".") {
			t.Errorf("recursive listing leaked a dot-path: %q", pd)
		}
	}
	if len(rec) != 6 {
		t.Fatalf("recursive entries = %d; want 6 (dot-dirs leaked: %v)", len(rec), names(rec))
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

func TestQueryByPathResolvesDigestSourceAliases(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"NOTE/Note/Personal", "DOCUMENT/Document"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "NOTE/Note/Personal/dinner.note"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "DOCUMENT/Document/book.pdf"), []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newFileHandler(t, root)

	for _, tc := range []struct {
		path string
		want string
	}{
		{"Note/Personal/dinner.note", "/NOTE/Note/Personal/dinner.note"},
		{"/Document/book.pdf", "/DOCUMENT/Document/book.pdf"},
	} {
		out := decodeMap(t, h.QueryByPath, `{"path":"`+tc.path+`"}`)
		e, ok := out["entriesVO"].(map[string]any)
		if !ok {
			t.Fatalf("entriesVO missing for alias %q: %v", tc.path, out["entriesVO"])
		}
		if e["path_display"] != tc.want {
			t.Errorf("alias %q path_display = %v; want %s", tc.path, e["path_display"], tc.want)
		}
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

// TestCapacityQuery: usedCapacity is the du-sum, totalCapacity the quota.
func TestCapacityQuery(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.bin"), make([]byte, 123), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newFileHandler(t, root)
	out := decodeMap(t, h.CapacityQuery, `{}`)
	if out["success"] != true {
		t.Fatalf("success = %v", out["success"])
	}
	if out["usedCapacity"].(float64) != 123 {
		t.Errorf("usedCapacity = %v; want 123", out["usedCapacity"])
	}
	if int64(out["totalCapacity"].(float64)) != 1<<40 {
		t.Errorf("totalCapacity = %v; want 1 TiB", out["totalCapacity"])
	}
}

// TestGetSpaceUsage: used = du-sum, allocationVO carries the quota + tag.
func TestGetSpaceUsage(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.bin"), make([]byte, 500), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newFileHandler(t, root)
	out := decodeMap(t, h.GetSpaceUsage, `{"equipmentNo":"SN078"}`)
	if out["success"] != true || out["equipmentNo"] != "SN078" {
		t.Fatalf("got %v", out)
	}
	if out["used"].(float64) != 500 {
		t.Errorf("used = %v; want 500", out["used"])
	}
	alloc, ok := out["allocationVO"].(map[string]any)
	if !ok {
		t.Fatalf("allocationVO missing: %v", out["allocationVO"])
	}
	if alloc["tag"] != "individual" {
		t.Errorf("allocationVO.tag = %v; want individual", alloc["tag"])
	}
	if int64(alloc["allocated"].(float64)) != 1<<40 {
		t.Errorf("allocationVO.allocated = %v; want 1 TiB", alloc["allocated"])
	}
}

// TestCreateFolderV2_CreatesAndReturnsMetadata: create_folder_v2 mkdir's the
// folder under FileRoot and returns metadata{tag,id,name,path_display} — the
// device needs the server-assigned id to then upload notes into the folder
// (without it the device aborts the sync; wire-confirmed 2026-05-26).
func TestCreateFolderV2_CreatesAndReturnsMetadata(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "NOTE", "Note"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newFileHandler(t, root)
	out := decodeMap(t, h.CreateFolderV2, `{"equipmentNo":"SN078","path":"/NOTE/Note/Moffitt","autorename":false}`)

	if out["success"] != true || out["equipmentNo"] != "SN078" {
		t.Fatalf("got %v; want success:true equipmentNo:SN078", out)
	}
	abs := filepath.Join(root, "NOTE", "Note", "Moffitt")
	fi, err := os.Stat(abs)
	if err != nil || !fi.IsDir() {
		t.Fatalf("folder not created on disk: stat err=%v", err)
	}
	md, ok := out["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("response missing metadata object: %v", out)
	}
	if md["tag"] != "folder" || md["name"] != "Moffitt" || md["path_display"] != "/NOTE/Note/Moffitt" {
		t.Errorf("metadata wrong: %v", md)
	}
	id, _ := md["id"].(string)
	if id == "" || id == "0" {
		t.Errorf("metadata.id must be a non-zero folder id, got %q", md["id"])
	}
	// The id must match what the registry assigns the same path, so a later
	// list_folder reports the identical id.
	want, err := h.Reg.IDFor(context.Background(), abs)
	if err != nil || md["id"] != strconv.FormatInt(want, 10) {
		t.Errorf("metadata.id %v != registry id %d (err=%v)", md["id"], want, err)
	}
}

// TestCreateFolderV2_ExistsNoAutorename_E0322: a collision with autorename=false
// returns E0322 (matches the real SPC server).
func TestCreateFolderV2_ExistsNoAutorename_E0322(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "NOTE", "Note", "Moffitt"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newFileHandler(t, root)
	out := decodeMap(t, h.CreateFolderV2, `{"equipmentNo":"SN078","path":"/NOTE/Note/Moffitt","autorename":false}`)

	if out["success"] != false || out["errorCode"] != "E0322" {
		t.Errorf("collision w/o autorename: got %v; want success:false errorCode:E0322", out)
	}
}

// TestCreateFolderV2_ExistsAutorename_Idempotent: a collision with autorename=true
// is idempotent — returns the existing folder's metadata (UB does not yet
// replicate the real server's rename-with-suffix scheme; the device queries by
// path first and only sends autorename=false, so this branch is defensive).
func TestCreateFolderV2_ExistsAutorename_Idempotent(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "NOTE", "Note", "Moffitt")
	if err := os.MkdirAll(abs, 0o755); err != nil {
		t.Fatal(err)
	}
	h := newFileHandler(t, root)
	want, _ := h.Reg.IDFor(context.Background(), abs)

	out := decodeMap(t, h.CreateFolderV2, `{"equipmentNo":"SN078","path":"/NOTE/Note/Moffitt","autorename":true}`)
	if out["success"] != true {
		t.Fatalf("autorename collision should succeed: %v", out)
	}
	md, _ := out["metadata"].(map[string]any)
	if md == nil || md["id"] != strconv.FormatInt(want, 10) {
		t.Errorf("expected existing folder id %d, got %v", want, out["metadata"])
	}
}

// TestQueryDeleteApiStub: query/deleteApi returns success with a null entry.
func TestQueryDeleteApiStub(t *testing.T) {
	h := newFileHandler(t, t.TempDir())
	out := decodeMap(t, h.QueryByIDDeleteAPI, `{"equipmentNo":"SN078","id":"5"}`)
	if out["success"] != true {
		t.Errorf("success = %v; want true", out["success"])
	}
	if out["entriesVO"] != nil {
		t.Errorf("entriesVO = %v; want null", out["entriesVO"])
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
