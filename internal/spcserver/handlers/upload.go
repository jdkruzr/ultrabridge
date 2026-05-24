package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/sysop/ultrabridge/internal/spcserver/dto"
	"github.com/sysop/ultrabridge/internal/spcserver/envelope"
	"github.com/sysop/ultrabridge/internal/spcserver/fileids"
	"github.com/sysop/ultrabridge/internal/spcserver/mapping"
	"github.com/sysop/ultrabridge/internal/spcserver/oss"
	"github.com/sysop/ultrabridge/internal/spcserver/staging"
)

// uploadApplyTTL is how long a minted upload URL / staged slot stays valid. It
// matches real SPC's 30-minute upload window (SignVerifier.java:55); the staging
// Sweep reclaims applies that never finish within it.
const uploadApplyTTL = 30 * time.Minute

// errUploadFailedCode is FileErrorCodeEnum.E0324 ("This file cannot be
// uploaded"), returned by upload/finish when the staged file fails md5/size
// verification or cannot be promoted.
const (
	errUploadFailedCode = "E0324"
	errUploadFailedMsg  = "This file cannot be uploaded"
)

// The /api/oss/upload byte-sink errors mirror the download GET's U1 shape: real
// SPC's GlobalExceptionHandler maps FileUploadException to a plain-text HTTP 500
// body, not a JSON envelope.
const (
	msgUploadSignatureFailed = "Signature verification failed."
	msgUploadFailed          = "File upload failed."
)

// uploadMaxMemory bounds the in-RAM portion of a multipart parse; the rest
// spills to a temp file. The body is streamed to staging regardless.
const uploadMaxMemory = 8 << 20 // 8 MiB

// UploadNotifier nudges the device to re-pull files after a server-side change.
// Optional on UploadHandler (nil = no push); the device's own upload round-trip
// does not depend on it.
type UploadNotifier interface {
	NotifyFile(ctx context.Context) error
}

// UploadHandler serves the SPC upload write-path (Phase 4): upload/apply mints a
// presigned /api/oss/upload URL + an innerName; the device POSTs the bytes to
// UploadStream, which stages them under FILE_ROOT/.staging; upload/finish then
// verifies md5/size and atomically promotes the staged file to its target path.
// It shares the FileHandler's Root, registry, and signer.
type UploadHandler struct {
	Root     string
	Reg      *fileids.Registry
	Signer   *oss.Signer
	Staging  *staging.Store
	Notifier UploadNotifier
	Logger   *slog.Logger
}

func (h *UploadHandler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// Apply mints an upload slot: it records the innerName→target mapping and
// returns a presigned /api/oss/upload URL the device POSTs the file bytes to.
// POST /api/file/3/files/upload/apply (F_FileLocalController.java:130).
func (h *UploadHandler) Apply(w http.ResponseWriter, r *http.Request) {
	var req dto.FileUploadApplyLocalDTO
	_ = json.NewDecoder(r.Body).Decode(&req)

	innerName := newNonce() // server-chosen UUID; the device treats it opaquely
	size, _ := strconv.ParseInt(req.Size, 10, 64)

	if h.Staging != nil {
		if err := h.Staging.Record(r.Context(), innerName, req.Path, req.FileName, size, uploadApplyTTL); err != nil {
			h.log().Error("upload/apply Record", "innerName", innerName, "err", err)
		}
	}

	url := h.signedUploadURL(r, innerName)
	envelope.WriteJSON(w, dto.FileUploadApplyLocalVO{
		BaseVO:        envelope.OK(),
		EquipmentNo:   req.EquipmentNo,
		InnerName:     innerName,
		FullUploadUrl: url,
	})
}

// UploadStream sinks the uploaded bytes into staging. POST /api/oss/upload
// (O_OssLocalController.java:97). It is NOT behind the JWT middleware — the
// query-string signature is its only auth (the device POSTs opaquely, no
// x-access-token). The body is multipart/form-data with a "file" part.
func (h *UploadHandler) UploadStream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sig := q.Get("signature")
	nonce := q.Get("nonce")
	encPath := q.Get("path")

	ts, err := strconv.ParseInt(q.Get("timestamp"), 10, 64)
	// Real SPC validates the upload signature with fileSize 0 (O_OssLocalController:102).
	if err != nil || !h.Signer.ValidateUpload(sig, ts, nonce, encPath, 0) {
		uploadError(w, msgUploadSignatureFailed)
		return
	}
	innerName, err := oss.DecryptPath(encPath)
	if err != nil {
		uploadError(w, msgUploadSignatureFailed)
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		uploadError(w, msgUploadFailed)
		return
	}
	defer file.Close()

	if h.Staging == nil {
		uploadError(w, msgUploadFailed)
		return
	}
	if _, err := h.Staging.Stage(innerName, file); err != nil {
		h.log().Error("oss/upload Stage", "innerName", innerName, "err", err)
		uploadError(w, msgUploadFailed)
		return
	}
	envelope.WriteJSON(w, dto.UploadFileVO{BaseVO: envelope.OK()})
}

