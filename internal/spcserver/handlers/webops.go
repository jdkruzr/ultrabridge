package handlers

import (
	"encoding/json"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sysop/ultrabridge/internal/spcserver/dto"
	"github.com/sysop/ultrabridge/internal/spcserver/envelope"
	"github.com/sysop/ultrabridge/internal/spcserver/mapping"
)

type webUploadApplyDTO struct {
	DirectoryID string `json:"directoryId"`
	FileName    string `json:"fileName"`
	Size        any    `json:"size"`
	FileSize    any    `json:"fileSize"`
	MD5         string `json:"md5"`
	EquipmentNo string `json:"equipmentNo"`
}

type webUploadFinishDTO struct {
	DirectoryID string `json:"directoryId"`
	FileName    string `json:"fileName"`
	FileSize    any    `json:"fileSize"`
	Size        any    `json:"size"`
	InnerName   string `json:"innerName"`
	MD5         string `json:"md5"`
	ContentHash string `json:"content_hash"`
	EquipmentNo string `json:"equipmentNo"`
}

// WebApply handles POST /api/file/upload/apply. It adapts the web/Partner
// directoryId-shaped request into UB's existing SPC staged upload model.
func (h *UploadHandler) WebApply(w http.ResponseWriter, r *http.Request) {
	var req webUploadApplyDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	parent := h.displayForID(r, req.DirectoryID)
	fileName := safeBaseName(req.FileName)
	if fileName == "" {
		envelope.WriteJSON(w, envelope.BaseVO{Success: false, ErrorCode: errUploadFailedCode, ErrorMsg: errUploadFailedMsg})
		return
	}
	innerName := newNonce()
	size := int64FromAny(firstAny(req.Size, req.FileSize))
	if h.Staging != nil {
		if err := h.Staging.Record(r.Context(), innerName, parent, fileName, size, uploadApplyTTL); err != nil {
			h.log().Error("web upload/apply Record", "innerName", innerName, "err", err)
		}
	}
	envelope.WriteJSON(w, struct {
		envelope.BaseVO
		InnerName     string `json:"innerName"`
		FullUploadUrl string `json:"fullUploadUrl"`
		PartUploadUrl string `json:"partUploadUrl"`
	}{BaseVO: envelope.OK(), InnerName: innerName, FullUploadUrl: h.signedUploadURL(r, innerName)})
}

// WebFinish handles POST /api/file/upload/finish.
func (h *UploadHandler) WebFinish(w http.ResponseWriter, r *http.Request) {
	var req webUploadFinishDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	parent := h.displayForID(r, req.DirectoryID)
	fileName := safeBaseName(req.FileName)
	if fileName == "" {
		envelope.WriteJSON(w, envelope.BaseVO{Success: false, ErrorCode: errUploadFailedCode, ErrorMsg: errUploadFailedMsg})
		return
	}
	hash := firstNonEmpty(req.ContentHash, req.MD5)
	size := int64FromAny(firstAny(req.FileSize, req.Size))
	h.finishTarget(w, r, req.EquipmentNo, req.InnerName, hash, size, path.Join(parent, fileName), false)
}

// FinishConfirm handles POST /api/file/3/files/upload/confirm, the v3 name for
// the same finalization shape UB already supports.
func (h *UploadHandler) FinishConfirm(w http.ResponseWriter, r *http.Request) {
	h.Finish(w, r)
}

