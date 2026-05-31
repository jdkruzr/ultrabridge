package web

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sysop/ultrabridge/internal/fnpath"
	"github.com/sysop/ultrabridge/internal/taskattach"
)

// sanitizeFilename strips control characters (incl. CR/LF), double-quotes, and
// backslashes from an untrusted filename before it goes into a quoted
// Content-Disposition value, preventing header corruption / injection.
func sanitizeFilename(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == '"' || r == '\\' {
			return -1
		}
		return r
	}, s)
}

// SetTaskAttach wires the optional task-ATTACH serving surface (signer + blob
// store + externally-reachable base URL). Set from main.go after construction,
// mirroring SetDigestService. baseURL is the public origin used to build the
// absolute signed URLs embedded in VTODO ATTACH properties (KeyBooxExternalBaseURL);
// it may be empty, in which case the CalDAV layer emits relative fetch paths.
func (h *Handler) SetTaskAttach(signer *taskattach.Signer, store *taskattach.BlobStore, baseURL string) {
	h.attachSigner = signer
	h.attachStore = store
	for len(baseURL) > 0 && baseURL[len(baseURL)-1] == '/' {
		baseURL = baseURL[:len(baseURL)-1]
	}
	h.attachBaseURL = baseURL
}

// HandleAttachmentDownload serves inline-binary attachment bytes from the
// content store. Public (no auth): the path segment is the content sha256 and
// the request must carry a signature over ("attach", sha). Range requests are
// supported via http.ServeContent. Mounted on the top-level mux in main.go so
// it bypasses the auth middleware — the signature is the only guard.
func (h *Handler) HandleAttachmentDownload(w http.ResponseWriter, r *http.Request) {
	if h.attachStore == nil || h.attachSigner == nil {
		http.NotFound(w, r)
		return
	}
	sha := r.PathValue("id")
	q := r.URL.Query()
	sig := q.Get("sig")
	if sha == "" || sig == "" || !h.attachSigner.Valid(sig, "attach", sha) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	f, _, err := h.attachStore.Open(sha)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	// fmttype/filename ride as unsigned cosmetic params — the holder already
	// proved a valid signature over the content sha, so mislabeling content to
	// themselves is harmless; we just don't trust them for anything else.
	if ct := q.Get("type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	name := sanitizeFilename(q.Get("name"))
	if name != "" {
		w.Header().Set("Content-Disposition", "inline; filename=\""+name+"\"")
	} else {
		name = sha
	}
	// Content is immutable (content-addressed), so a long cache is safe.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, name, time.Time{}, f)
}

// HandleFNRenderSigned renders a ForestNote page to JPEG behind a URL
// signature instead of UB's auth middleware, so a third-party CalDAV client
// can open the handwriting-page attachment UB emits. Mounted on the top-level
// mux in main.go (public). Mirrors handleForestNoteRender but sig-gated.
func (h *Handler) HandleFNRenderSigned(w http.ResponseWriter, r *http.Request) {
	if h.attachSigner == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	notePath := q.Get("path")
	sig := q.Get("sig")
	if notePath == "" || sig == "" || !h.attachSigner.Valid(sig, "fnrender", notePath) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !fnpath.Is(notePath) {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	stream, ct, err := h.notes.RenderPage(r.Context(), notePath, 0)
	if err != nil || stream == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer stream.Close()
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=300")
	io.Copy(w, stream)
}
