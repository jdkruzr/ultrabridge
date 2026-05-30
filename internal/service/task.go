package service

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/sysop/ultrabridge/internal/caldav"
	"github.com/sysop/ultrabridge/internal/taskstore"
)

// TaskStore is the interface required by the TaskService.
// This matches the interface defined in internal/caldav/backend.go.
type TaskStore interface {
	List(ctx context.Context) ([]taskstore.Task, error)
	ListIncludingDeleted(ctx context.Context) ([]taskstore.Task, error)
	Get(ctx context.Context, taskID string) (*taskstore.Task, error)
	Create(ctx context.Context, t *taskstore.Task) error
	Update(ctx context.Context, t *taskstore.Task) error
	Delete(ctx context.Context, taskID string) error
	DeleteCompleted(ctx context.Context) (int64, error)
	HardDeleteOlderThan(ctx context.Context, cutoffMs int64) (purged, skipped int64, err error)
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

// ListIncludingDeleted returns soft-deleted rows alongside the live ones.
// The Deleted field on each Task tells the caller which is which.
func (s *taskService) ListIncludingDeleted(ctx context.Context) ([]Task, error) {
	if s.store == nil {
		return nil, nil
	}
	internalTasks, err := s.store.ListIncludingDeleted(ctx)
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

func (s *taskService) Create(ctx context.Context, input TaskCreate) (Task, error) {
	if s.store == nil {
		return Task{}, fmt.Errorf("task store not available")
	}
	if input.Title == "" {
		return Task{}, fmt.Errorf("title is required")
	}
	now := time.Now().UnixMilli()
	t := &taskstore.Task{
		TaskID:    taskstore.GenerateTaskID(input.Title, now),
		Title:     taskstore.SqlStr(input.Title),
		Status:    taskstore.SqlStr("needsAction"),
		IsDeleted: "N",
	}
	if input.DueAt != nil {
		t.DueTime = input.DueAt.UnixMilli()
	}
	if input.Detail != "" {
		t.Detail = taskstore.SqlStr(input.Detail)
	}
	// URL → tasks.links column (existing). Priority → tasks.importance
	// (existing). Both are structured-column-only on write; the blob is
	// reserved for list-shaped (categories) and free-form (comment) values.
	if input.URL != "" {
		t.Links = taskstore.SqlStr(input.URL)
	}
	if input.Priority != "" {
		t.Importance = taskstore.SqlStr(input.Priority)
	}
	// CATEGORIES + COMMENT have no structured column — stash them in a
	// minimal blob so they survive a get_task immediately after create
	// (without waiting for a CalDAV device round-trip to write a "real" blob).
	if len(input.Categories) > 0 || input.Comment != "" {
		blob := caldav.BuildBlobWithMetadata(t.TaskID, caldav.BlobMetadata{
			Categories: input.Categories,
			Comment:    input.Comment,
		})
		if blob != "" {
			t.ICalBlob = taskstore.SqlStr(blob)
		}
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
	switch {
	case patch.ClearURL:
		t.Links = taskstore.SqlStr("")
	case patch.URL != nil:
		t.Links = taskstore.SqlStr(*patch.URL)
	}
	switch {
	case patch.ClearPriority:
		t.Importance = taskstore.SqlStr("")
	case patch.Priority != nil:
		t.Importance = taskstore.SqlStr(*patch.Priority)
	}

	// Blob-overlaid metadata (CATEGORIES, COMMENT): only touch the blob when
	// the patch actually carries one of these fields. Preserves any other
	// blob-only properties (X-FORESTNOTE-*, etc.) on tasks that arrived via
	// CalDAV PUT.
	//
	// Pass the patch shape straight through — the merge layer needs to
	// distinguish "leave alone" from "set to empty" per-field so a partial
	// blob loss can't silently nuke fields the caller didn't ask to touch
	// (the value-shape BlobMetadata used to lose this distinction at the
	// merge boundary; the patch-shape API restores it).
	if patch.Categories != nil || patch.Comment != nil || patch.ClearComment {
		existing := ""
		if t.ICalBlob.Valid {
			existing = t.ICalBlob.String
		}
		merged := caldav.MergeBlobMetadataPatch(t.TaskID, existing, caldav.BlobMetadataPatch{
			CategoriesPtr: patch.Categories,
			CommentPtr:    patch.Comment,
			ClearComment:  patch.ClearComment,
		})
		if merged == "" {
			t.ICalBlob = sql.NullString{Valid: false}
		} else {
			t.ICalBlob = taskstore.SqlStr(merged)
		}
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

func (s *taskService) PurgeCompleted(ctx context.Context) (int64, error) {
	if s.store == nil {
		return 0, nil
	}
	n, err := s.store.DeleteCompleted(ctx)
	if err != nil {
		return 0, err
	}
	s.notify(ctx)
	return n, nil
}

// PurgeDeleted permanently removes soft-deleted tasks whose last_modified is
// older than olderThanDays days. Returns (purged, skipped, error). Skipped
// counts rows that were soft-deleted but still inside the safety window —
// the existence of this count is what disambiguates "0 purged because the
// gate is doing its job and nothing was eligible" from "0 purged because
// the gate broke and ate everything silently." Unlike PurgeCompleted (which
// soft-deletes completed rows), this is the irreversible end of the
// pipeline — once removed, the row is gone. A non-positive olderThanDays
// is rejected to prevent accidentally wiping every ghost regardless of age.
func (s *taskService) PurgeDeleted(ctx context.Context, olderThanDays int) (purged, skipped int64, err error) {
	if s.store == nil {
		return 0, 0, nil
	}
	if olderThanDays <= 0 {
		return 0, 0, fmt.Errorf("older_than_days must be > 0, got %d", olderThanDays)
	}
	cutoff := time.Now().Add(-time.Duration(olderThanDays) * 24 * time.Hour).UnixMilli()
	purged, skipped, err = s.store.HardDeleteOlderThan(ctx, cutoff)
	if err != nil {
		return 0, 0, err
	}
	// No notify(): hard-purging soft-deleted rows doesn't change what the
	// live device sees (those rows were already tombstoned).
	return purged, skipped, nil
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
		CreatedAt: time.UnixMilli(it.CreatedAt), // taskdb.tasks.created_at (was mis-mapped from DueTime)
		Deleted:   it.IsDeleted == "Y",
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

	// URL (the VTODO URL property) lives in tasks.links — previously never
	// surfaced because the response was leaving Task.URL nil.
	if it.Links.Valid && it.Links.String != "" {
		u := it.Links.String
		t.URL = &u
	}

	// PRIORITY (tasks.importance) — RFC 5545 emits it as a string-coerced
	// integer "1"-"9"; we pass it through verbatim.
	if it.Importance.Valid && it.Importance.String != "" {
		p := it.Importance.String
		t.Priority = &p
	}

	// ForestNote provenance block: nil when none of the structured columns
	// are populated, so non-FN tasks (Apple Reminders, Tasks.org, etc.) drop
	// the field from the JSON entirely via omitempty.
	if it.ForestNoteNotebookID.Valid || it.ForestNotePageID.Valid ||
		it.ForestNoteNotebookName.Valid || it.ForestNoteSource.Valid {
		t.ForestNote = &TaskForestNote{
			NotebookID:   it.ForestNoteNotebookID.String,
			PageID:       it.ForestNotePageID.String,
			NotebookName: it.ForestNoteNotebookName.String,
			Source:       it.ForestNoteSource.String,
		}
	}

	// Categories + native URL + comment live in the blob (no structured column).
	// Parse on-read; ParseBlobMetadata returns zero values on any failure so
	// this path can never crash the response. Blank blob is the common case.
	if it.ICalBlob.Valid && it.ICalBlob.String != "" {
		meta := caldav.ParseBlobMetadata(it.ICalBlob.String)
		if len(meta.Categories) > 0 {
			t.Categories = meta.Categories
		}
		// NativeURL is a sibling of the structured X-FORESTNOTE-* columns —
		// only attach it when those columns confirm this task is in fact
		// FN-originated. A blob-only NativeURL with no column-side provenance
		// would conjure a misleading ForestNote block (just `native_url`,
		// no notebook context), so we drop it on the floor in that case.
		if meta.NativeURL != "" && t.ForestNote != nil {
			t.ForestNote.NativeURL = meta.NativeURL
		}
		if meta.Comment != "" {
			t.Comment = meta.Comment
		}
	}

	return t
}
