package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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
