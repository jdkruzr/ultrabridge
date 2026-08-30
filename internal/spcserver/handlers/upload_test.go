package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/spcserver/dto"
	"github.com/sysop/ultrabridge/internal/spcserver/fileids"
	"github.com/sysop/ultrabridge/internal/spcserver/oss"
	"github.com/sysop/ultrabridge/internal/spcserver/staging"
)

// newUploadHandler builds an UploadHandler over a temp root sharing one notedb
// (both fileids + staging migrated) with a fixed-secret signer.
func newUploadHandler(t *testing.T, root string) *UploadHandler {
	t.Helper()
	ctx := context.Background()
	db, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := fileids.Migrate(ctx, db); err != nil {
		t.Fatalf("fileids Migrate: %v", err)
	}
	if err := staging.Migrate(ctx, db); err != nil {
		t.Fatalf("staging Migrate: %v", err)
	}
	signer := &oss.Signer{Secret: testOssSecret}
	return &UploadHandler{
		Root:    root,
		Reg:     fileids.New(db, root),
		Signer:  signer,
		Staging: &staging.Store{Root: root, DB: db},
	}
}

// uploadStreamReq builds the multipart POST the device sends to /api/oss/upload,
// carrying the signed query lifted from a fullUploadUrl.
func uploadStreamReq(t *testing.T, fullUploadURL string, body []byte) *http.Request {
	t.Helper()
	u, err := url.Parse(fullUploadURL)
	if err != nil {
		t.Fatalf("parse fullUploadUrl %q: %v", fullUploadURL, err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "upload.bin")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close mw: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/oss/upload?"+u.RawQuery, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func uploadPartReq(t *testing.T, partUploadURL, uploadID string, partNumber, totalChunks int, body []byte) *http.Request {
	t.Helper()
	u, err := url.Parse(partUploadURL)
	if err != nil {
		t.Fatalf("parse partUploadUrl %q: %v", partUploadURL, err)
	}
	q := u.Query()
	q.Set("uploadId", uploadID)
	q.Set("partNumber", strconv.Itoa(partNumber))
	q.Set("totalChunks", strconv.Itoa(totalChunks))
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "upload.bin")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close mw: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/oss/upload/part?"+q.Encode(), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// AC3.1: apply mints an innerName + a fullUploadUrl whose signature validates and
// whose path decrypts back to the innerName.
func TestUploadApplyMintsValidURL(t *testing.T) {
	h := newUploadHandler(t, t.TempDir())
	out := decodeMap(t, h.Apply, `{"equipmentNo":"SN078","path":"/Note","fileName":"foo.note","size":"5"}`)

	if out["success"] != true {
		t.Fatalf("success = %v", out["success"])
	}
	inner, _ := out["innerName"].(string)
	full, _ := out["fullUploadUrl"].(string)
	part, _ := out["partUploadUrl"].(string)
	if inner == "" || full == "" || part == "" {
		t.Fatalf("innerName=%q fullUploadUrl=%q partUploadUrl=%q", inner, full, part)
	}
	u, err := url.Parse(full)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	q := u.Query()
	ts, _ := strconv.ParseInt(q.Get("timestamp"), 10, 64)
	if !h.Signer.ValidateUpload(q.Get("signature"), ts, q.Get("nonce"), q.Get("path"), 0) {
		t.Fatalf("minted upload signature does not validate")
	}
	dec, err := oss.DecryptPath(q.Get("path"))
	if err != nil || dec != inner {
		t.Fatalf("path decrypts to %q (err %v), want innerName %q", dec, err, inner)
	}
	partURL, err := url.Parse(part)
	if err != nil || partURL.Path != "/api/oss/upload/part" || partURL.RawQuery != u.RawQuery {
		t.Fatalf("partUploadUrl = %q, want signed /api/oss/upload/part matching full URL", part)
	}
	if out["bucketName"] != "foo.note" || out["xAmzDate"] == "" || out["authorization"] != q.Get("signature") {
		t.Fatalf("incomplete apply metadata: %#v", out)
	}
}

func TestChunkedUploadRoundTrip(t *testing.T) {
	root := t.TempDir()
	h := newUploadHandler(t, root)
	body := []byte("first-second-third")
	apply := decodeMap(t, h.Apply,
		`{"equipmentNo":"SN078","path":"/NOTE/Note/big.note","fileName":"big.note","size":"`+strconv.Itoa(len(body))+`"}`)
	inner := apply["innerName"].(string)
	partURL := apply["partUploadUrl"].(string)

	chunks := map[int][]byte{1: []byte("first-"), 2: []byte("second-"), 3: []byte("third")}
	for _, number := range []int{2, 1, 3} {
		rec := httptest.NewRecorder()
		h.UploadPart(rec, uploadPartReq(t, partURL, "upload-123", number, 3, chunks[number]))
		if rec.Code != http.StatusOK {
			t.Fatalf("part %d status = %d, body %q", number, rec.Code, rec.Body.String())
		}
		var response dto.FileChunkVO
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("part %d response: %v", number, err)
		}
		if !response.Success || response.UploadID != "upload-123" || response.PartNumber != number || response.TotalParts != 3 || response.ChunkMD5 != md5Hex(t, chunks[number]) {
			t.Fatalf("part %d response = %#v", number, response)
		}
	}

	finishBody, _ := json.Marshal(map[string]any{
		"equipmentNo": "SN078", "path": "/NOTE/Note/", "fileName": "big.note",
		"size": strconv.Itoa(len(body)), "content_hash": md5Hex(t, body), "innerName": inner,
	})
	out := decodeMap(t, h.Finish, string(finishBody))
	if out["success"] != true {
		t.Fatalf("finish response = %#v", out)
	}
	got, err := os.ReadFile(filepath.Join(root, "NOTE", "Note", "big.note"))
	if err != nil {
		t.Fatalf("read promoted file: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("promoted bytes = %q, want %q", got, body)
	}
}

// AC3.2 + AC3.3: apply → oss/upload → finish round-trips; the file lands at its
// target under FILE_ROOT with matching md5, and finish returns a populated VO.
func TestUploadRoundTrip(t *testing.T) {
	root := t.TempDir()
	h := newUploadHandler(t, root)
	body := []byte("supernote upload bytes")
	md5sum := md5Hex(t, body)

	apply := decodeMap(t, h.Apply,
		`{"equipmentNo":"SN078","path":"/Note","fileName":"foo.note","size":"`+strconv.Itoa(len(body))+`"}`)
	inner := apply["innerName"].(string)
	full := apply["fullUploadUrl"].(string)

	// oss/upload
	rec := httptest.NewRecorder()
	h.UploadStream(rec, uploadStreamReq(t, full, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("oss/upload status = %d, body %q", rec.Code, rec.Body.String())
	}

	// finish
	finishBody, _ := json.Marshal(map[string]any{
		"equipmentNo": "SN078", "path": "/Note", "fileName": "foo.note",
		"size": strconv.Itoa(len(body)), "content_hash": md5sum, "innerName": inner,
	})
	out := decodeMap(t, h.Finish, string(finishBody))
	if out["success"] != true {
		t.Fatalf("finish success = %v, body %v", out["success"], out)
	}
	if out["content_hash"] != md5sum {
		t.Fatalf("finish content_hash = %v, want %s", out["content_hash"], md5sum)
	}
	if out["id"] == "" || out["id"] == nil {
		t.Fatalf("finish id empty")
	}

	// File landed at the target with the right bytes.
	got, err := os.ReadFile(filepath.Join(root, "Note", "foo.note"))
	if err != nil {
		t.Fatalf("read promoted file: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("promoted bytes mismatch")
	}
}

// fakeEnqueuer records the paths handed to the OCR pipeline.
type fakeEnqueuer struct{ paths []string }

func (f *fakeEnqueuer) Enqueue(_ context.Context, path string) error {
	f.paths = append(f.paths, path)
	return nil
}

// roundTrip drives apply→upload→finish for fileName under /Note and returns the
// finish response.
func roundTrip(t *testing.T, h *UploadHandler, fileName string, body []byte) map[string]any {
	t.Helper()
	apply := decodeMap(t, h.Apply,
		`{"equipmentNo":"SN078","path":"/Note","fileName":"`+fileName+`","size":"`+strconv.Itoa(len(body))+`"}`)
	inner := apply["innerName"].(string)
	full := apply["fullUploadUrl"].(string)
	rec := httptest.NewRecorder()
	h.UploadStream(rec, uploadStreamReq(t, full, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("oss/upload status = %d for %s", rec.Code, fileName)
	}
	finishBody, _ := json.Marshal(map[string]any{
		"equipmentNo": "SN078", "path": "/Note", "fileName": fileName,
		"size": strconv.Itoa(len(body)), "content_hash": md5Hex(t, body), "innerName": inner,
	})
	return decodeMap(t, h.Finish, string(finishBody))
}

// AC5.1: a finished .note upload under the watched tree is enqueued for OCR; a
// .txt (or other non-note) upload is not.
func TestUploadKicksOCRForNotes(t *testing.T) {
	root := t.TempDir()
	h := newUploadHandler(t, root)
	fe := &fakeEnqueuer{}
	h.Enqueuer = fe
	h.OCRWatchDir = root // /Note is under root

	if out := roundTrip(t, h, "kick.note", []byte("note bytes")); out["success"] != true {
		t.Fatalf("note finish failed: %v", out)
	}
	if out := roundTrip(t, h, "skip.txt", []byte("text bytes")); out["success"] != true {
		t.Fatalf("txt finish failed: %v", out)
	}

	if len(fe.paths) != 1 {
		t.Fatalf("enqueued %d paths, want 1 (%v)", len(fe.paths), fe.paths)
	}
	if filepath.Base(fe.paths[0]) != "kick.note" {
		t.Fatalf("enqueued %q, want the .note", fe.paths[0])
	}
}

// AC5.1: a .note uploaded outside OCRWatchDir is not enqueued.
func TestUploadSkipsOCROutsideWatchDir(t *testing.T) {
	root := t.TempDir()
	h := newUploadHandler(t, root)
	fe := &fakeEnqueuer{}
	h.Enqueuer = fe
	h.OCRWatchDir = filepath.Join(root, "Document") // /Note is NOT under here

	if out := roundTrip(t, h, "outside.note", []byte("x")); out["success"] != true {
		t.Fatalf("finish failed: %v", out)
	}
	if len(fe.paths) != 0 {
		t.Fatalf("should not enqueue outside watch dir, got %v", fe.paths)
	}
}

// AC3.2: a tampered signature is refused with HTTP 500 + plain text and stages nothing.
func TestUploadStreamRejectsBadSignature(t *testing.T) {
	root := t.TempDir()
	h := newUploadHandler(t, root)
	body := []byte("x")
	apply := decodeMap(t, h.Apply, `{"equipmentNo":"SN078","path":"/Note","fileName":"f.note","size":"1"}`)
	full := apply["fullUploadUrl"].(string)

	// Flip the signature.
	u, _ := url.Parse(full)
	q := u.Query()
	q.Set("signature", "deadbeef")
	u.RawQuery = q.Encode()

	rec := httptest.NewRecorder()
	h.UploadStream(rec, uploadStreamReq(t, u.String(), body))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("bad-sig status = %d, want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain;charset=UTF-8" {
		t.Fatalf("bad-sig content-type = %q", ct)
	}
	// Nothing staged.
	entries, _ := os.ReadDir(filepath.Join(root, ".staging"))
	if len(entries) != 0 {
		t.Fatalf("expected nothing staged, got %d entries", len(entries))
	}
}

// AC3.3: finish with a wrong content_hash fails and leaves the target absent.
func TestUploadFinishRejectsMd5Mismatch(t *testing.T) {
	root := t.TempDir()
	h := newUploadHandler(t, root)
	body := []byte("real bytes")
	apply := decodeMap(t, h.Apply,
		`{"equipmentNo":"SN078","path":"/Note","fileName":"bar.note","size":"`+strconv.Itoa(len(body))+`"}`)
	inner := apply["innerName"].(string)
	full := apply["fullUploadUrl"].(string)

	rec := httptest.NewRecorder()
	h.UploadStream(rec, uploadStreamReq(t, full, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("oss/upload status = %d", rec.Code)
	}

	finishBody, _ := json.Marshal(map[string]any{
		"equipmentNo": "SN078", "path": "/Note", "fileName": "bar.note",
		"size": strconv.Itoa(len(body)), "content_hash": "00000000000000000000000000000000", "innerName": inner,
	})
	out := decodeMap(t, h.Finish, string(finishBody))
	if out["success"] != false {
		t.Fatalf("finish should fail on md5 mismatch, got %v", out)
	}
	if out["errorCode"] != errUploadFailedCode {
		t.Fatalf("errorCode = %v, want %s", out["errorCode"], errUploadFailedCode)
	}
	if _, err := os.Stat(filepath.Join(root, "Note", "bar.note")); !os.IsNotExist(err) {
		t.Fatalf("target should be absent after failed finish, err=%v", err)
	}
}
