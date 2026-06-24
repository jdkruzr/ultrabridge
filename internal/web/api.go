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

// handleAPISearch handles GET /api/search. Optional ?limit=N caps the
// result count (0/absent → service default; out-of-range → service ceiling).
// Non-integer ?limit is treated as 0 (use default) rather than a 400 —
// keeps the surface friendly to MCP callers that send the param as a
// string sometimes.
func (h *Handler) handleAPISearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		apiError(w, http.StatusBadRequest, "missing required parameter: q")
		return
	}

	opts, _, _ := h.searchOptionsFromRequest(r)
	if _, ok := r.URL.Query()["source"]; !ok && r.URL.Query().Get("sources_submitted") != "1" {
		opts.Sources = nil
	}
	results, err := h.search.SearchAdvanced(r.Context(), q, opts)
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

// handleAPIForestNoteTextBoxes handles GET /api/forestnote/text-boxes?notebook=...
// It lists a notebook's live text boxes (id, page, text) so an agent can pick one
// to edit. Emits snake_case JSON to match the other note API endpoints.
func (h *Handler) handleAPIForestNoteTextBoxes(w http.ResponseWriter, r *http.Request) {
	if !h.notes.HasForestNoteSource() {
		apiError(w, http.StatusNotFound, "no forestnote source")
		return
	}
	notebookID := r.URL.Query().Get("notebook")
	if notebookID == "" {
		apiError(w, http.StatusBadRequest, "missing required parameter: notebook")
		return
	}
	refs, err := h.notes.ListForestNoteTextBoxes(r.Context(), notebookID)
	if err != nil {
		h.logger.Error("api list text boxes failed", "notebook", notebookID, "err", err)
		apiError(w, http.StatusInternalServerError, "failed to list text boxes")
		return
	}
	type box struct {
		ID     string `json:"id"`
		PageID string `json:"page_id"`
		Text   string `json:"text"`
		Z      int64  `json:"z"`
	}
	out := make([]box, len(refs))
	for i, r := range refs {
		out[i] = box{ID: r.ID, PageID: r.PageID, Text: r.Text, Z: r.Z}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// handleAPIForestNoteEditTextBox handles POST /api/forestnote/text-boxes/edit with
// a JSON body {"id": "...", "text": "..."}. It authors a server-side edit of the
// box's text (relayed to devices) and re-renders/re-indexes the affected page.
func (h *Handler) handleAPIForestNoteEditTextBox(w http.ResponseWriter, r *http.Request) {
	if !h.notes.HasForestNoteSource() {
		apiError(w, http.StatusNotFound, "no forestnote source")
		return
	}
	var body struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.ID == "" {
		apiError(w, http.StatusBadRequest, "missing required field: id")
		return
	}
	if err := h.notes.EditForestNoteTextBox(r.Context(), body.ID, body.Text); err != nil {
		h.logger.Error("api edit text box failed", "id", body.ID, "err", err)
		// A missing/deleted box is the client's fault; everything else is ours.
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}
