package service

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/taskstore"
)

type mockTaskStore struct {
	tasks map[string]taskstore.Task
}

func (m *mockTaskStore) List(ctx context.Context) ([]taskstore.Task, error) {
	var list []taskstore.Task
	for _, t := range m.tasks {
		if t.IsDeleted == "N" {
			list = append(list, t)
		}
	}
	return list, nil
}

func (m *mockTaskStore) ListIncludingDeleted(ctx context.Context) ([]taskstore.Task, error) {
	var list []taskstore.Task
	for _, t := range m.tasks {
		list = append(list, t)
	}
	return list, nil
}

func (m *mockTaskStore) Get(ctx context.Context, taskID string) (*taskstore.Task, error) {
	t, ok := m.tasks[taskID]
	if !ok || t.IsDeleted == "Y" {
		return nil, sql.ErrNoRows
	}
	return &t, nil
}

func (m *mockTaskStore) Create(ctx context.Context, t *taskstore.Task) error {
	m.tasks[t.TaskID] = *t
	return nil
}

func (m *mockTaskStore) Update(ctx context.Context, t *taskstore.Task) error {
	m.tasks[t.TaskID] = *t
	return nil
}

func (m *mockTaskStore) Delete(ctx context.Context, taskID string) error {
	t, ok := m.tasks[taskID]
	if ok {
		t.IsDeleted = "Y"
		m.tasks[taskID] = t
	}
	return nil
}

func (m *mockTaskStore) DeleteCompleted(ctx context.Context) (int64, error) {
	var count int64
	for id, t := range m.tasks {
		if t.Status.String == "completed" && t.IsDeleted == "N" {
			t.IsDeleted = "Y"
			m.tasks[id] = t
			count++
		}
	}
	return count, nil
}

func (m *mockTaskStore) HardDeleteOlderThan(ctx context.Context, cutoffMs int64) (purged, skipped int64, err error) {
	for id, t := range m.tasks {
		if t.IsDeleted != "Y" || !t.LastModified.Valid {
			continue
		}
		if t.LastModified.Int64 < cutoffMs {
			delete(m.tasks, id)
			purged++
		} else {
			skipped++
		}
	}
	return purged, skipped, nil
}

type mockNotifier struct {
	notified int
}

func (m *mockNotifier) Notify(ctx context.Context) error {
	m.notified++
	return nil
}

func TestTaskService_Create(t *testing.T) {
	store := &mockTaskStore{tasks: make(map[string]taskstore.Task)}
	notifier := &mockNotifier{}
	svc := NewTaskService(store, notifier)

	title := "Test Task"
	due := time.Now().Add(24 * time.Hour)
	task, err := svc.Create(context.Background(), TaskCreate{Title: title, DueAt: &due})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if task.Title != title {
		t.Errorf("expected title %s, got %s", title, task.Title)
	}
	if task.Status != StatusNeedsAction {
		t.Errorf("expected status %s, got %s", StatusNeedsAction, task.Status)
	}
	if notifier.notified != 1 {
		t.Errorf("expected 1 notification, got %d", notifier.notified)
	}
}

