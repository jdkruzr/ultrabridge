package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/auth"
	"github.com/sysop/ultrabridge/internal/logging"
	"github.com/sysop/ultrabridge/internal/service"
)

// TestAPIv1GetTask verifies GET /api/v1/tasks/{id} returns the task JSON and
// 404s on unknown ids.
func TestAPIv1GetTask(t *testing.T) {
	h := newTestHandler()
	tasks := h.tasks.(*mockTaskService)
	tasks.tasks = []service.Task{
		{ID: "t1", Title: "Draft", Status: service.StatusNeedsAction},
	}

	t.Run("found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var got service.Task
		if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.ID != "t1" || got.Title != "Draft" {
			t.Errorf("unexpected task: %+v", got)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/missing", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("status=%d, want 404; body=%s", w.Code, w.Body.String())
		}
	})
}

// TestAPIv1UpdateTask verifies PATCH /api/v1/tasks/{id} applies a partial
// update and returns the post-write task JSON.
func TestAPIv1UpdateTask(t *testing.T) {
	h := newTestHandler()
	tasks := h.tasks.(*mockTaskService)
	due := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	tasks.tasks = []service.Task{
		{ID: "t1", Title: "Original", Status: service.StatusNeedsAction, DueAt: &due},
	}

	t.Run("title_only", func(t *testing.T) {
		body := `{"title":"Renamed"}`
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/tasks/t1", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var got service.Task
		if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Title != "Renamed" {
			t.Errorf("title not applied: %q", got.Title)
		}
		if got.DueAt == nil {
			t.Errorf("due date should be preserved on partial update")
		}
	})

	t.Run("clear_due_date", func(t *testing.T) {
		body := `{"clear_due_at":true}`
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/tasks/t1", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var got service.Task
		if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.DueAt != nil {
			t.Errorf("due date should be cleared; got %v", got.DueAt)
		}
	})

	t.Run("invalid_json", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/tasks/t1", strings.NewReader("{bad"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status=%d, want 400", w.Code)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		body := `{"title":"ghost"}`
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/tasks/missing", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("status=%d, want 404", w.Code)
		}
	})
}

// TestAPIv1PurgeCompleted verifies POST /api/v1/tasks/purge-completed
// invokes the service and returns 200 with {"deleted": N}. Previously
// returned 204-no-body; the count was added per UB-3 so MCP/CLI callers
// can surface "Soft-deleted N completed task(s)." rather than the
// opaque "All completed tasks purged." they used to get.
func TestAPIv1PurgeCompleted(t *testing.T) {
	h := newTestHandler()
	tasks := h.tasks.(*mockTaskService)
	tasks.tasks = []service.Task{
		{ID: "t1", Status: service.StatusCompleted},
		{ID: "t2", Status: service.StatusNeedsAction},
		{ID: "t3", Status: service.StatusCompleted},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/purge-completed", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Deleted int64 `json:"deleted"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Deleted != 2 {
		t.Errorf("deleted: got %d, want 2", body.Deleted)
	}
	// The mock's PurgeCompleted drops completed tasks from the slice.
	if len(tasks.tasks) != 1 || tasks.tasks[0].ID != "t2" {
		t.Errorf("expected only t2 to remain, got %+v", tasks.tasks)
	}
}

