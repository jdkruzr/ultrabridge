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
	task, err := svc.Create(context.Background(), title, &due)
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

	created, err := svc.Create(context.Background(), "find me", nil)
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

	task, _ := svc.Create(context.Background(), "Task 1", nil)

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

	t1, _ := svc.Create(context.Background(), "Task 1", nil)
	t2, _ := svc.Create(context.Background(), "Task 2", nil)
	t3, _ := svc.Create(context.Background(), "Task 3", nil)

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

func TestTaskService_Update(t *testing.T) {
	store := &mockTaskStore{tasks: map[string]taskstore.Task{}}
	notifier := &mockNotifier{}
	svc := NewTaskService(store, notifier)

	due := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	created, err := svc.Create(context.Background(), "Draft proposal", &due)
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
