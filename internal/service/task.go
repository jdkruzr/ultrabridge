package service

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/sysop/ultrabridge/internal/taskstore"
)

// TaskStore is the interface required by the TaskService.
// This matches the interface defined in internal/caldav/backend.go.
type TaskStore interface {
	List(ctx context.Context) ([]taskstore.Task, error)
	Get(ctx context.Context, taskID string) (*taskstore.Task, error)
	Create(ctx context.Context, t *taskstore.Task) error
	Update(ctx context.Context, t *taskstore.Task) error
	Delete(ctx context.Context, taskID string) error
	DeleteCompleted(ctx context.Context) (int64, error)
}

// SyncNotifier is the interface for triggering device sync.
type SyncNotifier interface {
	Notify(ctx context.Context) error
}

type taskService struct {
	store    TaskStore
	notifier SyncNotifier
}

// NewTaskService creates a new TaskService.
func NewTaskService(store TaskStore, notifier SyncNotifier) TaskService {
	return &taskService{
		store:    store,
		notifier: notifier,
	}
}

func (s *taskService) List(ctx context.Context) ([]Task, error) {
	if s.store == nil {
		return nil, nil
	}
	internalTasks, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}

	tasks := make([]Task, len(internalTasks))
	for i, it := range internalTasks {
		tasks[i] = mapInternalTask(it)
	}
	return tasks, nil
}

func (s *taskService) Get(ctx context.Context, id string) (Task, error) {
	if s.store == nil {
		return Task{}, fmt.Errorf("task store not available")
	}
	t, err := s.store.Get(ctx, id)
	if err != nil {
		return Task{}, err
	}
	return mapInternalTask(*t), nil
}

func (s *taskService) Create(ctx context.Context, title string, dueAt *time.Time) (Task, error) {
	if s.store == nil {
		return Task{}, fmt.Errorf("task store not available")
	}
	now := time.Now().UnixMilli()
	t := &taskstore.Task{
		TaskID:    taskstore.GenerateTaskID(title, now),
		Title:     taskstore.SqlStr(title),
		Status:    taskstore.SqlStr("needsAction"),
		IsDeleted: "N",
	}
	if dueAt != nil {
		t.DueTime = dueAt.UnixMilli()
	}

	if err := s.store.Create(ctx, t); err != nil {
		return Task{}, err
	}

	s.notify(ctx)
	return mapInternalTask(*t), nil
}

// Update applies a partial patch to an existing task. Empty-string title is
// rejected to avoid producing invalid VTODOs on sync. Returns the updated
// task in its post-write shape.
func (s *taskService) Update(ctx context.Context, id string, patch TaskPatch) (Task, error) {
	if s.store == nil {
		return Task{}, fmt.Errorf("task store not available")
	}
	t, err := s.store.Get(ctx, id)
	if err != nil {
		return Task{}, err
	}

	if patch.Title != nil {
		title := *patch.Title
		if title == "" {
			return Task{}, fmt.Errorf("title cannot be empty")
		}
		t.Title = taskstore.SqlStr(title)
	}
	switch {
	case patch.ClearDueAt:
		t.DueTime = 0
	case patch.DueAt != nil:
		t.DueTime = patch.DueAt.UnixMilli()
	}
	if patch.Detail != nil {
		t.Detail = taskstore.SqlStr(*patch.Detail)
	}

	if err := s.store.Update(ctx, t); err != nil {
		return Task{}, err
	}
	s.notify(ctx)
	return mapInternalTask(*t), nil
}

func (s *taskService) Complete(ctx context.Context, id string) error {
	if s.store == nil {
		return fmt.Errorf("task store not available")
	}
	task, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}

	task.Status = taskstore.SqlStr("completed")
	if !task.CompletedTime.Valid {
		task.CompletedTime = sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true}
	}

	if err := s.store.Update(ctx, task); err != nil {
		return err
	}

	s.notify(ctx)
	return nil
}

func (s *taskService) Delete(ctx context.Context, id string) error {
	if s.store == nil {
		return fmt.Errorf("task store not available")
	}
	if err := s.store.Delete(ctx, id); err != nil {
		return err
	}
	s.notify(ctx)
	return nil
}

func (s *taskService) PurgeCompleted(ctx context.Context) error {
	if s.store == nil {
		return nil
	}
	_, err := s.store.DeleteCompleted(ctx)
	if err != nil {
		return err
	}
	s.notify(ctx)
	return nil
}

func (s *taskService) BulkComplete(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if err := s.Complete(ctx, id); err != nil {
			return fmt.Errorf("bulk complete failed at id %s: %w", id, err)
		}
	}
	return nil
}

func (s *taskService) BulkDelete(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if err := s.Delete(ctx, id); err != nil {
			return fmt.Errorf("bulk delete failed at id %s: %w", id, err)
		}
	}
	return nil
}

func (s *taskService) notify(ctx context.Context) {
	if s.notifier != nil {
		_ = s.notifier.Notify(ctx)
	}
}

func mapInternalTask(it taskstore.Task) Task {
	t := Task{
		ID:        it.TaskID,
		Title:     it.Title.String,
		Status:    TaskStatus(it.Status.String),
		CreatedAt: time.UnixMilli(it.DueTime), // This mapping might need verification
	}

	if it.DueTime > 0 {
		dt := time.UnixMilli(it.DueTime)
		t.DueAt = &dt
	}

	if it.CompletedTime.Valid && it.CompletedTime.Int64 > 0 {
		ct := time.UnixMilli(it.CompletedTime.Int64)
		t.CompletedAt = &ct
	}

	if it.Detail.Valid {
		t.Detail = &it.Detail.String
	}

	return t
}
