package web

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sysop/ultrabridge/internal/auth"
	"github.com/sysop/ultrabridge/internal/service"
	"github.com/sysop/ultrabridge/internal/syncstore"
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
	h.mux.HandleFunc("POST /api/v1/tasks/purge-deleted", h.handleV1PurgeDeleted)
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

	// ForestNote sync device management. Handlers 404 when no SyncDeviceService
	// is wired (no ForestNote source configured).
	h.mux.HandleFunc("GET /api/v1/sync/devices", h.handleV1ListSyncDevices)
	h.mux.HandleFunc("DELETE /api/v1/sync/devices/{id}", h.handleV1PruneSyncDevice)
	h.mux.HandleFunc("POST /api/v1/sync/compact", h.handleV1SyncCompact)
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

// parseBoolQuery accepts "1"/"true"/"yes"/"on" (case-insensitive) as true;
// anything else is false. Keeps the surface friendly to MCP tools that send
// a stringified bool from JSON ("true") and to HTML form posts where an
// unchecked checkbox is absent and a checked one sends "on" by default.
func parseBoolQuery(s string) bool {
	switch strings.ToLower(s) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// containsCategory is an O(N) per-task equality scan used by the
// list-tasks filter (case-sensitive — matches the wire-level CATEGORIES
// value verbatim). Scale ceiling: this runs per-task in the post-fetch
// filter loop, so the total work is len(tasks) * avg(categories per task).
// At the current live-server shape (1.5k rows, a few categories per task
// at most) this is sub-millisecond; if either dimension grows by an order
// of magnitude, push the filter down into a SQL `category LIKE` index
// instead.
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
// call. Returns 200 + {"deleted": N}. (Was 204 no-body pre-UB-3; the
// shape change is documented in the inline comment below.)
func (h *Handler) handleV1PurgeCompleted(w http.ResponseWriter, r *http.Request) {
	n, err := h.tasks.PurgeCompleted(r.Context())
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to purge completed tasks")
		return
	}
	h.auditMutation(r, "purge_completed", "deleted", n)
	// Returns 200 + {"deleted": N} to match purge-deleted's response shape
	// and give MCP/CLI callers visibility into what actually happened.
	// Was 204 no-body previously; this is a soft-breaking change but every
	// known caller in-tree is updated in this commit. Browser POSTs through
	// the legacy /tasks/purge-completed route still get the empty-200 / 303
	// shape they expect.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int64{"deleted": n})
}

// purgeDeletedDefaultDays is the safety-window default when no
// ?older_than_days= is provided. Long enough that recently-deleted rows
// aren't surprise-cleared, short enough that the backlog doesn't grow
// unbounded. Callers wanting a different window pass it explicitly.
//
// Paired with `webPurgeDeletedDays` in handler.go (the legacy form-route
// constant). The two values are intentionally separate (one is per-API,
// one is per-UI) but should agree numerically — if you tune one without
// the other, the UI button and the REST default diverge.
const purgeDeletedDefaultDays = 30

// handleV1PurgeDeleted permanently removes soft-deleted tasks whose
// last_modified is older than ?older_than_days=N (default 30). Returns 200
// with {"deleted": N, "skipped": M}. This is irreversible. Days must be > 0 —
// pass an explicit small value (e.g. 1) to aggressively reap, never 0 to
// mean "all".
//
// `skipped` counts rows that were soft-deleted but inside the safety window;
// it disambiguates "0 purged because nothing was eligible" from "0 purged
// because the gate broke" for the caller.
func (h *Handler) handleV1PurgeDeleted(w http.ResponseWriter, r *http.Request) {
	days := purgeDeletedDefaultDays
	if raw := r.URL.Query().Get("older_than_days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			apiError(w, http.StatusBadRequest, "older_than_days must be a positive integer")
			return
		}
		days = n
	}

	purged, skipped, err := h.tasks.PurgeDeleted(r.Context(), days)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to purge deleted tasks")
		return
	}
	h.auditMutation(r, "purge_deleted", "older_than_days", days, "deleted", purged, "skipped", skipped)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int64{"deleted": purged, "skipped": skipped})
}

// handleV1ListSyncDevices lists ForestNote sync devices (the sync_cursors
// registry + derived health fields). 404 when no ForestNote source is active.
func (h *Handler) handleV1ListSyncDevices(w http.ResponseWriter, r *http.Request) {
	if h.syncDevices == nil {
		apiError(w, http.StatusNotFound, "no ForestNote sync source configured")
		return
	}
	devices, err := h.syncDevices.ListSyncDevices(r.Context())
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to list sync devices")
		return
	}
	if devices == nil {
		devices = []service.SyncDevice{} // emit [] not null
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"devices": devices})
}

// handleV1PruneSyncDevice deletes a device's sync registry row (spec §4.3
// prune). Cleanup only: the device's authored ops are retained, and a
// still-alive device re-registers on its next sync.
func (h *Handler) handleV1PruneSyncDevice(w http.ResponseWriter, r *http.Request) {
	if h.syncDevices == nil {
		apiError(w, http.StatusNotFound, "no ForestNote sync source configured")
		return
	}
	siteID := r.PathValue("id")
	if !syncstore.IsULID(siteID) {
		apiError(w, http.StatusBadRequest, "device id must be a 26-char ULID")
		return
	}
	if err := h.syncDevices.PruneSyncDevice(r.Context(), siteID); err != nil {
		if errors.Is(err, service.ErrSyncDeviceNotFound) {
			apiError(w, http.StatusNotFound, "no such device")
			return
		}
		apiError(w, http.StatusInternalServerError, "failed to prune device")
		return
	}
	h.auditMutation(r, "sync_device_prune", "site_id", siteID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"site_id": siteID, "pruned": true})
}

// handleV1SyncCompact runs one relay-log compaction pass on demand (typically
// right after pruning a dead device, to reclaim the history it pinned).
func (h *Handler) handleV1SyncCompact(w http.ResponseWriter, r *http.Request) {
	if h.syncDevices == nil {
		apiError(w, http.StatusNotFound, "no ForestNote sync source configured")
		return
	}
	result, err := h.syncDevices.CompactNow(r.Context())
	if err != nil {
		apiError(w, http.StatusInternalServerError, "compaction failed")
		return
	}
	h.auditMutation(r, "sync_compact",
		"watermark", result.Watermark,
		"collapsed_superseded", result.CollapsedSuperseded,
		"purged_tombstones", result.PurgedTombstones,
		"evicted", len(result.EvictedSites))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (h *Handler) handleV1CreateTask(w http.ResponseWriter, r *http.Request) {
	// Decode directly into the service-layer struct so JSON tags stay
	// single-sourced. Unknown fields are tolerated (forward-compat).
	var input service.TaskCreate
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		apiError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if input.Title == "" {
		apiError(w, http.StatusBadRequest, "title is required")
		return
	}

	task, err := h.tasks.Create(r.Context(), input)
	if err != nil {
		// The handler-level title check above means service.Create's
		// "title is required" error is unreachable from this path today.
		// When future validation errors land at the service boundary,
		// switch to a sentinel error (`errors.Is(err, service.ErrTaskValidation)`)
		// — string-matching on err.Error() to dispatch 400 vs 500 was already
		// fragile when chunk 5 introduced it and has now been removed.
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
