package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"

	"github.com/sysop/ultrabridge/internal/spcserver/auth"
	"github.com/sysop/ultrabridge/internal/spcserver/dedup"
	"github.com/sysop/ultrabridge/internal/spcserver/dto"
	"github.com/sysop/ultrabridge/internal/spcserver/envelope"
	"github.com/sysop/ultrabridge/internal/spcserver/groups"
	"github.com/sysop/ultrabridge/internal/spcserver/mapping"
	"github.com/sysop/ultrabridge/internal/taskstore"
)

// taskPageSize is the default /schedule/task/all page size (SPC default 20).
const taskPageSize = 20

// TaskStore is the subset of the CalDAV task store the schedule handlers need.
// taskdb.Store satisfies it.
type TaskStore interface {
	List(ctx context.Context) ([]taskstore.Task, error)
	Get(ctx context.Context, taskID string) (*taskstore.Task, error)
	Create(ctx context.Context, t *taskstore.Task) error
	Update(ctx context.Context, t *taskstore.Task) error
	Delete(ctx context.Context, taskID string) error
	MaxLastModified(ctx context.Context) (int64, error)
}

// ScheduleHandler serves the device-facing task group/task/sort endpoints,
// translating SPC JSON to/from the CalDAV task store at the boundary.
type ScheduleHandler struct {
	Store  TaskStore
	Groups groups.GroupProvider
	Dedup  *dedup.Checker
}

// --- Groups (Option A: single synthesized group; CRUD are success no-ops) ---

// GroupAll handles POST /api/file/schedule/group/all.
func (h *ScheduleHandler) GroupAll(w http.ResponseWriter, r *http.Request) {
	envelope.WriteJSON(w, dto.ScheduleTaskGroupVO{
		BaseVO:            envelope.OK(),
		ScheduleTaskGroup: h.Groups.Groups(userIDInt(r)),
	})
}

// GroupNoOp handles group create/update/delete/clear/get — accepted as
// well-formed success (UB has one collection; see the multi-collection seam).
func (h *ScheduleHandler) GroupNoOp(w http.ResponseWriter, r *http.Request) {
	envelope.WriteJSON(w, envelope.OK())
}

// --- Tasks ---

// TaskAll handles POST /api/file/schedule/task/all: tasks mapped from the store,
// paginated 20/page by lastModified ASC, with nextPageToken when more remain and
// nextSyncToken = max lastModified. The request's nextPageTokens (plural) is the
// page cursor (an offset).
func (h *ScheduleHandler) TaskAll(w http.ResponseWriter, r *http.Request) {
	var req dto.ScheduleTaskDTO
	_ = json.NewDecoder(r.Body).Decode(&req)

	all, err := h.Store.List(r.Context())
	if err != nil {
		envelope.WriteError(w, "E0330", "list failed")
		return
	}
	sort.Slice(all, func(i, j int) bool {
		li, lj := all[i].LastModified.Int64, all[j].LastModified.Int64
		if li != lj {
			return li < lj
		}
		return all[i].TaskID < all[j].TaskID
	})

	size := taskPageSize
	if n, errp := strconv.Atoi(req.MaxResults); errp == nil && n > 0 {
		size = n
	}
	offset := 0
	if n, errp := strconv.Atoi(req.NextPageTokens); errp == nil && n > 0 {
		offset = n
	}
	if offset > len(all) {
		offset = len(all)
	}
	end := offset + size
	if end > len(all) {
		end = len(all)
	}

	page := make([]dto.SPCTask, 0, end-offset)
	for _, t := range all[offset:end] {
		page = append(page, h.toSPC(t))
	}
	next := ""
	if end < len(all) {
		next = strconv.Itoa(end)
	}
	syncToken, _ := h.Store.MaxLastModified(r.Context())

	envelope.WriteJSON(w, dto.ScheduleTaskAllVO{
		BaseVO:        envelope.OK(),
		NextPageToken: next,
		NextSyncToken: syncToken,
		ScheduleTask:  page,
	})
}