func (h *UploadHandler) finishTarget(w http.ResponseWriter, r *http.Request, equipmentNo, innerName, contentHash string, size int64, target string, includeEntry bool) {
	fail := func() {
		envelope.WriteJSON(w, dto.FileUploadFinishLocalVO{
			BaseVO:      envelope.BaseVO{Success: false, ErrorCode: errUploadFailedCode, ErrorMsg: errUploadFailedMsg},
			EquipmentNo: equipmentNo,
		})
	}
	if h.Staging == nil {
		fail()
		return
	}
	abs, err := h.Staging.Finalize(r.Context(), innerName, contentHash, size, target)
	if err != nil {
		h.log().Warn("web upload/finish failed", "innerName", innerName, "err", err)
		fail()
		return
	}
	entry, err := mapping.EntryFor(r.Context(), h.Root, abs, h.Reg)
	if err != nil {
		fail()
		return
	}
	h.maybeEnqueueOCR(r.Context(), abs)
	if h.Notifier != nil {
		_ = h.Notifier.NotifyFile(r.Context())
	}
	if includeEntry {
		envelope.WriteJSON(w, dto.FileUploadFinishLocalVO{
			BaseVO:      envelope.OK(),
			EquipmentNo: equipmentNo,
			PathDisplay: entry.PathDisplay,
			ID:          entry.ID,
			Size:        entry.Size,
			Name:        entry.Name,
			ContentHash: entry.ContentHash,
		})
		return
	}
	envelope.WriteJSON(w, envelope.OK())
}

func (h *UploadHandler) displayForID(r *http.Request, rawID string) string {
	if rawID == "" || rawID == "0" || h.Reg == nil {
		return "/"
	}
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		return "/"
	}
	abs, found, err := h.Reg.PathFor(r.Context(), id)
	if err != nil || !found {
		return "/"
	}
	if e, err := mapping.EntryFor(r.Context(), h.Root, abs, h.Reg); err == nil {
		return e.PathDisplay
	}
	return "/"
}

type webDownloadDTO struct {
	ID string `json:"id"`
}

// WebDownloadURL handles POST /api/file/download/url.
func (h *DownloadHandler) WebDownloadURL(w http.ResponseWriter, r *http.Request) {
	var req webDownloadDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	id, err := strconv.ParseInt(req.ID, 10, 64)
	if err != nil || h.Root == "" {
		envelope.WriteJSON(w, envelope.BaseVO{Success: false, ErrorCode: errFileNotExistCode, ErrorMsg: errFileNotExistMsg})
		return
	}
	abs, found, err := h.Reg.PathFor(r.Context(), id)
	if err != nil || !found {
		envelope.WriteJSON(w, envelope.BaseVO{Success: false, ErrorCode: errFileNotExistCode, ErrorMsg: errFileNotExistMsg})
		return
	}
	entry, err := mapping.EntryFor(r.Context(), h.Root, abs, h.Reg)
	if err != nil {
		envelope.WriteJSON(w, envelope.BaseVO{Success: false, ErrorCode: errFileNotExistCode, ErrorMsg: errFileNotExistMsg})
		return
	}
	envelope.WriteJSON(w, struct {
		envelope.BaseVO
		URL string `json:"url"`
		MD5 string `json:"md5"`
	}{BaseVO: envelope.OK(), URL: h.signedDownloadURL(r, entry.PathDisplay, entry.ID), MD5: entry.ContentHash})
}

// FolderAdd handles POST /api/file/folder/add.
func (h *WebFileHandler) FolderAdd(w http.ResponseWriter, r *http.Request) {
	var req webFileRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	parent, ok := h.dirFromDirectoryID(r.Context(), req.DirectoryID)
	if !ok {
		envelope.WriteError(w, errSameNameCode, errSameNameMsg)
		return
	}
	fileName := safeBaseName(req.FileName)
	if fileName == "" {
		envelope.WriteError(w, errSameNameCode, errSameNameMsg)
		return
	}
	abs := filepath.Join(parent, fileName)
	if err := os.MkdirAll(abs, 0o755); err != nil {
		envelope.WriteError(w, errSameNameCode, errSameNameMsg)
		return
	}
	entry, err := mapping.EntryFor(r.Context(), h.Root, abs, h.Reg)
	if err != nil {
		envelope.WriteError(w, errSameNameCode, errSameNameMsg)
		return
	}
	envelope.WriteJSON(w, struct {
		envelope.BaseVO
		Metadata *dto.MetadataVO `json:"metadata,omitempty"`
	}{BaseVO: envelope.OK(), Metadata: &dto.MetadataVO{Tag: entry.Tag, ID: entry.ID, Name: entry.Name, PathDisplay: entry.PathDisplay}})
}

