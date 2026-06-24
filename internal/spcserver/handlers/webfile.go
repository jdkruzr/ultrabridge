package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sysop/ultrabridge/internal/spcserver/dto"
	"github.com/sysop/ultrabridge/internal/spcserver/envelope"
	"github.com/sysop/ultrabridge/internal/spcserver/fileids"
	"github.com/sysop/ultrabridge/internal/spcserver/mapping"
)

// WebFileHandler implements the SPC web/Partner file controller surface. It is
// intentionally rooted only in SPC FileRoot; it never consults UB's cross-source
// stores, unified search, Boox, ForestNote, or reMarkable adapters.
type WebFileHandler struct {
	Root   string
	Reg    *fileids.Registry
	Logger *slog.Logger
}

func (h *WebFileHandler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

type webFileRequest struct {
	ID            string   `json:"id"`
	DirectoryID   string   `json:"directoryId"`
	GoDirectoryID string   `json:"goDirectoryId"`
	FileName      string   `json:"fileName"`
	NewName       string   `json:"newName"`
	Path          string   `json:"path"`
	Recursive     bool     `json:"recursive"`
	IDList        []string `json:"idList"`
	Page          int      `json:"page"`
	CurrentPage   int      `json:"currentPage"`
	PageSize      int      `json:"pageSize"`
	Limit         int      `json:"limit"`
}

type webSearchRequest struct {
	webFileRequest
	Keyword  string `json:"keyword"`
	FileName string `json:"fileName"`
	Name     string `json:"name"`
}

type webFileListVO struct {
	envelope.BaseVO
	Entries     []dto.EntriesVO `json:"entries"`
	FileList    []dto.EntriesVO `json:"fileList"`
	Records     []dto.EntriesVO `json:"records"`
	Total       int             `json:"total"`
	CurrentPage int             `json:"currentPage"`
	PageSize    int             `json:"pageSize"`
}

// ListByPath is POST /api/file/3/files/list, the recursive path-based listing
// used by the web/Partner controller family.
func (h *WebFileHandler) ListByPath(w http.ResponseWriter, r *http.Request) {
	var req webFileRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	dir, ok := h.dirFromPath(req.Path)
	if !ok {
		writeWebList(w, nil, req)
		return
	}
	entries := h.entriesUnder(r.Context(), dir, req.Recursive)
	writeWebList(w, entries, req)
}

// ListQuery handles POST /api/file/list/query: paginated browsing by directory id.
func (h *WebFileHandler) ListQuery(w http.ResponseWriter, r *http.Request) {
	var req webFileRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	dir, ok := h.dirFromDirectoryID(r.Context(), req.DirectoryID)
	if !ok {
		writeWebList(w, nil, req)
		return
	}
	writeWebList(w, h.entriesUnder(r.Context(), dir, false), req)
}

// PathQuery resolves a file/folder id to its SPC path and breadcrumb.
func (h *WebFileHandler) PathQuery(w http.ResponseWriter, r *http.Request) {
	var req webFileRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	id := firstNonEmpty(req.ID, req.DirectoryID)
	abs, ok := h.absFromID(r.Context(), id)
	if !ok {
		envelope.WriteJSON(w, struct {
			envelope.BaseVO
			Path string `json:"path"`
		}{BaseVO: envelope.OK()})
		return
	}
	entry, err := mapping.EntryFor(r.Context(), h.Root, abs, h.Reg)
	if err != nil {
		envelope.WriteJSON(w, struct {
			envelope.BaseVO
			Path string `json:"path"`
		}{BaseVO: envelope.OK()})
		return
	}
	envelope.WriteJSON(w, struct {
		envelope.BaseVO
		Path        string          `json:"path"`
		PathDisplay string          `json:"path_display"`
		Crumbs      []dto.EntriesVO `json:"crumbs"`
	}{BaseVO: envelope.OK(), Path: entry.PathDisplay, PathDisplay: entry.PathDisplay, Crumbs: h.crumbs(r.Context(), abs)})
}

// FolderListQuery returns folders only, for move/copy destination pickers.
func (h *WebFileHandler) FolderListQuery(w http.ResponseWriter, r *http.Request) {
	var req webFileRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	dir, ok := h.dirFromDirectoryID(r.Context(), req.DirectoryID)
	if !ok {
		writeWebList(w, nil, req)
		return
	}
	var folders []dto.EntriesVO
	for _, e := range h.entriesUnder(r.Context(), dir, true) {
		if e.Tag == "folder" {
			folders = append(folders, e)
		}
	}
	writeWebList(w, folders, req)
}

// Search handles both /api/file/list/search and /api/file/label/list/search.
// SPC's "label" search is filename substring search, scoped to a directory.
func (h *WebFileHandler) Search(w http.ResponseWriter, r *http.Request) {
	var req webSearchRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	needle := strings.ToLower(firstNonEmpty(req.FileName, req.Keyword, req.Name))
	dir, ok := h.dirFromDirectoryID(r.Context(), req.DirectoryID)
	if !ok {
		writeWebList(w, nil, req.webFileRequest)
		return
	}
	var matches []dto.EntriesVO
	for _, e := range h.entriesUnder(r.Context(), dir, true) {
		if needle == "" || strings.Contains(strings.ToLower(e.Name), needle) {
			matches = append(matches, e)
		}
	}
	writeWebList(w, matches, req.webFileRequest)
}

func (h *WebFileHandler) dirFromPath(reqPath string) (string, bool) {
	if h.Root == "" {
		return "", false
	}
	if reqPath == "" {
		reqPath = "/"
	}
	abs, err := mapping.SafeResolve(h.Root, reqPath)
	if err != nil {
		return "", false
	}
	fi, err := os.Lstat(abs)
	return abs, err == nil && fi.IsDir()
}

func (h *WebFileHandler) dirFromDirectoryID(ctx context.Context, directoryID string) (string, bool) {
	if h.Root == "" {
		return "", false
	}
	if directoryID == "" || directoryID == "0" {
		return h.Root, true
	}
	abs, ok := h.absFromID(ctx, directoryID)
	if !ok {
		return "", false
	}
	fi, err := os.Lstat(abs)
	return abs, err == nil && fi.IsDir()
}

func (h *WebFileHandler) absFromID(ctx context.Context, rawID string) (string, bool) {
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || h.Reg == nil {
		return "", false
	}
	abs, found, err := h.Reg.PathFor(ctx, id)
	if err != nil || !found {
		return "", false
	}
	if _, err := os.Lstat(abs); err != nil {
		return "", false
	}
	return abs, true
}

func (h *WebFileHandler) entriesUnder(ctx context.Context, dir string, recursive bool) []dto.EntriesVO {
	var paths []string
	if recursive {
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || p == dir {
				return nil
			}
			if isHidden(d.Name()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			paths = append(paths, p)
			return nil
		})
	} else {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		for _, ent := range ents {
			if !isHidden(ent.Name()) {
				paths = append(paths, filepath.Join(dir, ent.Name()))
			}
		}
	}
	entries := make([]dto.EntriesVO, 0, len(paths))
	for _, p := range paths {
		e, err := mapping.EntryFor(ctx, h.Root, p, h.Reg)
		if err != nil {
			h.log().Warn("web file entry skipped", "path", p, "err", err)
			continue
		}
		entries = append(entries, e)
	}
	sortEntries(entries)
	return entries
}

