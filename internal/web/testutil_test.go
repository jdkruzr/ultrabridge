package web

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strings"

	"github.com/sysop/ultrabridge/internal/logging"
	"github.com/sysop/ultrabridge/internal/service"
	"github.com/sysop/ultrabridge/internal/syncstore"
)

// newTestHandler creates a Handler with default mocks for testing.
func newTestHandler() *Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()

	tasks := &mockTaskService{}
	notes := &mockNoteService{
		docs:               make(map[string][]service.SearchResult),
		contents:           make(map[string]interface{}),
		pipelineConfigured: true,
		booxEnabled:        true,
	}
	search := &mockSearchService{
		embeddingPipelineConfigured: true,
		chatEnabled:                 true,
	}
	config := &mockConfigService{}

	return NewHandler(tasks, notes, search, config, nil, "", "", logger, broadcaster)
}

// mockTaskService implements TaskService for testing
type mockTaskService struct {
	tasks          []service.Task
	purgeDeletedFn func(ctx context.Context, olderThanDays int) (purged, skipped int64, err error)
}

func (m *mockTaskService) List(ctx context.Context) ([]service.Task, error) {
	out := m.tasks[:0:0]
	for _, t := range m.tasks {
		if !t.Deleted {
			out = append(out, t)
		}
	}
	return out, nil
}
func (m *mockTaskService) ListIncludingDeleted(ctx context.Context) ([]service.Task, error) {
	return m.tasks, nil
}
func (m *mockTaskService) Get(ctx context.Context, id string) (service.Task, error) {
	for _, t := range m.tasks {
		if t.ID == id {
			return t, nil
		}
	}
	return service.Task{}, sql.ErrNoRows
}
func (m *mockTaskService) Create(ctx context.Context, input service.TaskCreate) (service.Task, error) {
	t := service.Task{
		ID:     "test-id",
		Title:  input.Title,
		Status: service.StatusNeedsAction,
	}
	if input.DueAt != nil {
		t.DueAt = input.DueAt
	}
	if input.Detail != "" {
		d := input.Detail
		t.Detail = &d
	}
	if input.URL != "" {
		u := input.URL
		t.URL = &u
	}
	if input.Priority != "" {
		p := input.Priority
		t.Priority = &p
	}
	if len(input.Categories) > 0 {
		t.Categories = input.Categories
	}
	if input.Comment != "" {
		t.Comment = input.Comment
	}
	m.tasks = append(m.tasks, t)
	return t, nil
}
func (m *mockTaskService) Update(ctx context.Context, id string, patch service.TaskPatch) (service.Task, error) {
	for i := range m.tasks {
		if m.tasks[i].ID == id {
			if patch.Title != nil {
				m.tasks[i].Title = *patch.Title
			}
			if patch.ClearDueAt {
				m.tasks[i].DueAt = nil
			} else if patch.DueAt != nil {
				m.tasks[i].DueAt = patch.DueAt
			}
			if patch.Detail != nil {
				m.tasks[i].Detail = patch.Detail
			}
			switch {
			case patch.ClearURL:
				m.tasks[i].URL = nil
			case patch.URL != nil:
				m.tasks[i].URL = patch.URL
			}
			switch {
			case patch.ClearPriority:
				m.tasks[i].Priority = nil
			case patch.Priority != nil:
				m.tasks[i].Priority = patch.Priority
			}
			if patch.Categories != nil {
				m.tasks[i].Categories = *patch.Categories
			}
			switch {
			case patch.ClearComment:
				m.tasks[i].Comment = ""
			case patch.Comment != nil:
				m.tasks[i].Comment = *patch.Comment
			}
			return m.tasks[i], nil
		}
	}
	return service.Task{}, sql.ErrNoRows
}
func (m *mockTaskService) Complete(ctx context.Context, id string) error { return nil }
func (m *mockTaskService) Delete(ctx context.Context, id string) error   { return nil }
func (m *mockTaskService) PurgeCompleted(ctx context.Context) (int64, error) {
	var active []service.Task
	var purged int64
	for _, t := range m.tasks {
		if t.Status != service.StatusCompleted {
			active = append(active, t)
		} else {
			purged++
		}
	}
	m.tasks = active
	return purged, nil
}
func (m *mockTaskService) PurgeDeleted(ctx context.Context, olderThanDays int) (purged, skipped int64, err error) {
	if olderThanDays <= 0 {
		return 0, 0, nil
	}
	// Mock doesn't model last_modified; the handler tests provide a stubbed
	// override via purgeDeletedFn when they need to observe the call shape.
	if m.purgeDeletedFn != nil {
		return m.purgeDeletedFn(ctx, olderThanDays)
	}
	var kept []service.Task
	for _, t := range m.tasks {
		if t.Deleted {
			purged++
			continue
		}
		kept = append(kept, t)
	}
	m.tasks = kept
	return purged, 0, nil
}
func (m *mockTaskService) BulkComplete(ctx context.Context, ids []string) error { return nil }
func (m *mockTaskService) BulkDelete(ctx context.Context, ids []string) error   { return nil }