// TaskCreate handles POST /api/file/schedule/task (deduped via ResubmitCheck).
func (h *ScheduleHandler) TaskCreate(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if h.Dedup != nil && h.Dedup.Seen(auth.UserID(r.Context()), "/schedule/task", body) {
		envelope.WriteJSON(w, envelope.OK()) // duplicate within 1s — single effect
		return
	}
	var s dto.SPCTask
	if err := json.Unmarshal(body, &s); err != nil {
		envelope.WriteError(w, "E0330", "bad task")
		return
	}
	t := mapping.SPCToTask(s)
	if t.TaskListID.String == "" {
		t.TaskListID.String, t.TaskListID.Valid = h.Groups.DefaultID(), true
	}
	if err := h.Store.Create(r.Context(), &t); err != nil {
		envelope.WriteError(w, "E0330", "create failed")
		return
	}
	envelope.WriteJSON(w, envelope.OK())
}

// TaskUpdate handles PUT /api/file/schedule/task.
func (h *ScheduleHandler) TaskUpdate(w http.ResponseWriter, r *http.Request) {
	var s dto.SPCTask
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		envelope.WriteError(w, "E0330", "bad task")
		return
	}
	t := mapping.SPCToTask(s)
	if err := h.Store.Update(r.Context(), &t); err != nil {
		envelope.WriteError(w, "E0330", "update failed")
		return
	}
	envelope.WriteJSON(w, envelope.OK())
}

// TaskListUpdate handles PUT /api/file/schedule/task/list (bulk upsert).
func (h *ScheduleHandler) TaskListUpdate(w http.ResponseWriter, r *http.Request) {
	var list []dto.SPCTask
	if err := json.NewDecoder(r.Body).Decode(&list); err != nil {
		envelope.WriteError(w, "E0330", "bad task list")
		return
	}
	for i := range list {
		t := mapping.SPCToTask(list[i])
		if t.TaskListID.String == "" {
			t.TaskListID.String, t.TaskListID.Valid = h.Groups.DefaultID(), true
		}
		// Upsert: update, falling back to create for new ids.
		if err := h.Store.Update(r.Context(), &t); err != nil {
			_ = h.Store.Create(r.Context(), &t)
		}
	}
	envelope.WriteJSON(w, envelope.OK())
}

// TaskDelete handles DELETE /api/file/schedule/task/{taskId} (soft delete).
func (h *ScheduleHandler) TaskDelete(w http.ResponseWriter, r *http.Request) {
	if err := h.Store.Delete(r.Context(), r.PathValue("taskId")); err != nil {
		envelope.WriteError(w, "E0330", "delete failed")
		return
	}
	envelope.WriteJSON(w, envelope.OK())
}

// TaskGet handles GET /api/file/schedule/task/{taskId}.
func (h *ScheduleHandler) TaskGet(w http.ResponseWriter, r *http.Request) {
	t, err := h.Store.Get(r.Context(), r.PathValue("taskId"))
	if err != nil || t == nil {
		envelope.WriteError(w, "E0330", "not found")
		return
	}
	envelope.WriteJSON(w, struct {
		envelope.BaseVO
		dto.SPCTask
	}{BaseVO: envelope.OK(), SPCTask: h.toSPC(*t)})
}

// toSPC maps a stored task to the wire shape and stamps the single group's id
// on output, since taskdb does not persist task_list_id (UB has one collection).
func (h *ScheduleHandler) toSPC(t taskstore.Task) dto.SPCTask {
	s := mapping.TaskToSPC(t)
	if s.TaskListID == "" {
		s.TaskListID = h.Groups.DefaultID()
	}
	return s
}

// --- Sort (UB doesn't reorder; stored/echoed stub) ---

// SortNoOp handles POST/PUT /schedule/sort and DELETE /schedule/sort/{taskListId}.
func (h *ScheduleHandler) SortNoOp(w http.ResponseWriter, r *http.Request) {
	envelope.WriteJSON(w, envelope.OK())
}

// QuerySort handles POST /api/file/query/schedule/sort.
func (h *ScheduleHandler) QuerySort(w http.ResponseWriter, r *http.Request) {
	envelope.WriteJSON(w, dto.GetScheduleSortVO{BaseVO: envelope.OK()})
}

// --- Summary stubs (device hits these every sync; AC4.7) ---

// SummaryStub handles POST /api/file/query/summary/{hash,group,id}.
func (h *ScheduleHandler) SummaryStub(w http.ResponseWriter, r *http.Request) {
	envelope.WriteJSON(w, envelope.OK())
}

// userIDInt returns the context userId as int64 (0 if absent/non-numeric).
func userIDInt(r *http.Request) int64 {
	n, _ := strconv.ParseInt(auth.UserID(r.Context()), 10, 64)
	return n
}
