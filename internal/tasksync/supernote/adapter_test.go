package supernote

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/tasksync"
)

// mockNotifier records Notify calls for testing.
type mockNotifier struct {
	mu       sync.Mutex
	notified bool
	lastErr  error
}

func (m *mockNotifier) Notify(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notified = true
	return m.lastErr
}

func (m *mockNotifier) wasNotified() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.notified
}

// mockSPCServer mocks the SPC REST API. Hand-rolled, no mocking libraries.
type mockSPCServer struct {
	server              *httptest.Server
	mu                  sync.Mutex
	token               string
	loginFails          bool
	createdTasks        map[string]SPCTask
	tasks               map[string]SPCTask // All tasks, keyed by ID
	recordedPushes      []struct {
		method string
		path   string
		body   []byte
	}
	nextLoginShouldFail bool // Triggers 401 on next request after login succeeds
}

func newMockSPCServer() *mockSPCServer {
	m := &mockSPCServer{
		token:        "test-jwt-token-" + fmt.Sprint(time.Now().UnixNano()),
		createdTasks: make(map[string]SPCTask),
		tasks:        make(map[string]SPCTask),
	}

	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()

		path := r.RequestURI
		if idx := strings.Index(path, "?"); idx >= 0 {
			path = path[:idx]
		}

		switch path {
		case "/api/official/user/query/random/code":
			resp := map[string]interface{}{
				"success":    true,
				"randomCode": "test-challenge-" + fmt.Sprint(time.Now().UnixNano()),
				"timestamp":  time.Now().UnixMilli(),
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)

		case "/api/official/user/account/login/new":
			if m.loginFails {
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]interface{}{"success": false})
				return
			}
			resp := map[string]interface{}{
				"success": true,
				"token":   m.token,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)

		case "/api/file/schedule/task/all":
			// Check token for 401 test
			if m.nextLoginShouldFail && r.Header.Get("x-access-token") == m.token {
				m.nextLoginShouldFail = false // Only fail once
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]interface{}{"success": false})
				return
			}

			resp := map[string]interface{}{
				"success":      true,
				"scheduleTask": m.tasksAsSlice(),
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)

		case "/api/file/schedule/task":
			if r.Header.Get("x-access-token") != m.token {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var task SPCTask
			json.NewDecoder(r.Body).Decode(&task)
			m.tasks[task.ID] = task
			m.createdTasks[task.ID] = task
			resp := map[string]interface{}{"success": true}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)

		case "/api/file/schedule/task/list":
			if r.Method != "PUT" {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if r.Header.Get("x-access-token") != m.token {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var body struct {
				UpdateScheduleTaskList []SPCTask `json:"updateScheduleTaskList"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			for _, task := range body.UpdateScheduleTaskList {
				m.tasks[task.ID] = task
			}
			resp := map[string]interface{}{"success": true}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)

		default:
			// Handle DELETE /api/file/schedule/task/{id}
			if r.Method == "DELETE" && strings.HasPrefix(path, "/api/file/schedule/task/") {
				if r.Header.Get("x-access-token") != m.token {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				taskID := strings.TrimPrefix(path, "/api/file/schedule/task/")
				delete(m.tasks, taskID)
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	return m
}

func (m *mockSPCServer) tasksAsSlice() []SPCTask {
	var result []SPCTask
	for _, t := range m.tasks {
		result = append(result, t)
	}
	return result
}

func (m *mockSPCServer) close() {
	m.server.Close()
}


func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

// Test AC2.1: Create an adapter, call Push with ChangeCreate. Verify STARTSYNC notification.
func TestAdapter_AC2_1_Push_Create_SendsNotification(t *testing.T) {
	mock := newMockSPCServer()
	defer mock.close()

	notifier := &mockNotifier{}
	adapter := NewAdapter(mock.server.URL, "test@example.com", "testpass", notifier, testLogger())

	ctx := context.Background()
	if err := adapter.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Push a create
	changes := []tasksync.Change{
		{
			Type:   tasksync.ChangeCreate,
			TaskID: "local-task-1",
			Remote: tasksync.RemoteTask{
				Title:  "Test Task",
				Detail: "Test detail",
				Status: "needsAction",
			},
		},
	}

	results, err := adapter.Push(ctx, changes)
	if err != nil {
		t.Fatalf("Push failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	if results[0].TaskID != "local-task-1" {
		t.Errorf("expected TaskID local-task-1, got %s", results[0].TaskID)
	}

	if results[0].RemoteID == "" {
		t.Errorf("expected RemoteID to be set, got empty")
	}

	// Verify STARTSYNC was sent
	if !notifier.wasNotified() {
		t.Errorf("notifier.Notify() was not called")
	}
}

// Test AC2.2: Push with status="completed". Verify mock SPC received the update.
func TestAdapter_AC2_2_Push_Update_Status(t *testing.T) {
	mock := newMockSPCServer()
	defer mock.close()

	// Pre-populate a task
	mock.mu.Lock()
	mock.tasks["remote-task-1"] = SPCTask{
		ID:     "remote-task-1",
		Title:  "Original Task",
		Status: "needsAction",
	}
	mock.mu.Unlock()

	notifier := &mockNotifier{}
	adapter := NewAdapter(mock.server.URL, "test@example.com", "testpass", notifier, testLogger())

	ctx := context.Background()
	if err := adapter.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Push an update with completed status
	changes := []tasksync.Change{
		{
			Type:     tasksync.ChangeUpdate,
			TaskID:   "local-task-1",
			RemoteID: "remote-task-1",
			Remote: tasksync.RemoteTask{
				RemoteID: "remote-task-1",
				Title:    "Original Task",
				Status:   "completed",
			},
		},
	}

	_, err := adapter.Push(ctx, changes)
	if err != nil {
		t.Fatalf("Push failed: %v", err)
	}

	// Verify the task was updated in mock SPC
	mock.mu.Lock()
	updatedTask, exists := mock.tasks["remote-task-1"]
	mock.mu.Unlock()

	if !exists {
		t.Fatalf("task remote-task-1 not found in mock SPC")
	}

	if updatedTask.Status != "completed" {
		t.Errorf("expected status=completed, got %s", updatedTask.Status)
	}
}

// Test AC2.3: Pull returns tasks with correct field mapping.
func TestAdapter_AC2_3_Pull_FieldMapping(t *testing.T) {
	mock := newMockSPCServer()
	defer mock.close()

	// Pre-populate a task with all fields
	mock.mu.Lock()
	mock.tasks["spc-task-1"] = SPCTask{
		ID:            "spc-task-1",
		Title:         "Test Task",
		Detail:        "Test detail",
		Status:        "needsAction",
		Importance:    "high",
		DueTime:       1609459200000,
		CompletedTime: 1609372800000,
		Recurrence:    "DAILY",
		IsReminderOn:  "Y",
		Links:         "http://example.com",
		IsDeleted:     "N",
	}
	mock.mu.Unlock()

	adapter := NewAdapter(mock.server.URL, "test@example.com", "testpass", nil, testLogger())

	ctx := context.Background()
	if err := adapter.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	tasks, token, err := adapter.Pull(ctx, "")
	if err != nil {
		t.Fatalf("Pull failed: %v", err)
	}

	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}

	rt := tasks[0]
	if rt.RemoteID != "spc-task-1" {
		t.Errorf("expected RemoteID=spc-task-1, got %s", rt.RemoteID)
	}
	if rt.Title != "Test Task" {
		t.Errorf("expected Title=Test Task, got %s", rt.Title)
	}
	if rt.Detail != "Test detail" {
		t.Errorf("expected Detail=Test detail, got %s", rt.Detail)
	}
	if rt.Status != "needsAction" {
		t.Errorf("expected Status=needsAction, got %s", rt.Status)
	}
	if rt.Importance != "high" {
		t.Errorf("expected Importance=high, got %s", rt.Importance)
	}
	if rt.DueTime != 1609459200000 {
		t.Errorf("expected DueTime=1609459200000, got %d", rt.DueTime)
	}
	if rt.CompletedTime != 1609372800000 {
		t.Errorf("expected CompletedTime=1609372800000, got %d", rt.CompletedTime)
	}
	if rt.Recurrence != "DAILY" {
		t.Errorf("expected Recurrence=DAILY, got %s", rt.Recurrence)
	}
	if rt.IsReminderOn != "Y" {
		t.Errorf("expected IsReminderOn=Y, got %s", rt.IsReminderOn)
	}
	if rt.Links != "http://example.com" {
		t.Errorf("expected Links=http://example.com, got %s", rt.Links)
	}

	// SPC doesn't return sync tokens
	if token != "" {
		t.Errorf("expected empty sync token, got %s", token)
	}
}

// Test AC2.4: Pull detects title change.
func TestAdapter_AC2_4_Pull_TitleChange(t *testing.T) {
	mock := newMockSPCServer()
	defer mock.close()

	// Set up initial task
	mock.mu.Lock()
	mock.tasks["task-1"] = SPCTask{
		ID:     "task-1",
		Title:  "Original Title",
		Status: "needsAction",
	}
	mock.mu.Unlock()

	adapter := NewAdapter(mock.server.URL, "test@example.com", "testpass", nil, testLogger())

	ctx := context.Background()
	if err := adapter.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// First pull
	tasks1, _, err := adapter.Pull(ctx, "")
	if err != nil {
		t.Fatalf("Pull 1 failed: %v", err)
	}
	if len(tasks1) != 1 || tasks1[0].Title != "Original Title" {
		t.Fatalf("First pull didn't get original title")
	}

	// Simulate device modifying the task
	mock.mu.Lock()
	mock.tasks["task-1"] = SPCTask{
		ID:     "task-1",
		Title:  "Modified Title",
		Status: "needsAction",
	}
	mock.mu.Unlock()

	// Second pull should reflect the change
	tasks2, _, err := adapter.Pull(ctx, "")
	if err != nil {
		t.Fatalf("Pull 2 failed: %v", err)
	}

	if len(tasks2) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks2))
	}

	if tasks2[0].Title != "Modified Title" {
		t.Errorf("expected Modified Title, got %s", tasks2[0].Title)
	}
}

// Test AC2.5: Push sends back UB's version (conflict resolution at engine level).
func TestAdapter_AC2_5_Conflict_UBVersion(t *testing.T) {
	mock := newMockSPCServer()
	defer mock.close()

	mock.mu.Lock()
	mock.tasks["task-1"] = SPCTask{
		ID:     "task-1",
		Title:  "Device version",
		Status: "needsAction",
	}
	mock.mu.Unlock()

	notifier := &mockNotifier{}
	adapter := NewAdapter(mock.server.URL, "test@example.com", "testpass", notifier, testLogger())

	ctx := context.Background()
	if err := adapter.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Push UB's version (should overwrite device version)
	changes := []tasksync.Change{
		{
			Type:     tasksync.ChangeUpdate,
			TaskID:   "local-task-1",
			RemoteID: "task-1",
			Remote: tasksync.RemoteTask{
				RemoteID: "task-1",
				Title:    "UB version",
				Status:   "needsAction",
			},
		},
	}

	_, err := adapter.Push(ctx, changes)
	if err != nil {
		t.Fatalf("Push failed: %v", err)
	}

	// Verify UB's version was pushed
	mock.mu.Lock()
	updated := mock.tasks["task-1"]
	mock.mu.Unlock()

	if updated.Title != "UB version" {
		t.Errorf("expected Title=UB version, got %s", updated.Title)
	}
}

// Test AC2.6: Adapter re-authenticates on 401.
func TestAdapter_AC2_6_ReAuth_On401(t *testing.T) {
	mock := newMockSPCServer()
	defer mock.close()

	mock.mu.Lock()
	mock.tasks["task-1"] = SPCTask{
		ID:     "task-1",
		Title:  "Test",
		Status: "needsAction",
	}
	mock.mu.Unlock()

	adapter := NewAdapter(mock.server.URL, "test@example.com", "testpass", nil, testLogger())

	ctx := context.Background()
	if err := adapter.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// First pull succeeds
	_, _, err := adapter.Pull(ctx, "")
	if err != nil {
		t.Fatalf("First pull failed: %v", err)
	}

	// Trigger next request to return 401
	mock.mu.Lock()
	mock.nextLoginShouldFail = true
	mock.mu.Unlock()

	// Second pull should trigger re-auth and retry
	_, _, err = adapter.Pull(ctx, "")
	if err != nil {
		t.Fatalf("Pull after 401 failed: %v", err)
	}
}

// Test AC2.7: SPC unreachable — adapter returns error.
func TestAdapter_AC2_7_SPC_Unreachable(t *testing.T) {
	mock := newMockSPCServer()
	url := mock.server.URL
	mock.close() // Close server before using adapter

	adapter := NewAdapter(url, "test@example.com", "testpass", nil, testLogger())

	ctx := context.Background()
	err := adapter.Start(ctx)
	if err == nil {
		t.Errorf("Expected Start to fail when server is down")
	}
}

// Test AC2.8: Auth failure (wrong password) — Start returns error.
func TestAdapter_AC2_8_Auth_Failure(t *testing.T) {
	mock := newMockSPCServer()
	defer mock.close()

	mock.mu.Lock()
	mock.loginFails = true
	mock.mu.Unlock()

	adapter := NewAdapter(mock.server.URL, "test@example.com", "wrongpass", nil, testLogger())

	ctx := context.Background()
	err := adapter.Start(ctx)
	if err == nil {
		t.Errorf("Expected Start to fail with auth failure")
	}
}

// Test: Pull filters deleted tasks.
func TestAdapter_Pull_FiltersDeleted(t *testing.T) {
	mock := newMockSPCServer()
	defer mock.close()

	mock.mu.Lock()
	mock.tasks["task-1"] = SPCTask{
		ID:        "task-1",
		Title:     "Active task",
		Status:    "needsAction",
		IsDeleted: "N",
	}
	mock.tasks["task-2"] = SPCTask{
		ID:        "task-2",
		Title:     "Deleted task",
		Status:    "needsAction",
		IsDeleted: "Y",
	}
	mock.mu.Unlock()

	adapter := NewAdapter(mock.server.URL, "test@example.com", "testpass", nil, testLogger())

	ctx := context.Background()
	if err := adapter.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	tasks, _, err := adapter.Pull(ctx, "")
	if err != nil {
		t.Fatalf("Pull failed: %v", err)
	}

	if len(tasks) != 1 {
		t.Fatalf("expected 1 active task, got %d", len(tasks))
	}

	if tasks[0].Title != "Active task" {
		t.Errorf("expected active task, got %s", tasks[0].Title)
	}
}

// Test: RoundTrip SPCTask → RemoteTask → SPCTask preserves fields.
func TestAdapter_RoundTrip_PreservesFields(t *testing.T) {
	original := SPCTask{
		ID:            "task-1",
		Title:         "Test",
		Detail:        "Detail",
		Status:        "completed",
		Importance:    "high",
		DueTime:       1609459200000,
		CompletedTime: 1609372800000,
		Recurrence:    "DAILY",
		IsReminderOn:  "Y",
		Links:         "http://example.com",
		IsDeleted:     "N",
	}

	// Convert to RemoteTask
	rt := SPCTaskToRemote(original)

	// Convert back to SPCTask with same ID
	recovered := RemoteToSPCTask(rt, original.ID)

	// Check all fields match
	if recovered.ID != original.ID {
		t.Errorf("ID mismatch: %s vs %s", recovered.ID, original.ID)
	}
	if recovered.Title != original.Title {
		t.Errorf("Title mismatch: %s vs %s", recovered.Title, original.Title)
	}
	if recovered.Detail != original.Detail {
		t.Errorf("Detail mismatch: %s vs %s", recovered.Detail, original.Detail)
	}
	if recovered.Status != original.Status {
		t.Errorf("Status mismatch: %s vs %s", recovered.Status, original.Status)
	}
	if recovered.Importance != original.Importance {
		t.Errorf("Importance mismatch: %s vs %s", recovered.Importance, original.Importance)
	}
	if recovered.DueTime != original.DueTime {
		t.Errorf("DueTime mismatch: %d vs %d", recovered.DueTime, original.DueTime)
	}
	if recovered.CompletedTime != original.CompletedTime {
		t.Errorf("CompletedTime mismatch: %d vs %d", recovered.CompletedTime, original.CompletedTime)
	}
	if recovered.Recurrence != original.Recurrence {
		t.Errorf("Recurrence mismatch: %s vs %s", recovered.Recurrence, original.Recurrence)
	}
	if recovered.IsReminderOn != original.IsReminderOn {
		t.Errorf("IsReminderOn mismatch: %s vs %s", recovered.IsReminderOn, original.IsReminderOn)
	}
	if recovered.Links != original.Links {
		t.Errorf("Links mismatch: %s vs %s", recovered.Links, original.Links)
	}
}

// Test: ETag stability — same input produces same ETag.
func TestAdapter_ETag_Stability(t *testing.T) {
	spc := SPCTask{
		ID:           "task-1",
		Title:        "Test",
		Status:       "needsAction",
		Detail:       "Detail",
		DueTime:      1609459200000,
		LastModified: 1609372800000,
	}

	etag1 := computeSPCETag(spc)
	etag2 := computeSPCETag(spc)

	if etag1 != etag2 {
		t.Errorf("ETag not stable: %s vs %s", etag1, etag2)
	}

	// Modify a field and verify ETag changes
	spc.Title = "Modified"
	etag3 := computeSPCETag(spc)

	if etag1 == etag3 {
		t.Errorf("ETag should change when field changes")
	}
}

// Test: Adapter ID
func TestAdapter_ID(t *testing.T) {
	adapter := NewAdapter("http://localhost", "test@example.com", "pass", nil, testLogger())
	if adapter.ID() != "supernote" {
		t.Errorf("expected ID=supernote, got %s", adapter.ID())
	}
}

// Test: Stop is no-op
func TestAdapter_Stop(t *testing.T) {
	adapter := NewAdapter("http://localhost", "test@example.com", "pass", nil, testLogger())
	if err := adapter.Stop(); err != nil {
		t.Errorf("Stop failed: %v", err)
	}
}

// TestClient_BugFix_CreateChecksSuccess verifies that the client returns an
// error when SPC's response body has success=false. Previously the success
// field was decoded but never checked, so silent rejections looked like
// success and produced phantom sync_map entries.
func TestClient_BugFix_CreateChecksSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.RequestURI {
		case "/api/official/user/query/random/code":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "randomCode": "x", "timestamp": int64(1)})
		case "/api/official/user/account/login/new":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "token": "tok"})
		case "/api/file/schedule/task":
			// SPC says no.
			json.NewEncoder(w).Encode(map[string]any{
				"success":  false,
				"code":     "OUT_OF_LIMIT",
				"errorMsg": "task quota exceeded",
			})
		default:
			t.Fatalf("unexpected path: %s", r.RequestURI)
		}
	}))
	defer server.Close()

	c := NewClient(server.URL, "u", "p", testLogger())
	ctx := context.Background()
	if err := c.Login(ctx); err != nil {
		t.Fatalf("Login: %v", err)
	}
	err := c.CreateTask(ctx, SPCTask{ID: "x", Title: "test"})
	if err == nil {
		t.Fatal("expected CreateTask to error on success=false, got nil")
	}
	if !strings.Contains(err.Error(), "OUT_OF_LIMIT") || !strings.Contains(err.Error(), "task quota exceeded") {
		t.Errorf("expected error to surface SPC code+message, got %q", err.Error())
	}
}

// TestClient_FetchTasks_Paginates verifies that FetchTasks follows SPC's
// nextPageToken across multiple pages. SPC's /task/all defaults to 20 results
// per page; previously FetchTasks read only page 1. Note the field-name asymmetry:
// requests carry `nextPageTokens` (plural), responses carry `nextPageToken`
// (singular) — this is a SPC quirk discovered in the decompiled service source.
func TestClient_FetchTasks_Paginates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.RequestURI {
		case "/api/official/user/query/random/code":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "randomCode": "x", "timestamp": int64(1)})
		case "/api/official/user/account/login/new":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "token": "tok"})
		case "/api/file/schedule/task/all":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			page := body["nextPageTokens"]
			switch page {
			case nil, "":
				json.NewEncoder(w).Encode(map[string]any{
					"success":       true,
					"scheduleTask":  []SPCTask{{ID: "a", Title: "a"}, {ID: "b", Title: "b"}},
					"nextPageToken": "2",
				})
			case "2":
				json.NewEncoder(w).Encode(map[string]any{
					"success":       true,
					"scheduleTask":  []SPCTask{{ID: "c", Title: "c"}},
					"nextPageToken": "3",
				})
			case "3":
				json.NewEncoder(w).Encode(map[string]any{
					"success":      true,
					"scheduleTask": []SPCTask{{ID: "d", Title: "d"}},
					// no nextPageToken: this is the last page
				})
			default:
				t.Fatalf("unexpected page token: %v", page)
			}
		default:
			t.Fatalf("unexpected path: %s", r.RequestURI)
		}
	}))
	defer server.Close()

	c := NewClient(server.URL, "u", "p", testLogger())
	ctx := context.Background()
	if err := c.Login(ctx); err != nil {
		t.Fatalf("Login: %v", err)
	}
	tasks, err := c.FetchTasks(ctx)
	if err != nil {
		t.Fatalf("FetchTasks: %v", err)
	}
	if len(tasks) != 4 {
		t.Fatalf("expected 4 tasks across 3 pages, got %d: %+v", len(tasks), tasks)
	}
	for i, want := range []string{"a", "b", "c", "d"} {
		if tasks[i].ID != want {
			t.Errorf("tasks[%d].ID = %q, want %q", i, tasks[i].ID, want)
		}
	}
}

// TestClient_BugFix_UpdateChecksSuccess: same guarantee for the bulk update.
func TestClient_BugFix_UpdateChecksSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.RequestURI {
		case "/api/official/user/query/random/code":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "randomCode": "x", "timestamp": int64(1)})
		case "/api/official/user/account/login/new":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "token": "tok"})
		case "/api/file/schedule/task/list":
			json.NewEncoder(w).Encode(map[string]any{
				"success":  false,
				"code":     "INVALID_TASK_LIST",
				"errorMsg": "one or more tasks not found",
			})
		default:
			t.Fatalf("unexpected path: %s", r.RequestURI)
		}
	}))
	defer server.Close()

	c := NewClient(server.URL, "u", "p", testLogger())
	ctx := context.Background()
	if err := c.Login(ctx); err != nil {
		t.Fatalf("Login: %v", err)
	}
	err := c.UpdateTasks(ctx, []SPCTask{{ID: "x", Title: "test"}})
	if err == nil {
		t.Fatal("expected UpdateTasks to error on success=false, got nil")
	}
	if !strings.Contains(err.Error(), "INVALID_TASK_LIST") {
		t.Errorf("expected error to surface SPC code, got %q", err.Error())
	}
}