// mockNoteService implements NoteService for testing
type mockNoteService struct {
	files     []service.NoteFile
	docs      map[string][]service.SearchResult
	contents  map[string]interface{}
	notePages map[string][]service.NotePageView
	renders   map[string]io.ReadCloser

	processorStarted     bool
	booxProcessorStarted bool
	importTriggered      bool
	migrateTriggered     bool
	deletedPaths         []string

	// Settings for section visibility
	pipelineConfigured bool
	booxEnabled        bool

	// Boox-tab list
	booxNotes []service.BooxNoteSummary

	// ForestNote-tab fixtures
	forestNoteEnabled bool
	fnTree            []service.ForestNoteTreeNode
	fnUnfiled         []service.ForestNoteNotebook
	fnNotebookName    string
	fnPages           []service.ForestNotePage
	fnEntries         []service.ForestNoteEntry
	fnCrumbs          []service.ForestNoteCrumb
	fnDetail          service.ForestNoteNotebookDetail
	fnReprocessed     []string
	fnDeleted         []string
	fnExportPDF       []byte
	fnTextBoxes       []syncstore.TextBoxRef
	fnEdited          []string
}

func (m *mockNoteService) ListFiles(ctx context.Context, path, sort, order string, page, perPage int) ([]service.NoteFile, int, error) {
	return m.files, len(m.files), nil
}
func (m *mockNoteService) ListSupernoteFiles(ctx context.Context, path, sort, order string, page, perPage int) ([]service.NoteFile, int, error) {
	var out []service.NoteFile
	for _, f := range m.files {
		if f.Source != "boox" {
			out = append(out, f)
		}
	}
	return out, len(out), nil
}
func (m *mockNoteService) ListBooxNotes(ctx context.Context, device, folder, sort, order string, page, perPage int) ([]service.BooxNoteSummary, int, error) {
	if device == "" && folder == "" {
		return m.booxNotes, len(m.booxNotes), nil
	}
	var out []service.BooxNoteSummary
	for _, bn := range m.booxNotes {
		if device != "" && bn.DeviceModel != device {
			continue
		}
		if folder != "" && bn.Folder != folder {
			continue
		}
		out = append(out, bn)
	}
	return out, len(out), nil
}
func (m *mockNoteService) ListBooxFolders(ctx context.Context) ([]service.BooxFolder, error) {
	seen := map[string]int{}
	for _, bn := range m.booxNotes {
		seen[bn.Folder]++
	}
	out := make([]service.BooxFolder, 0, len(seen))
	for f, c := range seen {
		out = append(out, service.BooxFolder{Folder: f, Count: c})
	}
	return out, nil
}
func (m *mockNoteService) ListBooxDevices(ctx context.Context) ([]service.BooxDevice, error) {
	seen := map[string]int{}
	for _, bn := range m.booxNotes {
		if bn.DeviceModel == ".." {
			continue
		}
		seen[bn.DeviceModel]++
	}
	out := make([]service.BooxDevice, 0, len(seen))
	for d, c := range seen {
		out = append(out, service.BooxDevice{DeviceModel: d, Count: c})
	}
	return out, nil
}
func (m *mockNoteService) GetBooxNote(ctx context.Context, path string) (service.BooxNoteSummary, error) {
	for _, bn := range m.booxNotes {
		if bn.Path == path {
			return bn, nil
		}
	}
	return service.BooxNoteSummary{}, sql.ErrNoRows
}
func (m *mockNoteService) GetFile(ctx context.Context, path string) (service.NoteFile, error) {
	for _, f := range m.files {
		if f.Path == path {
			return f, nil
		}
	}
	return service.NoteFile{}, sql.ErrNoRows
}
func (m *mockNoteService) GetNoteDetails(ctx context.Context, path string) (interface{}, error) {
	return nil, nil
}
func (m *mockNoteService) GetContent(ctx context.Context, path string) (interface{}, error) {
	return m.contents[path], nil
}
func (m *mockNoteService) GetNotePages(ctx context.Context, path string) ([]service.NotePageView, error) {
	return m.notePages[path], nil
}
func (m *mockNoteService) RenderPage(ctx context.Context, path string, page int) (io.ReadCloser, string, error) {
	return m.renders[path], "image/jpeg", nil
}
func (m *mockNoteService) ScanFiles(ctx context.Context) error { return nil }
func (m *mockNoteService) Enqueue(ctx context.Context, path string, force bool) error {
	for i := range m.files {
		if m.files[i].Path == path {
			m.files[i].JobStatus = "pending"
		}
	}
	return nil
}
func (m *mockNoteService) Skip(ctx context.Context, path, reason string) error {
	for i := range m.files {
		if m.files[i].Path == path {
			m.files[i].JobStatus = "skipped"
		}
	}
	return nil
}
func (m *mockNoteService) Unskip(ctx context.Context, path string) error {
	for i := range m.files {
		if m.files[i].Path == path {
			m.files[i].JobStatus = ""
		}
	}
	return nil
}
func (m *mockNoteService) RetryFailed(ctx context.Context) error { return nil }
func (m *mockNoteService) DeleteNote(ctx context.Context, path string) error {
	m.deletedPaths = append(m.deletedPaths, path)
	return nil
}
func (m *mockNoteService) BulkDelete(ctx context.Context, paths []string) error {
	m.deletedPaths = append(m.deletedPaths, paths...)
	return nil
}
func (m *mockNoteService) SetEmbedIndex(d service.EmbedIndex)             {}
func (m *mockNoteService) SetForestNoteReader(r service.ForestNoteReader) {}
func (m *mockNoteService) HasForestNoteSource() bool                      { return m.forestNoteEnabled }
func (m *mockNoteService) ListForestNoteTree(ctx context.Context) ([]service.ForestNoteTreeNode, []service.ForestNoteNotebook, error) {
	return m.fnTree, m.fnUnfiled, nil
}
func (m *mockNoteService) ListForestNotePages(ctx context.Context, notebookID string) (string, []service.ForestNotePage, error) {
	return m.fnNotebookName, m.fnPages, nil
}
func (m *mockNoteService) SetForestNoteReprocessor(r service.ForestNoteReprocessor) {}
func (m *mockNoteService) ListForestNoteFolder(ctx context.Context, folderID, sortField, order string) ([]service.ForestNoteCrumb, []service.ForestNoteEntry, error) {
	return m.fnCrumbs, m.fnEntries, nil
}
func (m *mockNoteService) GetForestNoteNotebookDetail(ctx context.Context, notebookID string) (service.ForestNoteNotebookDetail, error) {
	return m.fnDetail, nil
}
func (m *mockNoteService) DeleteForestNoteNotebook(ctx context.Context, notebookID string) error {
	m.fnDeleted = append(m.fnDeleted, notebookID)
	return nil
}
func (m *mockNoteService) ReprocessForestNoteNotebook(ctx context.Context, notebookID string) error {
	m.fnReprocessed = append(m.fnReprocessed, notebookID)
	return nil
}
func (m *mockNoteService) ExportForestNoteNotebookPDF(ctx context.Context, notebookID string) (io.ReadCloser, string, error) {
	return io.NopCloser(strings.NewReader(string(m.fnExportPDF))), "notebook.pdf", nil
}
func (m *mockNoteService) ListForestNoteTextBoxes(ctx context.Context, notebookID string) ([]syncstore.TextBoxRef, error) {
	return m.fnTextBoxes, nil
}
func (m *mockNoteService) EditForestNoteTextBox(ctx context.Context, boxID, newText string) error {
	m.fnEdited = append(m.fnEdited, boxID+"="+newText)
	return nil
}
func (m *mockNoteService) StartProcessor(ctx context.Context) error {
	m.processorStarted = true
	return nil
}
func (m *mockNoteService) StopProcessor(ctx context.Context) error {
	m.processorStarted = false
	return nil
}
func (m *mockNoteService) StartBooxProcessor(ctx context.Context) error {
	m.booxProcessorStarted = true
	return nil
}
func (m *mockNoteService) StopBooxProcessor(ctx context.Context) error {
	m.booxProcessorStarted = false
	return nil
}
func (m *mockNoteService) GetProcessorStatus(ctx context.Context) (service.EmbeddingJobStatus, error) {
	return service.EmbeddingJobStatus{Running: m.processorStarted}, nil
}
func (m *mockNoteService) ImportFiles(ctx context.Context) error {
	m.importTriggered = true
	return nil
}
func (m *mockNoteService) MigrateImports(ctx context.Context) error {
	m.migrateTriggered = true
	return nil
}
func (m *mockNoteService) HasSupernoteSource() bool { return m.pipelineConfigured }
func (m *mockNoteService) HasBooxSource() bool      { return m.booxEnabled }
func (m *mockNoteService) ListVersions(ctx context.Context, path string) (interface{}, error) {
	return nil, nil
}
func (m *mockNoteService) ReconcileBooxCreatedAt(ctx context.Context) (int64, error) { return 0, nil }
func (m *mockNoteService) DeleteAutoNamedNotebooks(ctx context.Context) (int64, int64, int64, error) {
	return 0, 0, 0, nil
}
func (m *mockNoteService) ScanAndEnqueueUntracked(ctx context.Context) (int, int, error) {
	return 0, 0, nil
}
func (m *mockNoteService) MoveBooxNote(ctx context.Context, path, destFolder string) error {
	return nil
}
func (m *mockNoteService) BulkMoveBooxNotes(ctx context.Context, paths []string, destFolder string) (int, int, error) {
	return 0, 0, nil
}

