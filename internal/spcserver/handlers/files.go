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
	"strings"

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
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if p == dir {
				return nil
			}
			// Hide UB's own dot-dirs (.staging, .recycle) and any dotfile — they
			// are never part of the device's native tree. Skipping a dot *dir*
			// prunes its whole subtree.
			if isHidden(d.Name()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			paths = append(paths, p)
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
		if isHidden(e.Name()) {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	return paths, nil
}

// isHidden reports whether a directory entry is dot-prefixed. UB stages uploads
// under .staging and soft-deletes into .recycle; both live under FILE_ROOT but
// must stay invisible to the device's file listing.
func isHidden(name string) bool {
	return strings.HasPrefix(name, ".")
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
		if abs, ok := h.resolveQueryPath(req.Path); ok {
			if e, err := mapping.EntryFor(r.Context(), h.Root, abs, h.Reg); err == nil {
				vo.EntriesVO = &e
			}
		}
	}
	envelope.WriteJSON(w, vo)
}

func (h *FileHandler) resolveQueryPath(reqPath string) (string, bool) {
	for _, p := range queryPathCandidates(reqPath) {
		abs, err := mapping.SafeResolve(h.Reg.Root(), p)
		if err != nil {
			continue
		}
		if _, statErr := os.Lstat(abs); statErr == nil {
			return abs, true
		}
	}
	return "", false
}

func queryPathCandidates(reqPath string) []string {
	out := []string{reqPath}
	rel := strings.TrimPrefix(reqPath, "/")
	if rest, ok := strings.CutPrefix(rel, "Note/"); ok {
		out = append(out, "/NOTE/Note/"+rest)
	}
	if rest, ok := strings.CutPrefix(rel, "Document/"); ok {
		out = append(out, "/DOCUMENT/Document/"+rest)
	}
	return out
}

// CapacityQuery serves /api/file/capacity/query (the variant the device hits):
// used = du-sum under the root, total = the configured quota.
func (h *FileHandler) CapacityQuery(w http.ResponseWriter, r *http.Request) {
	envelope.WriteJSON(w, dto.CapacityVO{
		BaseVO:        envelope.OK(),
		UsedCapacity:  h.Meter.Usage(),
		TotalCapacity: h.Meter.Quota(),
	})
}

// GetSpaceUsage serves /api/file/2/users/get_space_usage. allocationVO.tag is
// "individual" per FileLocalServiceImpl.queryCapacity.
func (h *FileHandler) GetSpaceUsage(w http.ResponseWriter, r *http.Request) {
	var req dto.CapacityLocalDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	envelope.WriteJSON(w, dto.CapacityLocalVO{
		BaseVO:       envelope.OK(),
		Used:         h.Meter.Usage(),
		AllocationVO: dto.AllocationVO{Tag: "individual", Allocated: h.Meter.Quota()},
		EquipmentNo:  req.EquipmentNo,
	})
}

// CreateFolderV2 creates the requested folder under FileRoot and returns its
// metadata (tag/id/name/path_display). The server-assigned id is load-bearing:
// the device records it and uses it to upload notes into the new folder — when
// UB returned an empty metadata stub, the device aborted the sync right after
// create_folder_v2 (wire-confirmed 2026-05-26: query/by/path → create_folder_v2
// → synchronous/end, no upload). The id comes from the same fileids registry
// list_folder uses, so a later listing reports the identical id.
//
// Collision (the path already exists): with autorename=false the real server
// returns E0322; UB mirrors that. With autorename=true UB is idempotent and
// returns the existing folder's metadata (the device queries by path first and
// only sends autorename=false, so the real server's rename-with-suffix scheme
// is not yet replicated — left for a capture that shows the device needs it).
func (h *FileHandler) CreateFolderV2(w http.ResponseWriter, r *http.Request) {
	var req dto.CreateFolderLocalDTO
	_ = json.NewDecoder(r.Body).Decode(&req)

	abs, err := mapping.SafeResolve(h.Root, req.Path)
	if err != nil {
		h.log().Warn("create_folder_v2 bad path", "path", req.Path, "err", err)
		envelope.WriteError(w, "E0322", "invalid path")
		return
	}

	if fi, statErr := os.Stat(abs); statErr == nil {
		// Path exists. A file (not a dir) collision, or a dir collision without
		// autorename, is the real server's E0322. With autorename we fall through
		// to the idempotent MkdirAll (no-op) and return the existing folder.
		if !fi.IsDir() || !req.Autorename {
			envelope.WriteError(w, "E0322", "folder already exists")
			return
		}
	}

	if err := os.MkdirAll(abs, 0o755); err != nil {
		h.log().Error("create_folder_v2 mkdir", "path", abs, "err", err)
		envelope.WriteError(w, "E0322", "create failed")
		return
	}

	// Build metadata via the same machinery list_folder uses, so the id and
	// path_display are identical to what a subsequent listing reports.
	entry, err := mapping.EntryFor(r.Context(), h.Root, abs, h.Reg)
	if err != nil {
		h.log().Error("create_folder_v2 EntryFor", "path", abs, "err", err)
		envelope.WriteError(w, "E0322", "create failed")
		return
	}
	envelope.WriteJSON(w, dto.CreateFolderLocalVO{
		BaseVO:      envelope.OK(),
		EquipmentNo: req.EquipmentNo,
		Metadata: &dto.MetadataVO{
			Tag:         entry.Tag,
			ID:          entry.ID,
			Name:        entry.Name,
			PathDisplay: entry.PathDisplay,
		},
	})
}

// QueryByIDDeleteAPI is a canned-success stub for query/deleteApi (not observed
// in 0b; becomes real in Phase 4/5). Returns success with a null entry.
func (h *FileHandler) QueryByIDDeleteAPI(w http.ResponseWriter, r *http.Request) {
	var req dto.FileQueryV2DTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	envelope.WriteJSON(w, dto.FileQueryV2VO{
		BaseVO:      envelope.OK(),
		EquipmentNo: req.EquipmentNo,
	})
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