func (h *WebFileHandler) crumbs(ctx context.Context, abs string) []dto.EntriesVO {
	if h.Root == "" {
		return nil
	}
	var out []dto.EntriesVO
	root := filepath.Clean(h.Root)
	cur := filepath.Clean(abs)
	for strings.HasPrefix(cur, root) {
		if e, err := mapping.EntryFor(ctx, h.Root, cur, h.Reg); err == nil {
			out = append(out, e)
		}
		if cur == root {
			break
		}
		cur = filepath.Dir(cur)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func writeWebList(w http.ResponseWriter, entries []dto.EntriesVO, req webFileRequest) {
	if entries == nil {
		entries = []dto.EntriesVO{}
	}
	page, size := pageParams(req)
	total := len(entries)
	start := (page - 1) * size
	if start > total {
		start = total
	}
	end := start + size
	if end > total {
		end = total
	}
	pageEntries := entries[start:end]
	envelope.WriteJSON(w, webFileListVO{
		BaseVO:      envelope.OK(),
		Entries:     pageEntries,
		FileList:    pageEntries,
		Records:     pageEntries,
		Total:       total,
		CurrentPage: page,
		PageSize:    size,
	})
}

func pageParams(req webFileRequest) (int, int) {
	page := req.CurrentPage
	if page == 0 {
		page = req.Page
	}
	if page < 1 {
		page = 1
	}
	size := req.PageSize
	if size == 0 {
		size = req.Limit
	}
	if size < 1 {
		size = 500
	}
	return page, size
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