// mockSearchService implements SearchService for testing
type mockSearchService struct {
	results  []service.SearchResult
	sessions interface{}
	messages interface{}

	embeddingPipelineConfigured bool
	chatEnabled                 bool

	// lastLimit captures the limit value passed to Search for tests that
	// want to assert the param plumbing. Zero value when nothing called
	// Search or when the caller passed 0 (service-default).
	lastLimit int
}

func (m *mockSearchService) Search(ctx context.Context, query, folder string, sources []string, limit int) ([]service.SearchResult, error) {
	// Tests don't observe limit today; capture it on the mock if a future
	// test wants to assert it was threaded through (left at zero value
	// otherwise so existing assertions keep working).
	m.lastLimit = limit
	return m.results, nil
}
func (m *mockSearchService) Ask(ctx context.Context, question string, sessionID int) (<-chan service.ChatResponse, error) {
	return nil, nil
}
func (m *mockSearchService) ListSessions(ctx context.Context) (interface{}, error) {
	return m.sessions, nil
}
func (m *mockSearchService) GetMessages(ctx context.Context, sessionID int) (interface{}, error) {
	return m.messages, nil
}
func (m *mockSearchService) TriggerBackfill(ctx context.Context) error { return nil }
func (m *mockSearchService) GetEmbeddingCount(ctx context.Context) int { return 0 }
func (m *mockSearchService) HasEmbeddingPipeline() bool                { return m.embeddingPipelineConfigured }

// mockConfigService implements ConfigService for testing
type mockConfigService struct {
	config          interface{}
	sources         interface{}
	restartRequired bool
}

func (m *mockConfigService) GetConfig(ctx context.Context) (interface{}, error)         { return m.config, nil }
func (m *mockConfigService) UpdateConfig(ctx context.Context, config interface{}) error { return nil }
func (m *mockConfigService) IsRestartRequired() bool                                    { return m.restartRequired }
func (m *mockConfigService) ListSources(ctx context.Context) (interface{}, error) {
	return m.sources, nil
}
func (m *mockConfigService) AddSource(ctx context.Context, source interface{}) error { return nil }
func (m *mockConfigService) UpdateSource(ctx context.Context, id string, source interface{}) error {
	return nil
}
func (m *mockConfigService) DeleteSource(ctx context.Context, id string) error { return nil }