// TestAPIv1ListTasksFilters verifies the optional status + due_range filters
// and that unfiltered requests still return the full active list.
func TestAPIv1ListTasksFilters(t *testing.T) {
	h := newTestHandler()
	tasks := h.tasks.(*mockTaskService)
	dueSoon := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	dueLater := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	tasks.tasks = []service.Task{
		{ID: "t1", Title: "soon active", Status: service.StatusNeedsAction, DueAt: &dueSoon},
		{ID: "t2", Title: "later active", Status: service.StatusNeedsAction, DueAt: &dueLater},
		{ID: "t3", Title: "done", Status: service.StatusCompleted},
		{ID: "t4", Title: "no due", Status: service.StatusNeedsAction},
	}

	call := func(path string) []service.Task {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s -> %d body=%s", path, w.Code, w.Body.String())
		}
		var got []service.Task
		if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got
	}

	t.Run("no_filter_returns_all", func(t *testing.T) {
		got := call("/api/v1/tasks")
		if len(got) != 4 {
			t.Errorf("want 4 tasks, got %d: %+v", len(got), got)
		}
	})

	t.Run("status_needs_action", func(t *testing.T) {
		got := call("/api/v1/tasks?status=needs_action")
		if len(got) != 3 {
			t.Errorf("want 3 needs_action tasks, got %d", len(got))
		}
		for _, g := range got {
			if g.Status != service.StatusNeedsAction {
				t.Errorf("unexpected status %q", g.Status)
			}
		}
	})

	t.Run("status_completed", func(t *testing.T) {
		got := call("/api/v1/tasks?status=completed")
		if len(got) != 1 || got[0].ID != "t3" {
			t.Errorf("want only t3, got %+v", got)
		}
	})

	t.Run("due_before_excludes_no_due", func(t *testing.T) {
		got := call("/api/v1/tasks?due_before=2026-06-01T00:00:00Z")
		// t1 is before; t2 is not; t3, t4 excluded (t4 has no due date).
		if len(got) != 1 || got[0].ID != "t1" {
			t.Errorf("want only t1, got %+v", got)
		}
	})

	t.Run("due_after_range", func(t *testing.T) {
		got := call("/api/v1/tasks?due_after=2026-06-01T00:00:00Z")
		if len(got) != 1 || got[0].ID != "t2" {
			t.Errorf("want only t2, got %+v", got)
		}
	})

	t.Run("invalid_status", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks?status=bogus", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status=%d, want 400", w.Code)
		}
	})

	t.Run("invalid_due_before", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks?due_before=nope", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status=%d, want 400", w.Code)
		}
	})
}

// TestAPIv1ListTasksForestNoteFilters exercises the new ?notebook_id /
// ?notebook_name / ?source / ?category / ?priority / ?include_deleted query
// params introduced alongside FN-side X-FORESTNOTE-* emission.
func TestAPIv1ListTasksForestNoteFilters(t *testing.T) {
	h := newTestHandler()
	tasks := h.tasks.(*mockTaskService)
	priHigh := "1"
	priNorm := "5"
	tasks.tasks = []service.Task{
		{
			ID: "f1", Title: "from notebook A (lasso)", Status: service.StatusNeedsAction,
			Priority: &priHigh, Categories: []string{"work", "urgent"},
			ForestNote: &service.TaskForestNote{NotebookID: "A", NotebookName: "Project Notes", Source: "lasso"},
		},
		{
			ID: "f2", Title: "from notebook B (lasso)", Status: service.StatusNeedsAction,
			Priority: &priNorm, Categories: []string{"personal"},
			ForestNote: &service.TaskForestNote{NotebookID: "B", NotebookName: "Grocery", Source: "lasso"},
		},
		{
			ID: "f3", Title: "non-FN task", Status: service.StatusNeedsAction,
		},
		{
			ID: "f4", Title: "soft-deleted", Status: service.StatusNeedsAction, Deleted: true,
		},
	}

	call := func(path string) []service.Task {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s -> %d body=%s", path, w.Code, w.Body.String())
		}
		var got []service.Task
		if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got
	}

	t.Run("notebook_id_filter", func(t *testing.T) {
		got := call("/api/v1/tasks?notebook_id=A")
		if len(got) != 1 || got[0].ID != "f1" {
			t.Errorf("want only f1, got %+v", ids(got))
		}
	})

	t.Run("notebook_name_filter", func(t *testing.T) {
		got := call("/api/v1/tasks?notebook_name=Grocery")
		if len(got) != 1 || got[0].ID != "f2" {
			t.Errorf("want only f2, got %+v", ids(got))
		}
	})

	t.Run("source_filter", func(t *testing.T) {
		got := call("/api/v1/tasks?source=lasso")
		// f1, f2 carry lasso; f3 is non-FN; f4 is hidden.
		if len(got) != 2 {
			t.Errorf("want 2 lasso tasks, got %d: %v", len(got), ids(got))
		}
	})

	t.Run("category_filter", func(t *testing.T) {
		got := call("/api/v1/tasks?category=urgent")
		if len(got) != 1 || got[0].ID != "f1" {
			t.Errorf("want only f1, got %v", ids(got))
		}
	})

	t.Run("priority_filter", func(t *testing.T) {
		got := call("/api/v1/tasks?priority=1")
		if len(got) != 1 || got[0].ID != "f1" {
			t.Errorf("want only f1, got %v", ids(got))
		}
	})

	t.Run("include_deleted_surfaces_trash", func(t *testing.T) {
		got := call("/api/v1/tasks?include_deleted=true")
		// All four rows visible — f4 included.
		if len(got) != 4 {
			t.Errorf("want 4 tasks, got %d: %v", len(got), ids(got))
		}
		var sawDeleted bool
		for _, x := range got {
			if x.Deleted {
				sawDeleted = true
			}
		}
		if !sawDeleted {
			t.Error("expected at least one task with Deleted=true in include_deleted result")
		}
	})

	t.Run("default_hides_deleted", func(t *testing.T) {
		got := call("/api/v1/tasks")
		if len(got) != 3 {
			t.Errorf("want 3 (no trash), got %d: %v", len(got), ids(got))
		}
		for _, x := range got {
			if x.Deleted {
				t.Errorf("default list leaked deleted task %q", x.ID)
			}
		}
	})

	t.Run("filters_compose", func(t *testing.T) {
		// notebook_id=A + status=needs_action + priority=1 should still be just f1.
		got := call("/api/v1/tasks?notebook_id=A&status=needs_action&priority=1")
		if len(got) != 1 || got[0].ID != "f1" {
			t.Errorf("want only f1, got %v", ids(got))
		}
	})
}

