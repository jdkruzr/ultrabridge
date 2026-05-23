package handlers

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/sysop/ultrabridge/internal/spcserver/dto"
	"github.com/sysop/ultrabridge/internal/spcserver/envelope"
	"github.com/sysop/ultrabridge/internal/spcserver/fileids"
	"github.com/sysop/ultrabridge/internal/spcserver/mapping"
	"github.com/sysop/ultrabridge/internal/spcserver/oss"
)

// errFileNotExist is FileErrorCodeEnum.E0321 ("This file does not exist"),
// returned by download_v3 when the requested id has no live file
// (FileLocalServiceImpl.getDownloadUrl, FileErrorCodeEnum.java:29).
const (
	errFileNotExistCode = "E0321"
	errFileNotExistMsg  = "This file does not exist"
)

// The /api/oss/download byte-stream errors are NOT JSON envelopes: real SPC's
// GlobalExceptionHandler maps FileDownloadException to a ResponseEntity<String>
// with HTTP 500 and the bare message as the body (GlobalExceptionHandler.java:127,
// O_OssLocalController.java:169). Match that exactly so the device behaves as
// against real SPC.
const (
	msgSignatureFailed = "Signature verification failed."
	msgDownloadFailed  = "File download failed."
)

// DownloadHandler serves the SPC download read-path (Phase 3): download_v3 and
// generate/download/url mint a presigned /api/oss/download URL pointing back at
// UB; the byte stream itself is served by DownloadStream (Task 5). It reads the
// filesystem under Root directly (same dedicated root as FileHandler) and
// signs/verifies with Signer. An empty Root makes download_v3 report E0321.
type DownloadHandler struct {
	Root   string
	Reg    *fileids.Registry
	Signer *oss.Signer
	Logger *slog.Logger
}

func (h *DownloadHandler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// DownloadV3 resolves a file id to a presigned download URL.
// POST /api/file/3/files/download_v3 (F_FileLocalController.java:153).
func (h *DownloadHandler) DownloadV3(w http.ResponseWriter, r *http.Request) {
	var req dto.FileDownloadLocalDTO
	_ = json.NewDecoder(r.Body).Decode(&req)

	notFound := func() {
		envelope.WriteJSON(w, dto.FileDownloadLocalVO{
			BaseVO:      envelope.BaseVO{Success: false, ErrorCode: errFileNotExistCode, ErrorMsg: errFileNotExistMsg},
			EquipmentNo: req.EquipmentNo,
		})
	}

	// The device sends id as a quoted string (§8 String-in/Long-out); parse it.
	id, perr := strconv.ParseInt(req.ID, 10, 64)
	if perr != nil || h.Root == "" {
		notFound()
		return
	}
	abs, found, err := h.Reg.PathFor(r.Context(), id)
	if err != nil {
		h.log().Error("download_v3 PathFor", "id", id, "err", err)
		notFound()
		return
	}
	if !found {
		notFound()
		return
	}
	if _, err := os.Lstat(abs); err != nil {
		// Registered id whose file was moved/deleted out from under us.
		notFound()
		return
	}

	entry, err := mapping.EntryFor(r.Context(), h.Root, abs, h.Reg)
	if err != nil {
		h.log().Error("download_v3 EntryFor", "path", abs, "err", err)
		notFound()
		return
	}

	url := h.signedDownloadURL(r, entry.PathDisplay, entry.ID)
	size := entry.Size
	envelope.WriteJSON(w, dto.FileDownloadLocalVO{
		BaseVO:         envelope.OK(),
		EquipmentNo:    req.EquipmentNo,
		ID:             entry.ID,
		URL:            url,
		Name:           entry.Name,
		PathDisplay:    entry.PathDisplay,
		ContentHash:    entry.ContentHash,
		Size:           &size,
		IsDownloadable: true,
	})
}

// GenerateDownloadURL is the OSS-direct URL minter (form/query params).
// POST /api/oss/generate/download/url (O_OssLocalController.java:152). Not hit
// by the device in the 0b capture; wired for completeness. Unlike download_v3
// it takes the path components directly rather than a file id.
func (h *DownloadHandler) GenerateDownloadURL(w http.ResponseWriter, r *http.Request) {
	filePath := r.FormValue("filePath")
	fileName := r.FormValue("fileName")
	pathID := r.FormValue("pathId")

	display := path.Join(filePath, fileName)
	encPath := oss.EncryptPath(display)
	ts := h.nowMillis()
	nonce := newNonce()
	sig := h.Signer.DownloadSignature(encPath, ts, nonce)
	envelope.WriteJSON(w, dto.FileDownloadApplyVO{
		URL:       h.downloadURL(r, encPath, sig, ts, nonce, pathID),
		Signature: sig,
		Timestamp: ts,
		Nonce:     nonce,
		PathID:    pathID,
	})
}

// DownloadStream serves the actual file bytes for a presigned URL.
// GET /api/oss/download (O_OssLocalController.java:169). It is NOT behind the
// JWT middleware — the query-string signature is its only auth (the device
// fetches this URL opaquely, with no x-access-token). Range requests are
// honored (resumable downloads) via http.ServeContent.
func (h *DownloadHandler) DownloadStream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	encPath := q.Get("path")
	sig := q.Get("signature")
	nonce := q.Get("nonce")

	ts, err := strconv.ParseInt(q.Get("timestamp"), 10, 64)
	if err != nil || !h.Signer.ValidateDownload(sig, ts, nonce, encPath) {
		// Bad/expired/tampered signature — refuse before touching the filesystem.
		downloadError(w, msgSignatureFailed)
		return
	}

	decoded, err := oss.DecryptPath(encPath)
	if err != nil {
		downloadError(w, msgSignatureFailed)
		return
	}
	abs, err := mapping.SafeResolve(h.Root, decoded)
	if err != nil {
		// Validly signed but escapes the root (traversal) — treat as a download
		// failure; never serve bytes outside FileRoot.
		h.log().Warn("oss download refused unsafe path", "path", decoded, "err", err)
		downloadError(w, msgDownloadFailed)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		downloadError(w, msgDownloadFailed)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		downloadError(w, msgDownloadFailed)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fi.Name()))
	// ServeContent handles Range/If-Modified-Since/206 and sets Content-Length.
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

