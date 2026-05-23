package handlers

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/sysop/ultrabridge/internal/spcserver/capacity"
	"github.com/sysop/ultrabridge/internal/spcserver/dto"
	"github.com/sysop/ultrabridge/internal/spcserver/envelope"
	"github.com/sysop/ultrabridge/internal/spcserver/fileids"
	"github.com/sysop/ultrabridge/internal/spcserver/mapping"
)

// FileHandler serves the SPC file read-path endpoints (Phase 2): sync-session
// brackets, list_folder, query by id/path, capacity, and the create_folder/
// deleteApi stubs. It reads the filesystem under Root directly (not notestore —
// see docs/implementation-plans/spc-phase-2.md) and never mutates it this phase.
// An empty Root means file listing is disabled: handlers return empty-but-valid
// responses so default (client-mode) config stays inert.
type FileHandler struct {
	Root   string
	Reg    *fileids.Registry
	Meter  *capacity.Meter
	Logger *slog.Logger
}

func (h *FileHandler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// SynchronousStart opens a sync session. Mirrors
// FileLocalServiceImpl.synchronousStart: synType is true when the cloud already
// has top-level folders, false when empty (a first-sync signal). UB skips the
// real SPC's Redis cloud-lock and the cross-device sync broadcast — by design we
// serve a single device with no Redis (see the no-analogue audit).
func (h *FileHandler) SynchronousStart(w http.ResponseWriter, r *http.Request) {
	var req dto.SynchronousStartLocalDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	envelope.WriteJSON(w, dto.SynchronousStartLocalVO{
		BaseVO:      envelope.OK(),
		EquipmentNo: req.EquipmentNo,
		SynType:     h.hasTopLevelFolders(),
	})
}

// SynchronousEnd closes a sync session (echo + success; UB has no lock to release).
func (h *FileHandler) SynchronousEnd(w http.ResponseWriter, r *http.Request) {
	var req dto.SynchronousEndLocalDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	envelope.WriteJSON(w, dto.SynchronousEndLocalVO{
		BaseVO:      envelope.OK(),
		EquipmentNo: req.EquipmentNo,
	})
}

// ListFolder lists the children of a folder. A null/absent/0 id lists the file
// root; otherwise the id is resolved via the registry. An unknown id returns an
// empty (but successful) listing — the device must not error on a stale id.
// Folders are sorted before files, then by name. recursive flattens the whole
// subtree.
func (h *FileHandler) ListFolder(w http.ResponseWriter, r *http.Request) {
	var req dto.ListFolderLocalDTO
	_ = json.NewDecoder(r.Body).Decode(&req)

	vo := dto.ListFolderLocalVO{BaseVO: envelope.OK(), EquipmentNo: req.EquipmentNo, Entries: []dto.EntriesVO{}}

	parent, ok := h.resolveDir(r.Context(), req.ID)
	if !ok {
		envelope.WriteJSON(w, vo) // unset root or unknown id → empty listing
		return
	}

	paths, err := h.childPaths(parent, req.Recursive)
	if err != nil {
		h.log().Warn("list_folder read failed", "path", parent, "error", err)
		envelope.WriteJSON(w, vo)
		return
	}
	for _, p := range paths {
		e, err := mapping.EntryFor(r.Context(), h.Root, p, h.Reg)
		if err != nil {
			h.log().Warn("list_folder entry skipped", "path", p, "error", err)
			continue
		}
		vo.Entries = append(vo.Entries, e)
	}
	sortEntries(vo.Entries)
	envelope.WriteJSON(w, vo)
}

// ListFolderV3 is the /3/files/list_folder_v3 alias — identical logic and DTO.
func (h *FileHandler) ListFolderV3(w http.ResponseWriter, r *http.Request) {
	h.ListFolder(w, r)
}

// resolveDir maps a list_folder id to an absolute directory under the root. A
// nil/0 id is the root. ok is false when the root is unset or the id is unknown.
func (h *FileHandler) resolveDir(ctx context.Context, id *int64) (string, bool) {
	if h.Root == "" {
		return "", false
	}
	if id == nil || *id == 0 {
		return h.Reg.Root(), true
	}
	p, found, err := h.Reg.PathFor(ctx, *id)
	if err != nil || !found {
		return "", false
	}
	return p, true
}

// childPaths returns the absolute paths to list under dir: its direct children,
// or (when recursive) every descendant.
func (h *FileHandler) childPaths(dir string, recursive bool) ([]string, error) {
	if recursive {
		var paths []string
		err := filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if p != dir {
				paths = append(paths, p)
			}
			return nil
		})
		return paths, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(ents))
	for _, e := range ents {
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	return paths, nil
}

// sortEntries orders folders before files, then by path_display.
func sortEntries(es []dto.EntriesVO) {
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].Tag != es[j].Tag {
			return es[i].Tag == "folder" // folders first
		}
		return es[i].PathDisplay < es[j].PathDisplay
	})
}

// QueryByID serves query_v3: resolve an id to a single entry. A missing or
// unparseable id returns success with a null entriesVO — the device probes
// existence this way and must not get an error.
func (h *FileHandler) QueryByID(w http.ResponseWriter, r *http.Request) {
	var req dto.FileQueryLocalDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	vo := dto.FileQueryLocalVO{BaseVO: envelope.OK(), EquipmentNo: req.EquipmentNo}

	id, err := strconv.ParseInt(req.ID, 10, 64)
	if err != nil {
		envelope.WriteJSON(w, vo) // unparseable id → null entry
		return
	}
	if h.Root != "" {
		if p, found, err := h.Reg.PathFor(r.Context(), id); err == nil && found {
			if e, err := mapping.EntryFor(r.Context(), h.Root, p, h.Reg); err == nil {
				vo.EntriesVO = &e
			}
		}
	}
	envelope.WriteJSON(w, vo)
}

// QueryByPath serves query/by/path_v3: resolve an SPC path (root-relative,
// possibly double-slashed) to a single entry. A missing path or a traversal
// attempt returns success with a null entriesVO.
func (h *FileHandler) QueryByPath(w http.ResponseWriter, r *http.Request) {
	var req dto.FileQueryByPathLocalDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	vo := dto.FileQueryByPathLocalVO{BaseVO: envelope.OK(), EquipmentNo: req.EquipmentNo}

	if h.Root != "" {
		if abs, err := mapping.SafeResolve(h.Reg.Root(), req.Path); err == nil {
			if _, statErr := os.Lstat(abs); statErr == nil {
				if e, err := mapping.EntryFor(r.Context(), h.Root, abs, h.Reg); err == nil {
					vo.EntriesVO = &e
				}
			}
		}
	}
	envelope.WriteJSON(w, vo)
}

// hasTopLevelFolders reports whether the file root contains at least one
// subdirectory (the synType signal). An unset or unreadable root → false.
func (h *FileHandler) hasTopLevelFolders() bool {
	if h.Root == "" {
		return false
	}
	entries, err := os.ReadDir(h.Root)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			return true
		}
	}
	return false
}