// Finish verifies the staged file's md5/size and promotes it to its target path.
// POST /api/file/2/files/upload/finish (F_FileLocalController.java:146).
func (h *UploadHandler) Finish(w http.ResponseWriter, r *http.Request) {
	var req dto.FileUploadFinishLocalDTO
	_ = json.NewDecoder(r.Body).Decode(&req)

	fail := func() {
		envelope.WriteJSON(w, dto.FileUploadFinishLocalVO{
			BaseVO:      envelope.BaseVO{Success: false, ErrorCode: errUploadFailedCode, ErrorMsg: errUploadFailedMsg},
			EquipmentNo: req.EquipmentNo,
		})
	}

	if h.Staging == nil {
		fail()
		return
	}
	size, _ := strconv.ParseInt(req.Size, 10, 64)
	abs, err := h.Staging.Finalize(r.Context(), req.InnerName, req.ContentHash, size)
	if err != nil {
		h.log().Warn("upload/finish verify/promote failed", "innerName", req.InnerName, "err", err)
		fail()
		return
	}

	// Build the response from the promoted file (id minted via the shared
	// registry; path_display/content_hash/size derived from disk).
	entry, err := mapping.EntryFor(r.Context(), h.Root, abs, h.Reg)
	if err != nil {
		h.log().Error("upload/finish EntryFor", "path", abs, "err", err)
		fail()
		return
	}

	// Nudge the device to re-pull files (best-effort; not load-bearing).
	if h.Notifier != nil {
		_ = h.Notifier.NotifyFile(r.Context())
	}

	envelope.WriteJSON(w, dto.FileUploadFinishLocalVO{
		BaseVO:      envelope.OK(),
		EquipmentNo: req.EquipmentNo,
		PathDisplay: entry.PathDisplay,
		ID:          entry.ID,
		Size:        entry.Size,
		Name:        entry.Name,
		ContentHash: entry.ContentHash,
	})
}

// signedUploadURL builds a presigned /api/oss/upload URL whose path encodes the
// server-chosen innerName (not the human target path): bytes always stage under
// a server-controlled name, and the real path only materializes at finish after
// verification. fileSize is signed as 0, matching real SPC (O_OssLocalController:80).
func (h *UploadHandler) signedUploadURL(r *http.Request, innerName string) string {
	encPath := oss.EncryptPath(innerName)
	ts := h.nowMillis()
	nonce := newNonce()
	sig := h.Signer.UploadSignature(encPath, ts, nonce, 0)
	return requestBaseURL(r) + "/api/oss/upload?signature=" + sig +
		"&timestamp=" + strconv.FormatInt(ts, 10) + "&nonce=" + nonce + "&path=" + encPath
}

func (h *UploadHandler) nowMillis() int64 {
	if h.Signer != nil && h.Signer.Now != nil {
		return h.Signer.Now().UnixMilli()
	}
	return time.Now().UnixMilli()
}

// uploadError writes the SPC FileUploadException response: HTTP 500 with the bare
// message as a plain-text body (no JSON envelope), mirroring downloadError.
func uploadError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/plain;charset=UTF-8")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(msg))
}
