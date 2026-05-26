package web

// Imperative Shell: HTTP API handlers with JSON serialization and filesystem I/O.

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

// apiError writes a JSON error response.
func apiError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// handleAPISearch handles GET /api/search
func (h *Handler) handleAPISearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		apiError(w, http.StatusBadRequest, "missing required parameter: q")
		return
	}

	folder := r.URL.Query().Get("folder")
	sources := r.URL.Query()["source"] // repeated: ?source=digest&source=supernote
	results, err := h.search.Search(r.Context(), q, folder, sources)
	if err != nil {
		h.logger.Error("api search failed", "err", err)
		apiError(w, http.StatusInternalServerError, "search failed")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

// handleAPIGetPages handles GET /api/notes/pages?path=...
func (h *Handler) handleAPIGetPages(w http.ResponseWriter, r *http.Request) {
	notePath := r.URL.Query().Get("path")
	if notePath == "" {
		apiError(w, http.StatusBadRequest, "missing required parameter: path")
		return
	}
	if !h.validNotePath(notePath) {
		apiError(w, http.StatusForbidden, "path outside notes directory")
		return
	}

	docs, err := h.notes.GetContent(r.Context(), notePath)
	if err != nil {
		h.logger.Error("api get pages failed", "path", notePath, "err", err)
		apiError(w, http.StatusInternalServerError, "failed to get pages")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(docs)
}

// handleAPIGetImage handles GET /api/notes/pages/image?path=...&page=...
func (h *Handler) handleAPIGetImage(w http.ResponseWriter, r *http.Request) {
	notePath := r.URL.Query().Get("path")
	if notePath == "" {
		apiError(w, http.StatusBadRequest, "missing required parameter: path")
		return
	}
	if !h.validNotePath(notePath) {
		apiError(w, http.StatusForbidden, "path outside notes directory")
		return
	}
	pageStr := r.URL.Query().Get("page")
	if pageStr == "" {
		apiError(w, http.StatusBadRequest, "missing required parameter: page")
		return
	}
	page, err := strconv.Atoi(pageStr)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid page number")
		return
	}

	stream, contentType, err := h.notes.RenderPage(r.Context(), notePath, page)
	if err != nil {
		h.logger.Error("api get image failed", "path", notePath, "err", err)
		apiError(w, http.StatusNotFound, "image not available")
		return
	}
	defer stream.Close()

	w.Header().Set("Content-Type", contentType)
	io.Copy(w, stream)
}
