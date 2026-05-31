package caldav

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"strings"
	"sync"

	ical "github.com/emersion/go-ical"
	gocaldav "github.com/emersion/go-webdav/caldav"
	"github.com/sysop/ultrabridge/internal/taskattach"
	"github.com/sysop/ultrabridge/internal/taskstore"
)

// TaskStore defines the task persistence operations needed by CalDAV and Web handlers.
// *taskstore.Store satisfies this interface.
type TaskStore interface {
	List(ctx context.Context) ([]taskstore.Task, error)
	Get(ctx context.Context, taskID string) (*taskstore.Task, error)
	Create(ctx context.Context, t *taskstore.Task) error
	Update(ctx context.Context, t *taskstore.Task) error
	Delete(ctx context.Context, taskID string) error
	DeleteCompleted(ctx context.Context) (int64, error)
	MaxLastModified(ctx context.Context) (int64, error)
}

// SyncNotifier triggers device sync after task writes.
type SyncNotifier interface {
	Notify(ctx context.Context) error
}

type Backend struct {
	store       TaskStore
	notifier    SyncNotifier
	prefix      string
	dueTimeMode string

	// collectionName is mutable at runtime via SetCollectionName so a client
	// PROPPATCH that sets DAV:displayname takes effect immediately on
	// subsequent PROPFIND responses, without requiring a container restart.
	nameMu         sync.RWMutex
	collectionName string

	// attach (optional; zero value disables) de-bloats inline-binary ATTACH on
	// inbound PUT and reconstructs it on outbound GET. Wired from main.go via
	// SetTaskAttach.
	attach AttachDeps
}

// SetTaskAttach wires the inline-binary ATTACH side-store + signer + base URL.
// Set from main.go after construction; a zero value (never set) leaves inline
// binary in the blob as before.
func (b *Backend) SetTaskAttach(store *taskattach.BlobStore, signer *taskattach.Signer, baseURL string) {
	b.attach = AttachDeps{Store: store, Signer: signer, BaseURL: strings.TrimRight(baseURL, "/")}
}

func NewBackend(store TaskStore, prefix, collectionName, dueTimeMode string, notifier SyncNotifier) *Backend {
	return &Backend{
		store:          store,
		notifier:       notifier,
		prefix:         strings.TrimSuffix(prefix, "/"),
		collectionName: collectionName,
		dueTimeMode:    dueTimeMode,
	}
}

// CollectionName returns the current display name of the task collection.
func (b *Backend) CollectionName() string {
	b.nameMu.RLock()
	defer b.nameMu.RUnlock()
	return b.collectionName
}

// SetCollectionName updates the display name at runtime. Callers should
// also persist the new name (e.g. to the settings DB) so it survives
// restarts; this setter only affects the running backend.
func (b *Backend) SetCollectionName(name string) {
	b.nameMu.Lock()
	defer b.nameMu.Unlock()
	b.collectionName = name
}

func (b *Backend) CurrentUserPrincipal(ctx context.Context) (string, error) {
	return b.prefix + "/user/", nil
}

func (b *Backend) CalendarHomeSetPath(ctx context.Context) (string, error) {
	return b.prefix + "/user/calendars/", nil
}

func (b *Backend) CreateCalendar(ctx context.Context, calendar *gocaldav.Calendar) error {
	return fmt.Errorf("calendar creation not supported")
}

func (b *Backend) ListCalendars(ctx context.Context) ([]gocaldav.Calendar, error) {
	return []gocaldav.Calendar{b.collection()}, nil
}

func (b *Backend) GetCalendar(ctx context.Context, urlPath string) (*gocaldav.Calendar, error) {
	col := b.collection()
	if path.Clean(urlPath) != path.Clean(col.Path) {
		return nil, fmt.Errorf("calendar not found")
	}
	return &col, nil
}

func (b *Backend) GetCalendarObject(ctx context.Context, urlPath string, req *gocaldav.CalendarCompRequest) (*gocaldav.CalendarObject, error) {
	taskID := b.taskIDFromPath(urlPath)
	if taskID == "" {
		return nil, fmt.Errorf("invalid path")
	}
	task, err := b.store.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return b.taskToCalendarObject(task), nil
}

func (b *Backend) ListCalendarObjects(ctx context.Context, urlPath string, req *gocaldav.CalendarCompRequest) ([]gocaldav.CalendarObject, error) {
	tasks, err := b.store.List(ctx)
	if err != nil {
		return nil, err
	}
	objects := make([]gocaldav.CalendarObject, len(tasks))
	for i := range tasks {
		objects[i] = *b.taskToCalendarObject(&tasks[i])
	}
	return objects, nil
}

func (b *Backend) QueryCalendarObjects(ctx context.Context, urlPath string, query *gocaldav.CalendarQuery) ([]gocaldav.CalendarObject, error) {
	// List all tasks, then apply the query filter.
	// The go-webdav library does NOT filter results after QueryCalendarObjects returns —
	// it expects us to apply the filter. Use gocaldav.Filter() if available,
	// or apply CompFilter manually. For a small VTODO collection this is acceptable.
	tasks, err := b.store.List(ctx)
	if err != nil {
		return nil, err
	}
	var objects []gocaldav.CalendarObject
	for i := range tasks {
		obj := b.taskToCalendarObject(&tasks[i])
		// Apply filter: check if the object matches the query's CompFilter.
		// At minimum, verify the component type matches (VTODO).
		// The implementor should check if go-webdav exports a Filter() or Match()
		// helper and use it here. If not, a simple component name check suffices
		// since all our objects are VTODO.
		if query != nil && query.CompFilter.Name != "" &&
			query.CompFilter.Name != "VCALENDAR" {
			// If filter requests a specific component (e.g., VTODO), check it
			hasMatch := false
			for _, child := range obj.Data.Children {
				if child.Name == query.CompFilter.Name {
					hasMatch = true
					break
				}
			}
			if !hasMatch {
				continue
			}
		}
		objects = append(objects, *obj)
	}
	return objects, nil
}