func ids(ts []service.Task) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

// newHandlerWithLogBuf returns a handler whose logger writes to the returned
// buffer, so tests can assert on emitted audit lines.
func newHandlerWithLogBuf() (*Handler, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	broadcaster := logging.NewLogBroadcaster()
	notes := &mockNoteService{
		docs:               make(map[string][]service.SearchResult),
		contents:           make(map[string]interface{}),
		pipelineConfigured: true,
		booxEnabled:        true,
	}
	h := NewHandler(
		&mockTaskService{},
		notes,
		&mockSearchService{},
		&mockConfigService{},
		nil,
		"",
		"",
		logger,
		broadcaster,
	)
	return h, buf
}

// TestAuditLogIncludesBearerIdentity verifies that a mutation made with a
// bearer-auth Identity installed in context produces a "task mutation"
// log line carrying the token label.
func TestAuditLogIncludesBearerIdentity(t *testing.T) {
	h, buf := newHandlerWithLogBuf()

	body := `{"title":"audited"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Install Identity as if the middleware had run.
	ctx := auth.WithIdentity(req.Context(), auth.Identity{Method: "bearer", Label: "claude-desktop"})
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	logged := buf.String()
	if !strings.Contains(logged, "task mutation") {
		t.Errorf("audit line missing; log:\n%s", logged)
	}
	if !strings.Contains(logged, "op=create") {
		t.Errorf("op tag missing; log:\n%s", logged)
	}
	if !strings.Contains(logged, "auth_method=bearer") {
		t.Errorf("auth_method tag missing; log:\n%s", logged)
	}
	if !strings.Contains(logged, "auth_label=claude-desktop") {
		t.Errorf("auth_label tag missing; log:\n%s", logged)
	}
}

// TestAuditLogAnonymousWhenNoIdentity verifies the audit line still fires
// with empty identity fields when a handler is invoked without the
// middleware (e.g. tests or loopback). No panic, empty labels.
func TestAuditLogAnonymousWhenNoIdentity(t *testing.T) {
	h, buf := newHandlerWithLogBuf()

	body := `{"title":"x"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	logged := buf.String()
	if !strings.Contains(logged, "task mutation") {
		t.Errorf("audit line missing; log:\n%s", logged)
	}
	if !strings.Contains(logged, "auth_method=") {
		t.Errorf("auth_method tag missing; log:\n%s", logged)
	}
}

// TestAuditLogFiresOnAllMutations verifies every mutation handler emits one
// "task mutation" line — we wouldn't want a silent gap where an MCP agent
// can quietly modify state without a record.
func TestAuditLogFiresOnAllMutations(t *testing.T) {
	cases := []struct {
		name   string
		req    *http.Request
		wantOp string
	}{
		{
			name:   "create",
			req:    httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(`{"title":"x"}`)),
			wantOp: "op=create",
		},
		{
			name:   "purge_completed",
			req:    httptest.NewRequest(http.MethodPost, "/api/v1/tasks/purge-completed", nil),
			wantOp: "op=purge_completed",
		},
		{
			name:   "bulk_complete",
			req:    httptest.NewRequest(http.MethodPost, "/api/v1/tasks/bulk", strings.NewReader(`{"action":"complete","ids":["a","b"]}`)),
			wantOp: "op=bulk_complete",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, buf := newHandlerWithLogBuf()
			tc.req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, tc.req)
			if w.Code >= 400 {
				t.Fatalf("%s returned %d: %s", tc.name, w.Code, w.Body.String())
			}
			if !strings.Contains(buf.String(), tc.wantOp) {
				t.Errorf("missing %q in log:\n%s", tc.wantOp, buf.String())
			}
		})
	}
}

