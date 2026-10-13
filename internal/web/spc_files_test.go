package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/logging"
)

func spcBrowserForTest(t *testing.T) *Handler {
	t.Helper()
	h := NewHandler(nil, nil, nil, nil, nil, "", "", slog.New(slog.NewTextHandler(io.Discard, nil)), logging.NewLogBroadcaster())
	h.SetSPCFileRoot(t.TempDir())
	return h
}

func TestSPCBrowserSixFoldersWithoutNoteSource(t *testing.T) {
	h := spcBrowserForTest(t)
	for _, name := range []string{".digests", ".uploads", "Document_CONFLICT_test", "NOTE/Note"} {
		if err := os.MkdirAll(filepath.Join(h.spcFileRoot, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(h.spcFileRoot, "NOTE/Note", "book.note"), []byte("note"), 0644); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/files/supernote", nil))
	if w.Code != 200 {
		t.Fatalf("root: %d %s", w.Code, w.Body.String())
	}
	for _, name := range spcFolders {
		if !strings.Contains(w.Body.String(), "?path="+name) {
			t.Errorf("missing %s", name)
		}
	}
	for _, name := range []string{".digests", ".uploads", "Document_CONFLICT_test"} {
		if strings.Contains(w.Body.String(), name) {
			t.Errorf("exposed %s", name)
		}
	}
	for _, name := range spcFolders {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/files/supernote?path="+name, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "type=\"file\"") {
			t.Errorf("folder %s: %d %s", name, w.Code, w.Body.String())
		}
		if name == "Note" && !strings.Contains(w.Body.String(), "book.note") {
			t.Error("wrapped Note not mapped")
		}
	}
}

func browserUpload(h *Handler, folder, name, content string) *httptest.ResponseRecorder {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	f, _ := mw.CreateFormFile("file", name)
	io.WriteString(f, content)
	mw.Close()
	req := httptest.NewRequest("POST", "/files/supernote/upload?path="+url.QueryEscape(folder), &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestSPCBrowserUploadDownloadAndNoOverwrite(t *testing.T) {
	h := spcBrowserForTest(t)
	notified := 0
	h.SetSPCFileNotifier(func(context.Context) error { notified++; return nil })
	w := browserUpload(h, "Document", "book & test.epub", "epub test bytes")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	w = browserUpload(h, "Document", "book & test.epub", "replacement")
	if w.Code != http.StatusConflict {
		t.Fatalf("overwrite: %d", w.Code)
	}
	req := httptest.NewRequest("GET", "/files/supernote/download?path="+url.QueryEscape("Document/book & test.epub"), nil)
	req.Header.Set("Range", "bytes=0-3")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent || w.Body.String() != "epub" {
		t.Fatalf("download: %d %q", w.Code, w.Body.String())
	}
	if notified != 1 {
		t.Fatalf("notifications: %d", notified)
	}
	entries, err := os.ReadDir(filepath.Join(h.spcFileRoot, "DOCUMENT", "Document"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("staging leaked: %v %v", entries, err)
	}
}

func TestSPCBrowserRejectsTraversalAndSymlinkEscape(t *testing.T) {
	h := spcBrowserForTest(t)
	os.MkdirAll(filepath.Join(h.spcFileRoot, "DOCUMENT", "Document"), 0755)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0600)
	if err := os.Symlink(outside, filepath.Join(h.spcFileRoot, "DOCUMENT", "Document", "escape")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../secret.txt", "Document/../../secret.txt", ".digests/secret.txt", "Document/escape/secret.txt"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/files/supernote/download?path="+url.QueryEscape(bad), nil))
		if w.Code == 200 || strings.Contains(w.Body.String(), "secret") {
			t.Errorf("read escaped via %s: %d", bad, w.Code)
		}
	}
	if w := browserUpload(h, "Document/escape", "new.epub", "bad"); w.Code < 400 {
		t.Errorf("upload escaped: %d", w.Code)
	}
	req := httptest.NewRequest("POST", "/files/supernote/upload?path=Document", nil)
	req.Header.Set("Origin", "https://evil.invalid")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Errorf("cross-origin: %d", w.Code)
	}
}