// WebMove handles POST /api/file/move.
func (h *MutationHandler) WebMove(w http.ResponseWriter, r *http.Request) {
	h.webMoveCopy(w, r, false)
}

// WebCopy handles POST /api/file/copy.
func (h *MutationHandler) WebCopy(w http.ResponseWriter, r *http.Request) {
	h.webMoveCopy(w, r, true)
}

func (h *MutationHandler) webMoveCopy(w http.ResponseWriter, r *http.Request, copyMode bool) {
	var req webFileRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	destDir, ok := h.dirByID(r, req.GoDirectoryID)
	if !ok {
		envelope.WriteError(w, errMoveMissingCode, errMoveMissingMsg)
		return
	}
	for _, rawID := range req.IDList {
		id, src, ok := h.resolveSource(r, rawID)
		if !ok {
			envelope.WriteError(w, errMoveMissingCode, errMoveMissingMsg)
			return
		}
		dest := filepath.Join(destDir, filepath.Base(src))
		if copyMode {
			dest, _ = h.autorenameAbs(dest)
			if err := copyFile(src, dest); err != nil {
				envelope.WriteError(w, errCopyMissingCode, errCopyMissingMsg)
				return
			}
			h.reindexCopy(r.Context(), src, dest)
		} else {
			dest, _ = h.autorenameAbs(dest)
			if err := os.Rename(src, dest); err != nil {
				envelope.WriteError(w, errMoveMissingCode, errMoveMissingMsg)
				return
			}
			_ = h.Reg.UpdatePath(r.Context(), id, dest)
			h.reindexMove(r.Context(), src, dest)
			h.pruneEmptyParents(filepath.Dir(src))
		}
	}
	if h.Notifier != nil {
		_ = h.Notifier.NotifyFile(r.Context())
	}
	envelope.WriteJSON(w, envelope.OK())
}

// WebRename handles POST /api/file/rename.
func (h *MutationHandler) WebRename(w http.ResponseWriter, r *http.Request) {
	var req webFileRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	id, src, ok := h.resolveSource(r, req.ID)
	if !ok {
		envelope.WriteError(w, errMoveMissingCode, errMoveMissingMsg)
		return
	}
	dest := filepath.Join(filepath.Dir(src), filepath.Base(req.NewName))
	if _, err := os.Lstat(dest); err == nil {
		envelope.WriteError(w, errSameNameCode, errSameNameMsg)
		return
	}
	if err := os.Rename(src, dest); err != nil {
		envelope.WriteError(w, errMoveMissingCode, errMoveMissingMsg)
		return
	}
	_ = h.Reg.UpdatePath(r.Context(), id, dest)
	h.reindexMove(r.Context(), src, dest)
	if h.Notifier != nil {
		_ = h.Notifier.NotifyFile(r.Context())
	}
	envelope.WriteJSON(w, envelope.OK())
}

// WebDelete handles POST /api/file/delete.
func (h *MutationHandler) WebDelete(w http.ResponseWriter, r *http.Request) {
	var req webFileRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	for _, rawID := range req.IDList {
		_, src, ok := h.resolveSource(r, rawID)
		if !ok {
			continue
		}
		if e, err := mapping.EntryFor(r.Context(), h.Root, src, h.Reg); err == nil {
			_ = h.recycle(e.PathDisplay, src)
			h.deindex(r.Context(), src)
			h.pruneEmptyParents(filepath.Dir(src))
		}
	}
	if h.Notifier != nil {
		_ = h.Notifier.NotifyFile(r.Context())
	}
	envelope.WriteJSON(w, envelope.OK())
}

// RecycleList handles POST /api/file/recycle/list/query.
func (h *MutationHandler) RecycleList(w http.ResponseWriter, r *http.Request) {
	base := filepath.Join(h.Root, recycleDir)
	var entries []dto.EntriesVO
	_ = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == base || d.IsDir() {
			return nil
		}
		if e, err := mapping.EntryFor(r.Context(), h.Root, p, h.Reg); err == nil {
			entries = append(entries, e)
		}
		return nil
	})
	sortEntries(entries)
	writeWebList(w, entries, webFileRequest{})
}