// TestAuthMiddlewareInstallsIdentity verifies end-to-end that a successful
// bearer-token validation through auth.Middleware attaches the label to the
// request context read by downstream handlers.
func TestAuthMiddlewareInstallsIdentity(t *testing.T) {
	mw := auth.NewDynamic(func() (string, string) { return "", "" })
	mw.SetTokenValidator(func(token string) (string, error) {
		if token == "secret" {
			return "test-token", nil
		}
		return "", strings.NewReader("").UnreadByte() // arbitrary non-nil error
	})

	var observed auth.Identity
	wrapped := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = auth.IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if observed.Method != "bearer" || observed.Label != "test-token" {
		t.Errorf("identity not propagated: %+v", observed)
	}
}


// TestAPIv1PurgeDeleted exercises the hard-purge endpoint: default 30-day
// cutoff when no query param, explicit ?older_than_days=N override, and
// the rejection of malformed/zero/negative values. We use the mock's
// purgeDeletedFn hook so we can observe the days that reach the service —
// the in-memory mock doesn't carry timestamps.
func TestAPIv1PurgeDeleted(t *testing.T) {
	t.Run("default cutoff is 30 days; response carries deleted+skipped", func(t *testing.T) {
		h := newTestHandler()
		tasks := h.tasks.(*mockTaskService)
		var observedDays int
		tasks.purgeDeletedFn = func(ctx context.Context, days int) (purged, skipped int64, err error) {
			observedDays = days
			return 7, 3, nil
		}

		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/purge-deleted", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
		}
		if observedDays != 30 {
			t.Errorf("default days: got %d, want 30", observedDays)
		}
		var body struct {
			Deleted int64 `json:"deleted"`
			Skipped int64 `json:"skipped"`
		}
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Deleted != 7 {
			t.Errorf("deleted: got %d, want 7", body.Deleted)
		}
		if body.Skipped != 3 {
			t.Errorf("skipped: got %d, want 3", body.Skipped)
		}
	})

	t.Run("explicit older_than_days is honored", func(t *testing.T) {
		h := newTestHandler()
		tasks := h.tasks.(*mockTaskService)
		var observedDays int
		tasks.purgeDeletedFn = func(ctx context.Context, days int) (purged, skipped int64, err error) {
			observedDays = days
			return 1500, 0, nil
		}

		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/purge-deleted?older_than_days=7", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200", w.Code)
		}
		if observedDays != 7 {
			t.Errorf("explicit days: got %d, want 7", observedDays)
		}
	})

	t.Run("rejects zero, negative, and non-integer days", func(t *testing.T) {
		for _, raw := range []string{"0", "-1", "abc", "1.5"} {
			h := newTestHandler()
			req := httptest.NewRequest(http.MethodPost,
				"/api/v1/tasks/purge-deleted?older_than_days="+raw, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("raw=%q: status=%d, want 400", raw, w.Code)
			}
		}
	})

	t.Run("service error surfaces as 500", func(t *testing.T) {
		h := newTestHandler()
		tasks := h.tasks.(*mockTaskService)
		tasks.purgeDeletedFn = func(ctx context.Context, days int) (purged, skipped int64, err error) {
			return 0, 0, errors.New("disk full")
		}

		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/purge-deleted", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Errorf("status=%d, want 500; body=%s", w.Code, w.Body.String())
		}
	})
}