func TestTaskService_Get(t *testing.T) {
	store := &mockTaskStore{tasks: make(map[string]taskstore.Task)}
	svc := NewTaskService(store, nil)

	created, err := svc.Create(context.Background(), TaskCreate{Title: "find me"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	got, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get(%q) failed: %v", created.ID, err)
	}
	if got.ID != created.ID || got.Title != "find me" || got.Status != StatusNeedsAction {
		t.Errorf("Get returned %+v, want ID=%s Title=%q Status=%s", got, created.ID, "find me", StatusNeedsAction)
	}

	_, err = svc.Get(context.Background(), "does-not-exist")
	if err == nil {
		t.Fatal("Get(unknown) returned nil error, want sql.ErrNoRows")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Get(unknown) err=%v, want sql.ErrNoRows", err)
	}
}

func TestTaskService_Complete(t *testing.T) {
	store := &mockTaskStore{tasks: make(map[string]taskstore.Task)}
	notifier := &mockNotifier{}
	svc := NewTaskService(store, notifier)

	task, _ := svc.Create(context.Background(), TaskCreate{Title: "Task 1"})

	err := svc.Complete(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}

	updated, _ := svc.List(context.Background())
	if len(updated) != 1 || updated[0].Status != StatusCompleted {
		t.Errorf("expected status completed, got %v", updated[0].Status)
	}
	if updated[0].CompletedAt == nil {
		t.Error("expected CompletedAt to be set")
	}
	if notifier.notified != 2 { // 1 for create, 1 for complete
		t.Errorf("expected 2 notifications, got %d", notifier.notified)
	}
}

func TestTaskService_BulkActions(t *testing.T) {
	store := &mockTaskStore{tasks: make(map[string]taskstore.Task)}
	notifier := &mockNotifier{}
	svc := NewTaskService(store, notifier)

	t1, _ := svc.Create(context.Background(), TaskCreate{Title: "Task 1"})
	t2, _ := svc.Create(context.Background(), TaskCreate{Title: "Task 2"})
	t3, _ := svc.Create(context.Background(), TaskCreate{Title: "Task 3"})

	err := svc.BulkComplete(context.Background(), []string{t1.ID, t2.ID})
	if err != nil {
		t.Fatalf("BulkComplete failed: %v", err)
	}

	list, _ := svc.List(context.Background())
	completedCount := 0
	for _, tk := range list {
		if tk.Status == StatusCompleted {
			completedCount++
		}
	}
	if completedCount != 2 {
		t.Errorf("expected 2 completed tasks, got %d", completedCount)
	}

	err = svc.BulkDelete(context.Background(), []string{t1.ID, t3.ID})
	if err != nil {
		t.Fatalf("BulkDelete failed: %v", err)
	}

	list, _ = svc.List(context.Background())
	if len(list) != 1 {
		t.Errorf("expected 1 task remaining, got %d", len(list))
	}
	if list[0].ID != t2.ID {
		t.Errorf("expected remaining task to be t2, got %s", list[0].ID)
	}
}

func TestTaskService_ListIncludingDeleted(t *testing.T) {
	store := &mockTaskStore{tasks: map[string]taskstore.Task{
		"live": {
			TaskID:    "live",
			Title:     sql.NullString{String: "Live", Valid: true},
			Status:    sql.NullString{String: "needsAction", Valid: true},
			IsDeleted: "N",
		},
		"ghost": {
			TaskID:    "ghost",
			Title:     sql.NullString{String: "Ghost", Valid: true},
			Status:    sql.NullString{String: "completed", Valid: true},
			IsDeleted: "Y",
		},
	}}
	svc := NewTaskService(store, nil)

	got, err := svc.ListIncludingDeleted(context.Background())
	if err != nil {
		t.Fatalf("ListIncludingDeleted: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d tasks, want 2: %+v", len(got), got)
	}
	deleted := map[string]bool{}
	for _, task := range got {
		deleted[task.ID] = task.Deleted
	}
	if deleted["live"] || !deleted["ghost"] {
		t.Fatalf("deleted flags = %+v, want live=false ghost=true", deleted)
	}
}

func TestTaskService_Update(t *testing.T) {
	store := &mockTaskStore{tasks: map[string]taskstore.Task{}}
	notifier := &mockNotifier{}
	svc := NewTaskService(store, notifier)

	due := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	created, err := svc.Create(context.Background(), TaskCreate{Title: "Draft proposal", DueAt: &due})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	t.Run("partial_update_title_only", func(t *testing.T) {
		newTitle := "Draft proposal v2"
		updated, err := svc.Update(context.Background(), created.ID, TaskPatch{Title: &newTitle})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if updated.Title != "Draft proposal v2" {
			t.Errorf("title not applied: %q", updated.Title)
		}
		if updated.DueAt == nil || !updated.DueAt.Equal(due) {
			t.Errorf("due date should be unchanged, got %v", updated.DueAt)
		}
	})

	t.Run("set_detail", func(t *testing.T) {
		detail := "Include Q3 forecast numbers"
		updated, err := svc.Update(context.Background(), created.ID, TaskPatch{Detail: &detail})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if updated.Detail == nil || *updated.Detail != detail {
			t.Errorf("detail not applied: %v", updated.Detail)
		}
	})

	t.Run("clear_due_date", func(t *testing.T) {
		updated, err := svc.Update(context.Background(), created.ID, TaskPatch{ClearDueAt: true})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if updated.DueAt != nil {
			t.Errorf("due date should be nil after ClearDueAt, got %v", updated.DueAt)
		}
	})

	t.Run("clear_wins_over_set", func(t *testing.T) {
		// Re-set, then try to both set and clear in one call — clear should win.
		resetDue := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		_, err := svc.Update(context.Background(), created.ID, TaskPatch{DueAt: &resetDue})
		if err != nil {
			t.Fatalf("reset Update: %v", err)
		}
		newDue := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
		updated, err := svc.Update(context.Background(), created.ID, TaskPatch{
			DueAt:      &newDue,
			ClearDueAt: true,
		})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if updated.DueAt != nil {
			t.Errorf("ClearDueAt should win over DueAt, got %v", updated.DueAt)
		}
	})

	t.Run("empty_title_rejected", func(t *testing.T) {
		empty := ""
		_, err := svc.Update(context.Background(), created.ID, TaskPatch{Title: &empty})
		if err == nil {
			t.Errorf("expected error for empty title; got nil")
		}
	})

	t.Run("missing_task", func(t *testing.T) {
		title := "ghost"
		_, err := svc.Update(context.Background(), "does-not-exist", TaskPatch{Title: &title})
		if err == nil {
			t.Errorf("expected ErrNoRows for missing task; got nil")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("expected sql.ErrNoRows; got %v", err)
		}
	})

	t.Run("notifier_fires_on_update", func(t *testing.T) {
		before := notifier.notified
		title := "notify test"
		_, err := svc.Update(context.Background(), created.ID, TaskPatch{Title: &title})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if notifier.notified <= before {
			t.Errorf("expected notifier to fire; count stuck at %d", notifier.notified)
		}
	})
}

// TestMapInternalTask covers the response-mapping fixes: created_at no longer
// comes from DueTime, URL/Priority/Categories/ForestNote are populated, and
// non-FN tasks omit the ForestNote block entirely.
func TestMapInternalTask(t *testing.T) {
	t.Run("CreatedAt from row column, not DueTime", func(t *testing.T) {
		createdMs := int64(1740000000000) // 2025-02-19
		dueMs := int64(1750000000000)     // 2025-06-15
		in := taskstore.Task{
			TaskID:    "id",
			Title:     sql.NullString{String: "T", Valid: true},
			Status:    sql.NullString{String: "needsAction", Valid: true},
			CreatedAt: createdMs,
			DueTime:   dueMs,
		}
		got := mapInternalTask(in)
		if got.CreatedAt.UnixMilli() != createdMs {
			t.Errorf("CreatedAt: got %d want %d (DueTime mis-mapping regression)",
				got.CreatedAt.UnixMilli(), createdMs)
		}
		if got.DueAt == nil || got.DueAt.UnixMilli() != dueMs {
			t.Errorf("DueAt: got %+v want %d", got.DueAt, dueMs)
		}
	})

	t.Run("URL and Priority surface from row", func(t *testing.T) {
		in := taskstore.Task{
			TaskID:     "id",
			Title:      sql.NullString{String: "T", Valid: true},
			Status:     sql.NullString{String: "needsAction", Valid: true},
			Links:      sql.NullString{String: "https://ub.example/n/abc/p/def", Valid: true},
			Importance: sql.NullString{String: "1", Valid: true},
		}
		got := mapInternalTask(in)
		if got.URL == nil || *got.URL != "https://ub.example/n/abc/p/def" {
			t.Errorf("URL: got %+v want https://...", got.URL)
		}
		if got.Priority == nil || *got.Priority != "1" {
			t.Errorf("Priority: got %+v want 1", got.Priority)
		}
	})

	t.Run("ForestNote block populated when any column is non-NULL", func(t *testing.T) {
		in := taskstore.Task{
			TaskID:                 "id",
			Title:                  sql.NullString{String: "T", Valid: true},
			Status:                 sql.NullString{String: "needsAction", Valid: true},
			ForestNoteNotebookID:   sql.NullString{String: "01HZ3KAY", Valid: true},
			ForestNotePageID:       sql.NullString{String: "01HZ3L7M", Valid: true},
			ForestNoteNotebookName: sql.NullString{String: "Project Notes", Valid: true},
			ForestNoteSource:       sql.NullString{String: "lasso", Valid: true},
		}
		got := mapInternalTask(in)
		if got.ForestNote == nil {
			t.Fatal("ForestNote should be non-nil")
		}
		if got.ForestNote.NotebookID != "01HZ3KAY" {
			t.Errorf("NotebookID: got %q", got.ForestNote.NotebookID)
		}
		if got.ForestNote.Source != "lasso" {
			t.Errorf("Source: got %q", got.ForestNote.Source)
		}
	})

	t.Run("non-FN task omits ForestNote block", func(t *testing.T) {
		in := taskstore.Task{
			TaskID: "id",
			Title:  sql.NullString{String: "From Apple Reminders", Valid: true},
			Status: sql.NullString{String: "needsAction", Valid: true},
		}
		got := mapInternalTask(in)
		if got.ForestNote != nil {
			t.Errorf("ForestNote should be nil for non-FN task: %+v", got.ForestNote)
		}
		if got.URL != nil {
			t.Errorf("URL should be nil: %+v", got.URL)
		}
		if got.Priority != nil {
			t.Errorf("Priority should be nil: %+v", got.Priority)
		}
	})

	t.Run("Categories parsed from blob with no FN columns", func(t *testing.T) {
		blob := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:test\r\n" +
			"BEGIN:VTODO\r\nUID:id\r\nSUMMARY:T\r\n" +
			"CATEGORIES:work,urgent\r\n" +
			"X-FORESTNOTE-NATIVE-URL:forestnote://notebook/abc/page/def\r\n" +
			"END:VTODO\r\nEND:VCALENDAR\r\n"
		in := taskstore.Task{
			TaskID:   "id",
			Title:    sql.NullString{String: "T", Valid: true},
			Status:   sql.NullString{String: "needsAction", Valid: true},
			ICalBlob: sql.NullString{String: blob, Valid: true},
		}
		got := mapInternalTask(in)
		if len(got.Categories) != 2 || got.Categories[0] != "work" || got.Categories[1] != "urgent" {
			t.Errorf("Categories: got %v want [work urgent]", got.Categories)
		}
		// NativeURL came from the blob, but no structured X-FORESTNOTE-*
		// column was set — per the Important-1 review fix, mapInternalTask
		// drops the NativeURL on the floor in this case rather than conjure
		// a misleading ForestNote provenance block carrying only `native_url`
		// with no notebook context. (A blob with NativeURL alone almost
		// certainly means an old build or a non-FN client copying the prop.)
		if got.ForestNote != nil {
			t.Errorf("blob-only NativeURL must NOT create a ForestNote block; got %+v", got.ForestNote)
		}
	})

	t.Run("NativeURL attaches to existing ForestNote block from columns", func(t *testing.T) {
		blob := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:test\r\n" +
			"BEGIN:VTODO\r\nUID:id\r\nSUMMARY:T\r\n" +
			"X-FORESTNOTE-NATIVE-URL:forestnote://notebook/abc/page/def\r\n" +
			"END:VTODO\r\nEND:VCALENDAR\r\n"
		in := taskstore.Task{
			TaskID:               "id",
			Title:                sql.NullString{String: "T", Valid: true},
			Status:               sql.NullString{String: "needsAction", Valid: true},
			ForestNoteNotebookID: sql.NullString{String: "01HZ3KAY", Valid: true},
			ICalBlob:             sql.NullString{String: blob, Valid: true},
		}
		got := mapInternalTask(in)
		if got.ForestNote == nil {
			t.Fatal("ForestNote block should exist (NotebookID column was set)")
		}
		if got.ForestNote.NativeURL != "forestnote://notebook/abc/page/def" {
			t.Errorf("NativeURL should be attached to the existing block; got %q", got.ForestNote.NativeURL)
		}
		if got.ForestNote.NotebookID != "01HZ3KAY" {
			t.Errorf("NotebookID round-trip: got %q", got.ForestNote.NotebookID)
		}
	})

	t.Run("corrupt blob doesn't crash mapping", func(t *testing.T) {
		in := taskstore.Task{
			TaskID:   "id",
			Title:    sql.NullString{String: "T", Valid: true},
			Status:   sql.NullString{String: "needsAction", Valid: true},
			ICalBlob: sql.NullString{String: "not actually iCalendar at all", Valid: true},
		}
		got := mapInternalTask(in)
		if len(got.Categories) != 0 {
			t.Errorf("Categories should be empty: %v", got.Categories)
		}
		if got.ForestNote != nil {
			t.Errorf("ForestNote should be nil from a corrupt blob: %+v", got.ForestNote)
		}
	})

	t.Run("ATTACH surfaced from blob (URI + inline-binary metadata)", func(t *testing.T) {
		// base64("hi") = "aGk=" (2 decoded bytes).
		blob := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:test\r\n" +
			"BEGIN:VTODO\r\nUID:id\r\nSUMMARY:T\r\n" +
			"ATTACH;FMTTYPE=application/pdf;FILENAME=doc.pdf:https://example.com/doc.pdf\r\n" +
			"ATTACH;FMTTYPE=text/plain;ENCODING=BASE64;VALUE=BINARY:aGk=\r\n" +
			"END:VTODO\r\nEND:VCALENDAR\r\n"
		in := taskstore.Task{
			TaskID:   "id",
			Title:    sql.NullString{String: "T", Valid: true},
			Status:   sql.NullString{String: "needsAction", Valid: true},
			ICalBlob: sql.NullString{String: blob, Valid: true},
		}
		got := mapInternalTask(in)
		if len(got.Attachments) != 2 {
			t.Fatalf("want 2 attachments, got %d (%+v)", len(got.Attachments), got.Attachments)
		}
		if got.Attachments[0].URL != "https://example.com/doc.pdf" || got.Attachments[0].Inline {
			t.Errorf("URI attachment wrong: %+v", got.Attachments[0])
		}
		if !got.Attachments[1].Inline || got.Attachments[1].URL != "" || got.Attachments[1].Size != 2 {
			t.Errorf("inline attachment should expose metadata-only: %+v", got.Attachments[1])
		}
	})
}

// TestTaskService_PurgeDeleted covers the new hard-purge path: positive days
// translates into a cutoff that the store sees, non-positive days is rejected
// at the service boundary, and a nil store is a safe no-op.
func TestTaskService_PurgeDeleted(t *testing.T) {
	ctx := context.Background()
	dayMs := int64(24 * 60 * 60 * 1000)

	t.Run("positive days purges matching rows", func(t *testing.T) {
		now := time.Now().UnixMilli()
		store := &mockTaskStore{tasks: map[string]taskstore.Task{
			"ancient-ghost": {
				TaskID:       "ancient-ghost",
				IsDeleted:    "Y",
				LastModified: sql.NullInt64{Int64: now - 90*dayMs, Valid: true},
			},
			"recent-ghost": {
				TaskID:       "recent-ghost",
				IsDeleted:    "Y",
				LastModified: sql.NullInt64{Int64: now - 1*dayMs, Valid: true},
			},
			"live": {
				TaskID:       "live",
				IsDeleted:    "N",
				LastModified: sql.NullInt64{Int64: now - 90*dayMs, Valid: true},
			},
		}}
		svc := &taskService{store: store}

		purged, skipped, err := svc.PurgeDeleted(ctx, 30)
		if err != nil {
			t.Fatalf("PurgeDeleted: %v", err)
		}
		if purged != 1 {
			t.Errorf("purged: got %d, want 1", purged)
		}
		if skipped != 1 {
			t.Errorf("skipped: got %d, want 1 (the recent-ghost row)", skipped)
		}
		if _, present := store.tasks["ancient-ghost"]; present {
			t.Error("ancient-ghost should have been hard-deleted")
		}
		if _, present := store.tasks["recent-ghost"]; !present {
			t.Error("recent-ghost should still be present (inside safety window)")
		}
		if _, present := store.tasks["live"]; !present {
			t.Error("live row should be untouched by hard-purge")
		}
	})

	t.Run("zero or negative days is rejected", func(t *testing.T) {
		svc := &taskService{store: &mockTaskStore{tasks: map[string]taskstore.Task{}}}
		for _, days := range []int{0, -1, -30} {
			if _, _, err := svc.PurgeDeleted(ctx, days); err == nil {
				t.Errorf("PurgeDeleted(%d) should return error, got nil", days)
			}
		}
	})

	t.Run("nil store is a no-op", func(t *testing.T) {
		svc := &taskService{store: nil}
		purged, skipped, err := svc.PurgeDeleted(ctx, 30)
		if err != nil {
			t.Errorf("nil-store PurgeDeleted should not error: %v", err)
		}
		if purged != 0 || skipped != 0 {
			t.Errorf("nil-store counts: got purged=%d skipped=%d, want 0/0", purged, skipped)
		}
	})

	t.Run("does not notify (ghosts are already tombstoned)", func(t *testing.T) {
		notifier := &mockNotifier{}
		svc := &taskService{
			store:    &mockTaskStore{tasks: map[string]taskstore.Task{}},
			notifier: notifier,
		}
		if _, _, err := svc.PurgeDeleted(ctx, 30); err != nil {
			t.Fatalf("PurgeDeleted: %v", err)
		}
		if notifier.notified != 0 {
			t.Errorf("PurgeDeleted should not notify; notify count: %d", notifier.notified)
		}
	})

	t.Run("propagates store error", func(t *testing.T) {
		base := &mockTaskStore{tasks: map[string]taskstore.Task{}}
		svc := &taskService{store: &errStore{TaskStore: base, err: errors.New("disk full")}}
		if _, _, err := svc.PurgeDeleted(ctx, 30); err == nil || err.Error() != "disk full" {
			t.Errorf("expected disk-full error, got %v", err)
		}
	})
}

// errStore wraps a TaskStore and forces HardDeleteOlderThan to fail; used by
// the PurgeDeleted error-propagation subtest.
type errStore struct {
	TaskStore
	err error
}

func (e *errStore) HardDeleteOlderThan(ctx context.Context, cutoffMs int64) (purged, skipped int64, err error) {
	return 0, 0, e.err
}

// TestTaskService_Create_WithMetadata covers the extended write surface:
// URL + Priority land in structured columns; Categories + Comment land in
// a minimal blob that ParseBlobMetadata can read back immediately (without
// waiting for a CalDAV round-trip).
func TestTaskService_Create_WithMetadata(t *testing.T) {
	ctx := context.Background()

	t.Run("structured fields populate column-backed Task fields", func(t *testing.T) {
		store := &mockTaskStore{tasks: make(map[string]taskstore.Task)}
		svc := NewTaskService(store, nil)

		task, err := svc.Create(ctx, TaskCreate{
			Title:    "ship feature",
			Detail:   "context body",
			URL:      "https://ub.example/task/abc",
			Priority: "1",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if task.Detail == nil || *task.Detail != "context body" {
			t.Errorf("Detail: %+v", task.Detail)
		}
		if task.URL == nil || *task.URL != "https://ub.example/task/abc" {
			t.Errorf("URL: %+v", task.URL)
		}
		if task.Priority == nil || *task.Priority != "1" {
			t.Errorf("Priority: %+v", task.Priority)
		}
	})

	t.Run("categories + comment round-trip via blob immediately", func(t *testing.T) {
		store := &mockTaskStore{tasks: make(map[string]taskstore.Task)}
		svc := NewTaskService(store, nil)

		task, err := svc.Create(ctx, TaskCreate{
			Title:      "review notes",
			Categories: []string{"work", "urgent"},
			Comment:    "Recognized text: meeting outcomes",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if len(task.Categories) != 2 || task.Categories[0] != "work" || task.Categories[1] != "urgent" {
			t.Errorf("Categories round-trip: %v", task.Categories)
		}
		if task.Comment != "Recognized text: meeting outcomes" {
			t.Errorf("Comment round-trip: %q", task.Comment)
		}
	})

	t.Run("empty title is rejected", func(t *testing.T) {
		svc := NewTaskService(&mockTaskStore{tasks: make(map[string]taskstore.Task)}, nil)
		if _, err := svc.Create(ctx, TaskCreate{Title: ""}); err == nil {
			t.Error("Create with empty title should error")
		}
	})

	t.Run("no metadata means no blob is constructed", func(t *testing.T) {
		store := &mockTaskStore{tasks: make(map[string]taskstore.Task)}
		svc := NewTaskService(store, nil)

		task, err := svc.Create(ctx, TaskCreate{Title: "minimal"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Find the persisted taskstore.Task to verify blob is unset.
		var stored taskstore.Task
		for _, t := range store.tasks {
			if t.TaskID == task.ID {
				stored = t
				break
			}
		}
		if stored.ICalBlob.Valid && stored.ICalBlob.String != "" {
			t.Errorf("expected empty blob, got %q", stored.ICalBlob.String)
		}
	})
}

// TestTaskService_Update_NewFields verifies the extended TaskPatch surface
// patches URL/Priority columns and overlays Categories/Comment into the blob
// without clobbering pre-existing blob properties.
func TestTaskService_Update_NewFields(t *testing.T) {
	ctx := context.Background()

	t.Run("URL and Priority patches reach the column", func(t *testing.T) {
		store := &mockTaskStore{tasks: make(map[string]taskstore.Task)}
		svc := NewTaskService(store, nil)
		created, _ := svc.Create(ctx, TaskCreate{Title: "t"})

		url := "https://ub.example/x"
		prio := "1"
		updated, err := svc.Update(ctx, created.ID, TaskPatch{
			URL:      &url,
			Priority: &prio,
		})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if updated.URL == nil || *updated.URL != url {
			t.Errorf("URL: %+v", updated.URL)
		}
		if updated.Priority == nil || *updated.Priority != prio {
			t.Errorf("Priority: %+v", updated.Priority)
		}
	})

	t.Run("ClearURL and ClearPriority null out the columns", func(t *testing.T) {
		store := &mockTaskStore{tasks: make(map[string]taskstore.Task)}
		svc := NewTaskService(store, nil)
		created, _ := svc.Create(ctx, TaskCreate{
			Title:    "t",
			URL:      "https://x/",
			Priority: "1",
		})
		updated, err := svc.Update(ctx, created.ID, TaskPatch{
			ClearURL:      true,
			ClearPriority: true,
		})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if updated.URL != nil {
			t.Errorf("URL should be cleared: %+v", updated.URL)
		}
		if updated.Priority != nil {
			t.Errorf("Priority should be cleared: %+v", updated.Priority)
		}
	})

	t.Run("Categories patch replaces wholesale", func(t *testing.T) {
		store := &mockTaskStore{tasks: make(map[string]taskstore.Task)}
		svc := NewTaskService(store, nil)
		created, _ := svc.Create(ctx, TaskCreate{
			Title:      "t",
			Categories: []string{"old"},
		})

		newCats := []string{"new1", "new2"}
		updated, err := svc.Update(ctx, created.ID, TaskPatch{Categories: &newCats})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if len(updated.Categories) != 2 || updated.Categories[0] != "new1" {
			t.Errorf("Categories: %v", updated.Categories)
		}
	})

	t.Run("Comment patch + ClearComment", func(t *testing.T) {
		store := &mockTaskStore{tasks: make(map[string]taskstore.Task)}
		svc := NewTaskService(store, nil)
		created, _ := svc.Create(ctx, TaskCreate{Title: "t", Comment: "first"})

		updatedTxt := "second"
		updated, _ := svc.Update(ctx, created.ID, TaskPatch{Comment: &updatedTxt})
		if updated.Comment != "second" {
			t.Errorf("Comment: %q", updated.Comment)
		}

		cleared, _ := svc.Update(ctx, created.ID, TaskPatch{ClearComment: true})
		if cleared.Comment != "" {
			t.Errorf("ClearComment failed: %q", cleared.Comment)
		}
	})
}
