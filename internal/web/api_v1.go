package web

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sysop/ultrabridge/internal/auth"
	"github.com/sysop/ultrabridge/internal/service"
	"github.com/sysop/ultrabridge/internal/taskstore"
)

// auditMutation logs a task-mutation event with the caller's identity, so
// "why did that task disappear" is answerable post-hoc without replaying
// HTTP logs. Bearer-token requests include the token label; Basic Auth
// requests include the username. Extra kv pairs are appended as slog
// attributes.
func (h *Handler) auditMutation(r *http.Request, op string, extra ...any) {
	if h.logger == nil {
		return
	}
	id := auth.IdentityFromContext(r.Context())
	args := []any{"op", op, "auth_method", id.Method, "auth_label", id.Label}
	args = append(args, extra...)
	h.logger.Info("task mutation", args...)
}

// isTaskNotFound returns true when the underlying task store reports a
// missing id. The real taskdb returns taskstore.ErrNotFound; the in-memory
// test mocks return sql.ErrNoRows. Accept either so handlers behave
// identically in both environments.
func isTaskNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || taskstore.IsNotFound(err)
}

// RegisterAPIv1 registers all v1 standard API endpoints.
func (h *Handler) RegisterAPIv1() {
	// Tasks
	h.mux.HandleFunc("GET /api/v1/tasks", h.handleV1ListTasks)
	h.mux.HandleFunc("POST /api/v1/tasks", h.handleV1CreateTask)
	h.mux.HandleFunc("POST /api/v1/tasks/purge-completed", h.handleV1PurgeCompleted)
	h.mux.HandleFunc("GET /api/v1/tasks/{id}", h.handleV1GetTask)
	h.mux.HandleFunc("PATCH /api/v1/tasks/{id}", h.handleV1UpdateTask)
	h.mux.HandleFunc("POST /api/v1/tasks/{id}/complete", h.handleV1CompleteTask)
	h.mux.HandleFunc("DELETE /api/v1/tasks/{id}", h.handleV1DeleteTask)
	h.mux.HandleFunc("POST /api/v1/tasks/bulk", h.handleV1BulkTasks)

	// Files
	h.mux.HandleFunc("GET /api/v1/files", h.handleV1ListFiles)
	h.mux.HandleFunc("POST /api/v1/files/scan", h.handleV1ScanFiles)
	h.mux.HandleFunc("POST /api/v1/files/queue", h.handleV1EnqueueFile)
	h.mux.HandleFunc("GET /api/v1/files/content", h.handleV1GetFileContent)
	h.mux.HandleFunc("GET /api/v1/files/render", h.handleV1RenderFile)

	// Search & Chat
	h.mux.HandleFunc("GET /api/v1/search", h.handleV1Search)
	h.mux.HandleFunc("POST /api/v1/chat/ask", h.handleV1ChatAsk)

	// System
	h.mux.HandleFunc("GET /api/v1/status", h.handleV1Status)
	h.mux.HandleFunc("GET /api/v1/config", h.handleV1GetConfig)
	h.mux.HandleFunc("PUT /api/v1/config", h.handleV1UpdateConfig)
	h.mux.HandleFunc("POST /api/v1/client-error", h.handleV1ClientError)
}

// --- Tasks ---

