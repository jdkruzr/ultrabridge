package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"

	"github.com/sysop/ultrabridge/internal/spcserver/capacity"
	"github.com/sysop/ultrabridge/internal/spcserver/dto"
	"github.com/sysop/ultrabridge/internal/spcserver/envelope"
	"github.com/sysop/ultrabridge/internal/spcserver/fileids"
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