// TestAPIv1CreateTaskNewFields verifies POST /api/v1/tasks accepts the
// extended write surface (detail, url, priority, categories, comment) and
// returns them on the created task.
func TestAPIv1CreateTaskNewFields(t *testing.T) {
	h := newTestHandler()

	body := `{
		"title": "rich create",
		"detail": "ctx body",
		"url": "https://ub.example/n/abc",
		"priority": "1",
		"categories": ["work", "urgent"],
		"comment": "from a meeting"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got service.Task
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Title != "rich create" {
		t.Errorf("title: %q", got.Title)
	}
	if got.Detail == nil || *got.Detail != "ctx body" {
		t.Errorf("detail: %+v", got.Detail)
	}
	if got.URL == nil || *got.URL != "https://ub.example/n/abc" {
		t.Errorf("url: %+v", got.URL)
	}
	if got.Priority == nil || *got.Priority != "1" {
		t.Errorf("priority: %+v", got.Priority)
	}
	if len(got.Categories) != 2 || got.Categories[0] != "work" {
		t.Errorf("categories: %v", got.Categories)
	}
	if got.Comment != "from a meeting" {
		t.Errorf("comment: %q", got.Comment)
	}
}

// TestAPIv1UpdateTaskNewFields verifies PATCH /api/v1/tasks/{id} accepts
// the same extended fields and respects the Clear* sentinels.
func TestAPIv1UpdateTaskNewFields(t *testing.T) {
	h := newTestHandler()
	tasks := h.tasks.(*mockTaskService)
	url := "https://old/"
	prio := "5"
	tasks.tasks = []service.Task{
		{
			ID:         "t1",
			Title:      "orig",
			Status:     service.StatusNeedsAction,
			URL:        &url,
			Priority:   &prio,
			Categories: []string{"old"},
			Comment:    "old comment",
		},
	}

	t.Run("patch each new field", func(t *testing.T) {
		body := `{
			"url": "https://new/",
			"priority": "1",
			"categories": ["new1", "new2"],
			"comment": "new comment"
		}`
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/tasks/t1", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var got service.Task
		if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.URL == nil || *got.URL != "https://new/" {
			t.Errorf("url: %+v", got.URL)
		}
		if got.Priority == nil || *got.Priority != "1" {
			t.Errorf("priority: %+v", got.Priority)
		}
		if len(got.Categories) != 2 || got.Categories[0] != "new1" {
			t.Errorf("categories: %v", got.Categories)
		}
		if got.Comment != "new comment" {
			t.Errorf("comment: %q", got.Comment)
		}
	})

	t.Run("clear flags null out columns", func(t *testing.T) {
		body := `{"clear_url": true, "clear_priority": true, "clear_comment": true}`
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/tasks/t1", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var got service.Task
		if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.URL != nil {
			t.Errorf("url should be nil: %+v", got.URL)
		}
		if got.Priority != nil {
			t.Errorf("priority should be nil: %+v", got.Priority)
		}
		if got.Comment != "" {
			t.Errorf("comment should be empty: %q", got.Comment)
		}
	})
}
