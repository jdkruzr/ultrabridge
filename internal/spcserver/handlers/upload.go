package handlers

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
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

// Enqueuer submits a freshly-uploaded file to the OCR/index pipeline. It is the
// minimal slice of processor.Store the upload path needs; main.go adapts the
// real processor to it. Optional on UploadHandler (nil = no OCR kick) so tests
// and FileRoot-only deployments stay light. The file flows through the
// unmodified pipeline (catalog write-through included) — no worker changes.
type Enqueuer interface {
	Enqueue(ctx context.Context, path string) error
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
	// Enqueuer (optional) kicks the OCR pipeline for uploaded .note/.pdf files;
	// OCRWatchDir, when set, restricts the kick to uploads under that directory
	// (the Supernote source's NotesPath) so files outside the pipeline's reach
	// aren't enqueued. Empty OCRWatchDir enqueues any .note/.pdf.
	Enqueuer    Enqueuer
	OCRWatchDir string
	Logger      *slog.Logger
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

	urls := h.signedUploadURLs(r, innerName)
	envelope.WriteJSON(w, dto.FileUploadApplyLocalVO{
		BaseVO:        envelope.OK(),
		EquipmentNo:   req.EquipmentNo,
		BucketName:    req.FileName,
		InnerName:     innerName,
		XAmzDate:      strconv.FormatInt(urls.timestamp, 10),
		Authorization: urls.signature,
		FullUploadUrl: urls.full,
		PartUploadUrl: urls.part,
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

	if err := r.ParseMultipartForm(uploadMaxMemory); err != nil {
		uploadError(w, msgUploadFailed)
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

// UploadPart accepts one chunk from the device. The final arriving chunk
// assembles all numbered parts into the same staging file used by Finish.
func (h *UploadHandler) UploadPart(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ts, err := strconv.ParseInt(q.Get("timestamp"), 10, 64)
	if err != nil || !h.Signer.ValidateUpload(q.Get("signature"), ts, q.Get("nonce"), q.Get("path"), 0) {
		uploadError(w, msgUploadSignatureFailed)
		return
	}
	innerName, err := oss.DecryptPath(q.Get("path"))
	if err != nil {
		uploadError(w, msgUploadSignatureFailed)
		return
	}
	partNumber, partErr := strconv.Atoi(q.Get("partNumber"))
	totalChunks, totalErr := strconv.Atoi(q.Get("totalChunks"))
	uploadID := q.Get("uploadId")
	if partErr != nil || totalErr != nil || uploadID == "" {
		uploadError(w, msgUploadFailed)
		return
	}

	if err := r.ParseMultipartForm(uploadMaxMemory); err != nil {
		uploadError(w, msgUploadFailed)
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
	chunkHash := md5.New()
	if _, _, err := h.Staging.StagePart(innerName, uploadID, partNumber, totalChunks, io.TeeReader(file, chunkHash)); err != nil {
		h.log().Error("oss/upload/part StagePart", "innerName", innerName, "uploadId", uploadID, "partNumber", partNumber, "totalChunks", totalChunks, "err", err)
		uploadError(w, msgUploadFailed)
		return
	}
	// StagePart consumes the body for a new/retried part. A retry received after
	// assembly is already complete returns early, so drain the remaining body to
	// produce the same checksum response in both cases.
	if _, err := io.Copy(chunkHash, file); err != nil {
		uploadError(w, msgUploadFailed)
		return
	}
	envelope.WriteJSON(w, dto.FileChunkVO{
		BaseVO:     envelope.OK(),
		UploadID:   uploadID,
		PartNumber: partNumber,
		TotalParts: totalChunks,
		ChunkMD5:   hex.EncodeToString(chunkHash.Sum(nil)),
		Status:     "SUCCESS",
	})
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
	// The device's finish sends `path` as the PARENT directory plus a separate
	// `fileName`; join them for the promotion target (apply, by contrast, sends
	// the full path — do not use the apply-recorded path here). See §8 wire note.
	target := path.Join(req.Path, req.FileName)
	abs, err := h.Staging.Finalize(r.Context(), req.InnerName, req.ContentHash, size, target)
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

	// Kick the OCR pipeline for the uploaded notebook (best-effort; additive).
	h.maybeEnqueueOCR(r.Context(), abs)

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
type signedUploadURLs struct {
	full      string
	part      string
	signature string
	timestamp int64
}

func (h *UploadHandler) signedUploadURLs(r *http.Request, innerName string) signedUploadURLs {
	encPath := oss.EncryptPath(innerName)
	ts := h.nowMillis()
	nonce := newNonce()
	sig := h.Signer.UploadSignature(encPath, ts, nonce, 0)
	query := "?signature=" + sig + "&timestamp=" + strconv.FormatInt(ts, 10) + "&nonce=" + nonce + "&path=" + encPath
	base := requestBaseURL(r) + "/api/oss/upload"
	return signedUploadURLs{
		full:      base + query,
		part:      base + "/part" + query,
		signature: sig,
		timestamp: ts,
	}
}

func (h *UploadHandler) nowMillis() int64 {
	if h.Signer != nil && h.Signer.Now != nil {
		return h.Signer.Now().UnixMilli()
	}
	return time.Now().UnixMilli()
}

// maybeEnqueueOCR best-effort submits an uploaded .note/.pdf to the OCR pipeline.
// A non-.note/.pdf, a path outside OCRWatchDir (when set), a nil Enqueuer, or an
// enqueue error are all silently skipped — OCR is additive and never fails the
// upload (the device round-trip already succeeded).
func (h *UploadHandler) maybeEnqueueOCR(ctx context.Context, abs string) {
	if h.Enqueuer == nil {
		return
	}
	switch strings.ToLower(filepath.Ext(abs)) {
	case ".note", ".pdf":
	default:
		return
	}
	if h.OCRWatchDir != "" && !underDir(h.OCRWatchDir, abs) {
		return
	}
	if err := h.Enqueuer.Enqueue(ctx, abs); err != nil {
		h.log().Warn("upload OCR enqueue failed", "path", abs, "err", err)
	}
}

// underDir reports whether abs lies within dir (after cleaning), guarding against
// a "../" relative escape.
func underDir(dir, abs string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(abs))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// uploadError writes the SPC FileUploadException response: HTTP 500 with the bare
// message as a plain-text body (no JSON envelope), mirroring downloadError.
func uploadError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/plain;charset=UTF-8")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(msg))
}
