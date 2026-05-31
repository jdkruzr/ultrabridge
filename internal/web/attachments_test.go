package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/logging"
	"github.com/sysop/ultrabridge/internal/taskattach"
)

// newAttachTestServer builds a Handler with task-attach serving wired and
// mounts the two public routes on a real mux (mirroring main.go) so routing,
// PathValue, and literal-vs-wildcard precedence are all exercised. Registering
// both patterns also asserts they don't conflict/panic.
func newAttachTestServer(t *testing.T) (*httptest.Server, *taskattach.Signer, *taskattach.BlobStore, *mockNoteService) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	notes := &mockNoteService{renders: map[string]io.ReadCloser{}}
	h := NewHandler(nil, notes, nil, nil, nil, "", "", logger, logging.NewLogBroadcaster())
	signer := &taskattach.Signer{Secret: "test-secret"}
	store := &taskattach.BlobStore{Root: t.TempDir()}
	h.SetTaskAttach(signer, store, "https://ub.example.com")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/attachments/fn-render", h.HandleFNRenderSigned)
	mux.HandleFunc("GET /api/v1/attachments/{id}", h.HandleAttachmentDownload)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, signer, store, notes
}

func TestHandleAttachmentDownload(t *testing.T) {
	srv, signer, store, _ := newAttachTestServer(t)
	data := []byte("the quick brown fox attachment payload")
	sha, _, err := store.Put(data)
	if err != nil {
		t.Fatal(err)
	}
	good := signer.Sign("attach", sha)
	base := srv.URL + "/api/v1/attachments/" + sha

	t.Run("valid signature serves bytes", func(t *testing.T) {
		resp, err := http.Get(base + "?sig=" + good)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(body) != string(data) {
			t.Errorf("body mismatch: %q", body)
		}
	})

	t.Run("bad signature is forbidden", func(t *testing.T) {
		resp, err := http.Get(base + "?sig=deadbeef")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("range request returns 206 partial", func(t *testing.T) {
		req, _ := http.NewRequest("GET", base+"?sig="+good, nil)
		req.Header.Set("Range", "bytes=0-3")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("status = %d, want 206", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "the " {
			t.Errorf("partial body = %q, want %q", body, "the ")
		}
	})

	t.Run("missing content is 404 even with valid sig", func(t *testing.T) {
		// Valid sig over a sha that was never Put.
		ghostSha := strings.Repeat("ab", 32)
		sig := signer.Sign("attach", ghostSha)
		resp, err := http.Get(srv.URL + "/api/v1/attachments/" + ghostSha + "?sig=" + sig)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("malicious filename sanitized in Content-Disposition", func(t *testing.T) {
		// A name with a quote + CRLF must not corrupt/inject the header.
		resp, err := http.Get(base + "?sig=" + good + "&name=" + url.QueryEscape("a\"b\r\nX: y.txt"))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		cd := resp.Header.Get("Content-Disposition")
		if cd != `inline; filename="abX: y.txt"` {
			t.Errorf("Content-Disposition not sanitized: %q", cd)
		}
	})
}

func TestHandleFNRenderSigned(t *testing.T) {
	srv, signer, _, notes := newAttachTestServer(t)
	notePath := "forestnote://n1/pgA"
	notes.renders[notePath] = io.NopCloser(strings.NewReader("\xff\xd8\xff jpeg bytes"))
	good := signer.Sign("fnrender", notePath)
	base := srv.URL + "/api/v1/attachments/fn-render"

	t.Run("valid sig renders the page (precedence over {id})", func(t *testing.T) {
		resp, err := http.Get(base + "?path=" + url.QueryEscape(notePath) + "&sig=" + good)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d, want 200 (fn-render must win over {id})", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
			t.Errorf("Content-Type = %q, want image/jpeg", ct)
		}
	})

	t.Run("bad sig forbidden", func(t *testing.T) {
		resp, err := http.Get(base + "?path=" + url.QueryEscape(notePath) + "&sig=nope")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("non-fnpath with valid sig is 400", func(t *testing.T) {
		bad := "http://evil.example.com/x"
		sig := signer.Sign("fnrender", bad)
		resp, err := http.Get(base + "?path=" + url.QueryEscape(bad) + "&sig=" + sig)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("valid sig + fnpath but render unavailable is 404", func(t *testing.T) {
		other := "forestnote://n2/pgZ" // valid fnpath, not in the mock's renders
		sig := signer.Sign("fnrender", other)
		resp, err := http.Get(base + "?path=" + url.QueryEscape(other) + "&sig=" + sig)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})
}
