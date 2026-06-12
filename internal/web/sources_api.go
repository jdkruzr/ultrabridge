package web

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sysop/ultrabridge/internal/source"
)

// sourceView is the API representation of a source: the persisted row plus its
// derived, view-only sync model. The embedded SourceRow promotes its JSON
// fields to top level, so existing []source.SourceRow decoders keep working —
// the added sync_model field is simply ignored by them.
type sourceView struct {
	source.SourceRow
	SyncModel source.SyncModel `json:"sync_model"`
}

// handleListSources handles GET /api/sources — list all sources.
func (h *Handler) handleListSources(w http.ResponseWriter, r *http.Request) {
	sources, err := h.config.ListSources(r.Context())
	if err != nil {
		h.logger.Error("list sources", "error", err)
		apiError(w, http.StatusInternalServerError, "failed to list sources")
		return
	}

	rows, ok := sources.([]source.SourceRow)
	if !ok {
		h.logger.Error("list sources: unexpected type from service", "type", fmt.Sprintf("%T", sources))
		apiError(w, http.StatusInternalServerError, "failed to list sources")
		return
	}

	views := make([]sourceView, len(rows))
	for i, row := range rows {
		views[i] = sourceView{SourceRow: row, SyncModel: source.SyncModelFor(row.Type)}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(views)
}

// handleAddSource handles POST /api/sources — add a new source.
func (h *Handler) handleAddSource(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var row source.SourceRow
	if err := json.NewDecoder(r.Body).Decode(&row); err != nil {
		apiError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// Validate required fields.
	if strings.TrimSpace(row.Type) == "" {
		apiError(w, http.StatusBadRequest, "type must be non-empty")
		return
	}
	if strings.TrimSpace(row.Name) == "" {
		apiError(w, http.StatusBadRequest, "name must be non-empty")
		return
	}

	// Validate config_json is valid JSON if provided.
	if row.ConfigJSON != "" && !json.Valid([]byte(row.ConfigJSON)) {
		apiError(w, http.StatusBadRequest, "config_json must be valid JSON")
		return
	}

	if err := h.config.AddSource(ctx, &row); err != nil {
		h.logger.Error("add source", "error", err)
		apiError(w, http.StatusInternalServerError, "failed to add source")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleUpdateSource handles PUT /api/sources/{id} — update a source.
func (h *Handler) handleUpdateSource(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id := r.PathValue("id")
	if id == "" {
		apiError(w, http.StatusBadRequest, "missing source ID")
		return
	}

	var row source.SourceRow
	if err := json.NewDecoder(r.Body).Decode(&row); err != nil {
		apiError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// Validate required fields.
	if strings.TrimSpace(row.Type) == "" {
		apiError(w, http.StatusBadRequest, "type must be non-empty")
		return
	}
	if strings.TrimSpace(row.Name) == "" {
		apiError(w, http.StatusBadRequest, "name must be non-empty")
		return
	}

	// Validate config_json is valid JSON if provided.
	if row.ConfigJSON != "" && !json.Valid([]byte(row.ConfigJSON)) {
		apiError(w, http.StatusBadRequest, "config_json must be valid JSON")
		return
	}

	if err := h.config.UpdateSource(ctx, id, &row); err != nil {
		h.logger.Error("update source", "error", err)
		apiError(w, http.StatusInternalServerError, "failed to update source")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleDeleteSource handles DELETE /api/sources/{id} — delete a source.
func (h *Handler) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		apiError(w, http.StatusBadRequest, "missing source ID")
		return
	}

	if err := h.config.DeleteSource(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			apiError(w, http.StatusNotFound, "source not found")
			return
		}
		h.logger.Error("delete source", "error", err)
		apiError(w, http.StatusInternalServerError, "failed to delete source")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