func (b *Backend) PutCalendarObject(ctx context.Context, urlPath string, cal *ical.Calendar, opts *gocaldav.PutCalendarObjectOptions) (*gocaldav.CalendarObject, error) {
	// Reject VEVENTs — this collection only supports VTODO.
	// The implementor should check go-webdav's exported error types for the
	// "supported-calendar-component" precondition violation. If the library
	// provides a specific precondition error type, use it. Otherwise, return
	// a plain error — go-webdav will map it to an appropriate HTTP status.
	if HasVEvent(cal) {
		return nil, fmt.Errorf("only VTODO components are supported, not VEVENT")
	}

	task, err := VTODOToTask(cal, b.dueTimeMode)
	if err != nil {
		return nil, err
	}

	// De-bloat any inline-binary ATTACH out of the blob into the content store
	// before persisting (no-op when unwired or when there are none). ETag is
	// unaffected (ComputeETag ignores the blob), so this never re-syncs.
	if b.attach.enabled() && task.ICalBlob.Valid && task.ICalBlob.String != "" {
		task.ICalBlob.String = DebloatInlineAttachments(task.ICalBlob.String, b.attach)
	}

	taskID := b.taskIDFromPath(urlPath)

	// Check if task exists
	existing, getErr := b.store.Get(ctx, taskID)
	if getErr != nil {
		// Check if it's a "not found" error or a real error
		if !taskstore.IsNotFound(getErr) {
			// Real error, not just missing task
			return nil, getErr
		}
		// Task doesn't exist, create new one
		if task.TaskID == "" {
			task.TaskID = taskID
		}
		if err := b.store.Create(ctx, task); err != nil {
			return nil, err
		}
	} else {
		// Update existing — carry over fields the VTODO doesn't set
		task.TaskID = existing.TaskID
		if !task.CompletedTime.Valid {
			task.CompletedTime = existing.CompletedTime
		}
		if task.IsReminderOn == "" {
			task.IsReminderOn = existing.IsReminderOn
		}
		if task.IsDeleted == "" {
			task.IsDeleted = existing.IsDeleted
		}
		if task.Recurrence == (sql.NullString{}) && existing.Recurrence.Valid {
			task.Recurrence = existing.Recurrence
		}
		if task.Links == (sql.NullString{}) && existing.Links.Valid {
			task.Links = existing.Links
		}
		if err := b.store.Update(ctx, task); err != nil {
			return nil, err
		}
	}

	// Notify device of sync after successful store operation
	if b.notifier != nil {
		if err := b.notifier.Notify(ctx); err != nil {
			// Log warning but don't fail the operation (graceful degradation)
			// Logging will be wired in Phase 6
		}
	}

	// Re-fetch to get updated fields
	updated, err := b.store.Get(ctx, task.TaskID)
	if err != nil {
		return nil, err
	}
	return b.taskToCalendarObject(updated), nil
}

func (b *Backend) DeleteCalendarObject(ctx context.Context, urlPath string) error {
	taskID := b.taskIDFromPath(urlPath)
	if taskID == "" {
		return fmt.Errorf("invalid path")
	}
	if err := b.store.Delete(ctx, taskID); err != nil {
		return err
	}

	// Notify device of sync after successful store operation
	if b.notifier != nil {
		if err := b.notifier.Notify(ctx); err != nil {
			// Log warning but don't fail the operation (graceful degradation)
			// Logging will be wired in Phase 6
		}
	}

	return nil
}

func (b *Backend) collection() gocaldav.Calendar {
	return gocaldav.Calendar{
		Path:                  b.prefix + "/user/calendars/tasks/",
		Name:                  b.CollectionName(),
		Description:           "Tasks via UltraBridge",
		SupportedComponentSet: []string{"VTODO"},
	}
}

func (b *Backend) taskToCalendarObject(t *taskstore.Task) *gocaldav.CalendarObject {
	cal := TaskToVTODO(t, b.dueTimeMode)
	// Restore any de-bloated inline-binary ATTACH from the content store so the
	// client receives byte-equivalent content (no-op when unwired / none).
	if b.attach.Store != nil {
		ReconstructInlineAttachments(cal, b.attach.Store)
	}
	// Synthesize an ATTACH to the source FN page render for ForestNote tasks so
	// third-party clients can open the handwriting page (serve-time only, never
	// stored). No-op without a signer + base URL or for non-FN tasks.
	AddFNRenderAttach(cal, t, b.attach)
	return &gocaldav.CalendarObject{
		Path:    b.prefix + "/user/calendars/tasks/" + t.TaskID + ".ics",
		ModTime: taskstore.MsToTime(t.LastModified.Int64),
		ETag:    taskstore.ComputeETag(t),
		Data:    cal,
	}
}

// taskIDFromPath extracts the task ID from a path like
// /caldav/user/calendars/tasks/{id}.ics. Returns empty string for any path
// that isn't a .ics file — this prevents collection URLs (/caldav/,
// /caldav/user/calendars/tasks/) or unrelated probes from being treated as
// task IDs and then surfacing a misleading "task not found" 404 to
// browser-style GET probes (observed with Vivaldi's calendar add flow).
func (b *Backend) taskIDFromPath(urlPath string) string {
	if !strings.HasSuffix(urlPath, ".ics") {
		return ""
	}
	base := path.Base(urlPath)
	return strings.TrimSuffix(base, ".ics")
}