// RecycleRevert handles POST /api/file/recycle/revert.
func (h *MutationHandler) RecycleRevert(w http.ResponseWriter, r *http.Request) {
	var req webFileRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	for _, rawID := range idsFromRequest(req) {
		src, ok := h.recyclePathByID(r, rawID)
		if !ok {
			continue
		}
		dest := h.restoreDest(src)
		dest, _ = h.autorenameAbs(dest)
		_ = os.MkdirAll(filepath.Dir(dest), 0o755)
		if err := os.Rename(src, dest); err == nil {
			if id, err := strconv.ParseInt(rawID, 10, 64); err == nil {
				_ = h.Reg.UpdatePath(r.Context(), id, dest)
			}
			h.pruneEmptyParents(filepath.Dir(src))
		}
	}
	if h.Notifier != nil {
		_ = h.Notifier.NotifyFile(r.Context())
	}
	envelope.WriteJSON(w, envelope.OK())
}

// RecycleDelete handles POST /api/file/recycle/delete.
func (h *MutationHandler) RecycleDelete(w http.ResponseWriter, r *http.Request) {
	var req webFileRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	for _, rawID := range idsFromRequest(req) {
		if src, ok := h.recyclePathByID(r, rawID); ok {
			_ = os.RemoveAll(src)
			h.pruneEmptyParents(filepath.Dir(src))
		}
	}
	envelope.WriteJSON(w, envelope.OK())
}

// RecycleClear handles POST /api/file/recycle/clear.
func (h *MutationHandler) RecycleClear(w http.ResponseWriter, r *http.Request) {
	_ = os.RemoveAll(filepath.Join(h.Root, recycleDir))
	envelope.WriteJSON(w, envelope.OK())
}

func (h *MutationHandler) dirByID(r *http.Request, rawID string) (string, bool) {
	if rawID == "" || rawID == "0" {
		return h.Root, true
	}
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		return "", false
	}
	abs, found, err := h.Reg.PathFor(r.Context(), id)
	if err != nil || !found {
		return "", false
	}
	fi, err := os.Lstat(abs)
	return abs, err == nil && fi.IsDir()
}

func (h *MutationHandler) autorenameAbs(dest string) (string, string) {
	if _, err := os.Lstat(dest); os.IsNotExist(err) {
		return dest, ""
	}
	ext := filepath.Ext(dest)
	base := strings.TrimSuffix(dest, ext)
	for n := 1; n < 1000; n++ {
		cand := base + " (" + strconv.Itoa(n) + ")" + ext
		if _, err := os.Lstat(cand); os.IsNotExist(err) {
			return cand, ""
		}
	}
	return "", errSameNameCode
}

func (h *MutationHandler) recyclePathByID(r *http.Request, rawID string) (string, bool) {
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		return "", false
	}
	abs, found, err := h.Reg.PathFor(r.Context(), id)
	if err != nil || !found {
		return "", false
	}
	rel, err := filepath.Rel(filepath.Join(h.Root, recycleDir), abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	if _, err := os.Lstat(abs); err != nil {
		return "", false
	}
	return abs, true
}

func (h *MutationHandler) restoreDest(src string) string {
	rel, err := filepath.Rel(filepath.Join(h.Root, recycleDir), src)
	if err != nil {
		return filepath.Join(h.Root, filepath.Base(src))
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) > 1 {
		parts = parts[1:]
	}
	return filepath.Join(h.Root, filepath.FromSlash(strings.Join(parts, "/")))
}

func idsFromRequest(req webFileRequest) []string {
	if len(req.IDList) > 0 {
		return req.IDList
	}
	if req.ID != "" {
		return []string{req.ID}
	}
	return nil
}

func firstAny(vals ...any) any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

func int64FromAny(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case json.Number:
		n, _ := x.Int64()
		return n
	default:
		return 0
	}
}

func safeBaseName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return ""
	}
	base := filepath.Base(name)
	if base == "." || base == ".." {
		return ""
	}
	return base
}