// handleV1ListTasks returns active tasks, optionally filtered by status and
// due-date range. All filters are optional; when omitted the response shape
// matches the pre-filter contract (array of all active tasks).
//
// Query params:
//   - status=needs_action|completed|all (default: all)
//   - due_before=<RFC3339>: only tasks due strictly before this instant
//   - due_after=<RFC3339>: only tasks due at or after this instant
//
// Tasks with no due date are excluded when either due_before or due_after
// is supplied — a "when's this due" filter can't meaningfully match a task
// without a due date.
func (h *Handler) handleV1ListTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Pre-parse query params so a 400 doesn't burn a list query first.
	status := q.Get("status")
	switch status {
	case "", "all", "needs_action", "completed":
		// ok
	default:
		apiError(w, http.StatusBadRequest, "status must be needs_action, completed, or all")
		return
	}
	var dueBefore, dueAfter *time.Time
	if s := q.Get("due_before"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			apiError(w, http.StatusBadRequest, "due_before must be RFC3339")
			return
		}
		dueBefore = &t
	}
	if s := q.Get("due_after"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			apiError(w, http.StatusBadRequest, "due_after must be RFC3339")
			return
		}
		dueAfter = &t
	}
	includeDeleted := parseBoolQuery(q.Get("include_deleted"))
	notebookID := q.Get("notebook_id")
	notebookName := q.Get("notebook_name")
	source := q.Get("source")
	category := q.Get("category")
	priority := q.Get("priority")

	// Soft-deleted rows live below the default visibility waterline; only the
	// caller who asked for them gets them.
	var (
		tasks []service.Task
		err   error
	)
	if includeDeleted {
		tasks, err = h.tasks.ListIncludingDeleted(r.Context())
	} else {
		tasks, err = h.tasks.List(r.Context())
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to list tasks")
		return
	}

	filtered := make([]service.Task, 0, len(tasks))
	for _, t := range tasks {
		switch status {
		case "needs_action":
			if t.Status != service.StatusNeedsAction {
				continue
			}
		case "completed":
			if t.Status != service.StatusCompleted {
				continue
			}
		}
		if dueBefore != nil || dueAfter != nil {
			if t.DueAt == nil {
				continue
			}
			if dueBefore != nil && !t.DueAt.Before(*dueBefore) {
				continue
			}
			if dueAfter != nil && t.DueAt.Before(*dueAfter) {
				continue
			}
		}
		if notebookID != "" {
			if t.ForestNote == nil || t.ForestNote.NotebookID != notebookID {
				continue
			}
		}
		if notebookName != "" {
			if t.ForestNote == nil || t.ForestNote.NotebookName != notebookName {
				continue
			}
		}
		if source != "" {
			if t.ForestNote == nil || t.ForestNote.Source != source {
				continue
			}
		}
		if category != "" {
			if !containsCategory(t.Categories, category) {
				continue
			}
		}
		if priority != "" {
			if t.Priority == nil || *t.Priority != priority {
				continue
			}
		}
		filtered = append(filtered, t)
	}

	w.Header().Set("Content-Type", "application/json")
	if filtered == nil {
		filtered = []service.Task{}
	}
	json.NewEncoder(w).Encode(filtered)
}

// parseBoolQuery accepts "1"/"true"/"yes" (case-insensitive) as true; anything
// else is false. Keeps the surface friendly to MCP tools that might send a
// stringified bool from JSON.
func parseBoolQuery(s string) bool {
	switch strings.ToLower(s) {
	case "1", "true", "yes":
		return true
	}
	return false
}

func containsCategory(cats []string, want string) bool {
	for _, c := range cats {
		if c == want {
			return true
		}
	}
	return false
}

// handleV1GetTask returns a single task by id. 404 when the id is unknown
// or soft-deleted.
func (h *Handler) handleV1GetTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	task, err := h.tasks.Get(r.Context(), id)
	if err != nil {
		if isTaskNotFound(err) {
			apiError(w, http.StatusNotFound, "task not found")
			return
		}
		apiError(w, http.StatusInternalServerError, "failed to fetch task")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(task)
}

