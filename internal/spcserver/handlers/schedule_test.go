package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/spcserver/dedup"
	"github.com/sysop/ultrabridge/internal/spcserver/dto"
	"github.com/sysop/ultrabridge/internal/spcserver/groups"
	"github.com/sysop/ultrabridge/internal/taskdb"
	"github.com/sysop/ultrabridge/internal/taskstore"
)

func newScheduleHandler(t *testing.T) (*ScheduleHandler, TaskStore) {
	t.Helper()
	db, err := taskdb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("taskdb open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := taskdb.NewStore(db)
	return &ScheduleHandler{
		Store:  store,
		Groups: groups.NewSingle("Tasks"),
		Dedup:  dedup.NewChecker(),
	}, store
}

func seedTask(t *testing.T, store TaskStore, title string, lastMod int64) {
	t.Helper()
	tk := &taskstore.Task{
		TaskID:       taskstore.GenerateTaskID(title, lastMod),
		Title:        sql.NullString{String: title, Valid: true},
		Status:       sql.NullString{String: "NEEDS-ACTION", Valid: true},
		LastModified: sql.NullInt64{Int64: lastMod, Valid: true},
		IsDeleted:    "N",
	}
	if err := store.Create(context.Background(), tk); err != nil {
		t.Fatalf("seed create: %v", err)
	}
}

// TestGroupAll returns the single synthesized group. Verifies: spc-phase-1.AC4.1
func TestGroupAll(t *testing.T) {
	h, _ := newScheduleHandler(t)
	rec := postJSON(t, h.GroupAll, `{}`)

	var vo dto.ScheduleTaskGroupVO
	if err := json.Unmarshal(rec.Body.Bytes(), &vo); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !vo.Success || len(vo.ScheduleTaskGroup) != 1 {
		t.Fatalf("expected 1 group success, got %s", rec.Body.String())
	}
	if vo.ScheduleTaskGroup[0].TaskListID != groups.DefaultID || vo.ScheduleTaskGroup[0].Title != "Tasks" {
		t.Errorf("group: %+v", vo.ScheduleTaskGroup[0])
	}
}

// TestTaskAllPaginates seeds 21 tasks → page 1 carries a nextPageToken, page 2
// empties it. Verifies: spc-phase-1.AC4.2
func TestTaskAllPaginates(t *testing.T) {
	h, store := newScheduleHandler(t)
	for i := 0; i < 21; i++ {
		seedTask(t, store, "task"+strconv.Itoa(i), int64(1000+i))
	}

	var page1 dto.ScheduleTaskAllVO
	json.Unmarshal(postJSON(t, h.TaskAll, `{}`).Body.Bytes(), &page1)
	if len(page1.ScheduleTask) != 20 || page1.NextPageToken == "" {
		t.Fatalf("page1: got %d tasks, token %q", len(page1.ScheduleTask), page1.NextPageToken)
	}

	var page2 dto.ScheduleTaskAllVO
	json.Unmarshal(postJSON(t, h.TaskAll, `{"nextPageTokens":"`+page1.NextPageToken+`"}`).Body.Bytes(), &page2)
	if len(page2.ScheduleTask) != 1 || page2.NextPageToken != "" {
		t.Errorf("page2: got %d tasks, token %q", len(page2.ScheduleTask), page2.NextPageToken)
	}
}

// TestTaskCreateAndDedup: a create lands a row; an immediate identical repeat is
// deduplicated. Verifies: spc-phase-1.AC4.3, AC4.4
func TestTaskCreateAndDedup(t *testing.T) {
	h, store := newScheduleHandler(t)
	body := `{"taskId":"t1","title":"buy milk","status":"needsAction","completedTime":1690000000000,"lastModified":1695000000000}`

	if !decodeSuccess(t, postJSON(t, h.TaskCreate, body)) {
		t.Fatalf("create should succeed")
	}
	got, err := store.Get(context.Background(), "t1")
	if err != nil || got == nil {
		t.Fatalf("task not stored: %v", err)
	}
	if got.Title.String != "buy milk" || got.Status.String != "needsAction" {
		t.Errorf("stored task wrong: %+v", got)
	}

	// Immediate identical repeat is deduped — still exactly one row.
	postJSON(t, h.TaskCreate, body)
	all, _ := store.List(context.Background())
	if len(all) != 1 {
		t.Errorf("dedup failed: %d rows", len(all))
	}
}

// TestTaskGetAndDelete verifies get returns the mapped task and delete soft-
// deletes it. Verifies: spc-phase-1.AC4.3
func TestTaskGetAndDelete(t *testing.T) {
	h, store := newScheduleHandler(t)
	seedTask(t, store, "findme", 2000)
	id := taskstore.GenerateTaskID("findme", 2000)

	rec := getWithPath(t, h.TaskGet, "taskId", id)
	var vo struct {
		Success    bool   `json:"success"`
		Title      string `json:"title"`
		TaskListID string `json:"taskListId"`
	}
	json.Unmarshal(rec.Body.Bytes(), &vo)
	if !vo.Success || vo.Title != "findme" {
		t.Errorf("get: %s", rec.Body.String())
	}
	// taskdb drops task_list_id; the handler stamps the single group's id on emit.
	if vo.TaskListID != groups.DefaultID {
		t.Errorf("emitted taskListId: got %q, want %q", vo.TaskListID, groups.DefaultID)
	}

	del := getWithPath(t, h.TaskDelete, "taskId", id)
	if !decodeSuccess(t, del) {
		t.Errorf("delete should succeed")
	}
}

// TestTaskListUpdateWrapper verifies the bulk update accepts the device's
// UpdateScheduleTaskListDTO wrapper (tasks under updateScheduleTaskList, not a
// bare array) and applies a completion. Regression for the "bad task list"
// E0330 seen on-device 2026-05-23. Verifies: spc-phase-1.AC4.3
func TestTaskListUpdateWrapper(t *testing.T) {
	h, store := newScheduleHandler(t)
	seedTask(t, store, "complete me", 3000)
	id := taskstore.GenerateTaskID("complete me", 3000)

	// Device-shaped wrapper: complete the task.
	body := `{"taskListId":"1","updateScheduleTaskList":[` +
		`{"taskId":"` + id + `","title":"complete me","status":"completed","completedTime":3000,"lastModified":9999}]}`
	if !decodeSuccess(t, postJSON(t, h.TaskListUpdate, body)) {
		t.Fatalf("task/list update should succeed on the wrapper shape")
	}

	got, err := store.Get(context.Background(), id)
	if err != nil || got == nil {
		t.Fatalf("task missing after update: %v", err)
	}
	if got.Status.String != "completed" {
		t.Errorf("expected completed after completion, got %q", got.Status.String)
	}

	// A bare array (the old wrong shape) must fail — guards the regression.
	if decodeSuccess(t, postJSON(t, h.TaskListUpdate, `[{"taskId":"x"}]`)) {
		t.Errorf("bare-array body should be rejected as bad task list")
	}
}

// TestSummaryStub verifies the summary stubs return success. Verifies: AC4.7
func TestSummaryStub(t *testing.T) {
	h, _ := newScheduleHandler(t)
	if !decodeSuccess(t, postJSON(t, h.SummaryStub, `{}`)) {
		t.Errorf("summary stub should succeed")
	}
}

// getWithPath invokes a handler with a path value set (for {taskId} routes).
func getWithPath(t *testing.T, fn http.HandlerFunc, key, val string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", strings.NewReader(""))
	req.SetPathValue(key, val)
	rec := httptest.NewRecorder()
	fn(rec, req)
	return rec
}
