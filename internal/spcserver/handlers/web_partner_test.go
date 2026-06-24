package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPartnerSearchStaysInsideSPCFileRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Note"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Note", "Cambridge.note"), []byte("sn"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".convert"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".convert", "Cambridge-boox-leak.note"), []byte("hidden"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := newFileHandler(t, root)
	web := &WebFileHandler{Root: root, Reg: h.Reg}
	out := decodeMap(t, web.Search, `{"directoryId":"0","fileName":"cambridge"}`)
	es := entriesOf(t, out)
	if len(es) != 1 {
		t.Fatalf("search entries = %d; want 1 (%v)", len(es), names(es))
	}
	if es[0]["name"] != "Cambridge.note" {
		t.Fatalf("search returned %v; want only SPC visible file", es[0])
	}
}

func TestPartnerWebUploadFinishAndDownloadURL(t *testing.T) {
	root := t.TempDir()
	h := newUploadHandler(t, root)
	body := []byte("partner app bytes")
	md5sum := md5Hex(t, body)

	apply := decodeMap(t, h.WebApply, `{"directoryId":"0","fileName":"partner.note","fileSize":`+strconv.Itoa(len(body))+`}`)
	inner := apply["innerName"].(string)
	full := apply["fullUploadUrl"].(string)

	rec := httptest.NewRecorder()
	h.UploadStream(rec, uploadStreamReq(t, full, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("oss/upload status = %d body=%q", rec.Code, rec.Body.String())
	}

	finish := decodeMap(t, h.WebFinish, `{"directoryId":"0","fileName":"partner.note","fileSize":`+strconv.Itoa(len(body))+`,"innerName":"`+inner+`","md5":"`+md5sum+`"}`)
	if finish["success"] != true {
		t.Fatalf("web finish failed: %v", finish)
	}
	if got, err := os.ReadFile(filepath.Join(root, "partner.note")); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("promoted file mismatch: bytes=%q err=%v", got, err)
	}

	dl := &DownloadHandler{Root: root, Reg: h.Reg, Signer: h.Signer}
	id, err := h.Reg.IDFor(t.Context(), filepath.Join(root, "partner.note"))
	if err != nil {
		t.Fatal(err)
	}
	out := decodeMap(t, dl.WebDownloadURL, `{"id":"`+strconv.FormatInt(id, 10)+`"}`)
	if out["success"] != true || !strings.Contains(out["url"].(string), "/api/oss/download") || out["md5"] != md5sum {
		t.Fatalf("download/url response = %v", out)
	}
}

func TestPartnerRecycleRestore(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Note"), 0o755); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(root, "Note", "foo.note")
	if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, reg := newMutationHandler(t, root)
	id, err := reg.IDFor(t.Context(), abs)
	if err != nil {
		t.Fatal(err)
	}

	renamed := decodeMap(t, h.WebRename, `{"id":"`+strconv.FormatInt(id, 10)+`","newName":"renamed.note"}`)
	if renamed["success"] != true {
		t.Fatalf("rename failed: %v", renamed)
	}
	deleteOut := decodeMap(t, h.WebDelete, `{"idList":["`+strconv.FormatInt(id, 10)+`"]}`)
	if deleteOut["success"] != true {
		t.Fatalf("delete failed: %v", deleteOut)
	}
	if _, err := os.Stat(filepath.Join(root, "Note", "renamed.note")); !os.IsNotExist(err) {
		t.Fatalf("renamed file should be recycled, stat err=%v", err)
	}

	recycled := entriesOf(t, decodeMap(t, h.RecycleList, `{}`))
	if len(recycled) != 1 {
		t.Fatalf("recycle entries = %d; want 1 (%v)", len(recycled), recycled)
	}
	recycleID := recycled[0]["id"].(string)
	restore := decodeMap(t, h.RecycleRevert, `{"id":"`+recycleID+`"}`)
	if restore["success"] != true {
		t.Fatalf("restore failed: %v", restore)
	}
	if _, err := os.Stat(filepath.Join(root, "Note", "renamed.note")); err != nil {
		t.Fatalf("restored file missing: %v", err)
	}
}

func TestPartnerProbeEndpoints(t *testing.T) {
	for name, fn := range map[string]http.HandlerFunc{
		"baseParam":   SystemBaseParam,
		"publicKey":   EmailPublicKey,
		"emailConfig": EmailConfig,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
		fn(rec, req)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s unmarshal %q: %v", name, rec.Body.String(), err)
		}
		if out["success"] != true {
			t.Fatalf("%s success = %v; body=%v", name, out["success"], out)
		}
	}
}