// handleV1UpdateTask applies a partial update. Unknown fields in the JSON
// body are ignored; omitted fields leave the task untouched. See
// service.TaskPatch for the field-level semantics (ClearDueAt, empty-title
// rejection).
func (h *Handler) handleV1UpdateTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var patch service.TaskPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		apiError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	updated, err := h.tasks.Update(r.Context(), id, patch)
	if err != nil {
		if isTaskNotFound(err) {
			apiError(w, http.StatusNotFound, "task not found")
			return
		}
		// title-required, future validation errors — surface message to client.
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.auditMutation(r, "update", "task_id", id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(updated)
}

// handleV1PurgeCompleted soft-deletes every completed task in a single
// call. Returns 204 on success.
func (h *Handler) handleV1PurgeCompleted(w http.ResponseWriter, r *http.Request) {
	if err := h.tasks.PurgeCompleted(r.Context()); err != nil {
		apiError(w, http.StatusInternalServerError, "failed to purge completed tasks")
		return
	}
	h.auditMutation(r, "purge_completed")
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleV1CreateTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string     `json:"title"`
		DueAt *time.Time `json:"due_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Title == "" {
		apiError(w, http.StatusBadRequest, "title is required")
		return
	}

	task, err := h.tasks.Create(r.Context(), req.Title, req.DueAt)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to create task")
		return
	}
	h.auditMutation(r, "create", "task_id", task.ID, "title", task.Title)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(task)
}

func (h *Handler) handleV1CompleteTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.tasks.Complete(r.Context(), id); err != nil {
		if isTaskNotFound(err) {
			apiError(w, http.StatusNotFound, "task not found")
			return
		}
		apiError(w, http.StatusInternalServerError, "failed to complete task")
		return
	}
	h.auditMutation(r, "complete", "task_id", id)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleV1DeleteTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.tasks.Delete(r.Context(), id); err != nil {
		if isTaskNotFound(err) {
			apiError(w, http.StatusNotFound, "task not found")
			return
		}
		apiError(w, http.StatusInternalServerError, "failed to delete task")
		return
	}
	h.auditMutation(r, "delete", "task_id", id)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleV1BulkTasks(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string   `json:"action"`
		IDs    []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var err error
	switch req.Action {
	case "complete":
		err = h.tasks.BulkComplete(r.Context(), req.IDs)
	case "delete":
		err = h.tasks.BulkDelete(r.Context(), req.IDs)
	default:
		apiError(w, http.StatusBadRequest, "invalid action")
		return
	}

	if err != nil {
		apiError(w, http.StatusInternalServerError, "bulk operation failed")
		return
	}
	h.auditMutation(r, "bulk_"+req.Action, "count", len(req.IDs))
	w.WriteHeader(http.StatusNoContent)
}

// --- Files ---

func (h *Handler) handleV1ListFiles(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	sort := r.URL.Query().Get("sort")
	order := r.URL.Query().Get("order")

	files, _, err := h.notes.ListFiles(r.Context(), path, sort, order, 0, 0)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to list files")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(files)
}

func (h *Handler) handleV1ScanFiles(w http.ResponseWriter, r *http.Request) {
	if err := h.notes.ScanFiles(r.Context()); err != nil {
		apiError(w, http.StatusInternalServerError, "failed to trigger scan")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) handleV1EnqueueFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path  string `json:"path"`
		Force bool   `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.notes.Enqueue(r.Context(), req.Path, req.Force); err != nil {
		apiError(w, http.StatusInternalServerError, "failed to enqueue file")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) handleV1GetFileContent(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	content, err := h.notes.GetContent(r.Context(), path)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to get content")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(content)
}

func (h *Handler) handleV1RenderFile(w http.ResponseWriter, r *http.Request) {
	// For v1, we might just proxy to the existing logic but keep it here for standard
	h.handleFilesRender(w, r)
}

// --- Search & Chat ---

func (h *Handler) handleV1Search(w http.ResponseWriter, r *http.Request) {
	h.handleAPISearch(w, r)
}

func (h *Handler) handleV1ChatAsk(w http.ResponseWriter, r *http.Request) {
	h.handleAsk(w, r)
}

// --- System ---

func (h *Handler) handleV1Status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	jobStatus, _ := h.notes.GetProcessorStatus(ctx)

	resp := map[string]interface{}{
		"jobs": jobStatus,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *Handler) handleV1GetConfig(w http.ResponseWriter, r *http.Request) {
	h.handleGetConfig(w, r)
}

func (h *Handler) handleV1UpdateConfig(w http.ResponseWriter, r *http.Request) {
	h.handlePutConfig(w, r)
}

func (h *Handler) handleV1ClientError(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		URL     string `json:"url"`
		Status  int    `json:"status"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		apiError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	h.logger.Warn("frontend client error",
		"url", payload.URL,
		"status", payload.Status,
		"message", payload.Message,
	)

	w.WriteHeader(http.StatusNoContent)
}