// downloadError writes the SPC FileDownloadException response: HTTP 500 with the
// bare message as a plain-text body (no JSON envelope).
func downloadError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/plain;charset=UTF-8")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(msg))
}

// signedDownloadURL builds a presigned /api/oss/download URL for a path_display.
// pathId carries the file's registry id (not load-bearing for UB's GET handler).
func (h *DownloadHandler) signedDownloadURL(r *http.Request, pathDisplay, pathID string) string {
	encPath := oss.EncryptPath(pathDisplay)
	ts := h.nowMillis()
	nonce := newNonce()
	sig := h.Signer.DownloadSignature(encPath, ts, nonce)
	return h.downloadURL(r, encPath, sig, ts, nonce, pathID)
}

// downloadURL assembles the URL in the §6 template order. The query values need
// no escaping: encPath is base64url (URL-safe, unpadded), sig is hex, nonce is a
// UUID, pathId is numeric — matching real SPC's String.format construction.
func (h *DownloadHandler) downloadURL(r *http.Request, encPath, sig string, ts int64, nonce, pathID string) string {
	return fmt.Sprintf("%s/api/oss/download?path=%s&signature=%s&timestamp=%s&nonce=%s&pathId=%s",
		requestBaseURL(r), encPath, sig, strconv.FormatInt(ts, 10), nonce, pathID)
}

func (h *DownloadHandler) nowMillis() int64 {
	if h.Signer != nil && h.Signer.Now != nil {
		return h.Signer.Now().UnixMilli()
	}
	return time.Now().UnixMilli()
}

// requestBaseURL reconstructs the externally-visible scheme://host UB is reached
// at, from the reverse-proxy headers (NPM sets x-forwarded-proto + Host). Mirrors
// the requestUrl the real controllers build from x-forwarded-proto + host.
func requestBaseURL(r *http.Request) string {
	proto := r.Header.Get("x-forwarded-proto")
	if proto == "" {
		proto = "https"
	}
	return proto + "://" + r.Host
}

// newNonce returns a random UUIDv4 string (lowercase hex with hyphens), matching
// the device-observed nonce shape (UUID.randomUUID().toString(), §6).
func newNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
