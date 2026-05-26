package service

import (
	"context"
	"io"
	"time"

	"github.com/sysop/ultrabridge/internal/booxpipeline"
)

// TaskStatus is a type-safe status for tasks.
type TaskStatus string

const (
	StatusNeedsAction TaskStatus = "needsAction"
	StatusCompleted   TaskStatus = "completed"
)

// Task represents a unified task entity.
type Task struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Status      TaskStatus `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	DueAt       *time.Time `json:"due_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Detail      *string    `json:"detail,omitempty"`
	Links       *TaskLink  `json:"links,omitempty"`
}

type TaskLink struct {
	AppName  string `json:"app_name"`
	FilePath string `json:"file_path"`
	Page     int    `json:"page"`
}

// NoteFile represents a notebook file (Supernote or Boox).
type NoteFile struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	RelPath    string    `json:"rel_path"`
	IsDir      bool      `json:"is_dir"`
	FileType   string    `json:"file_type"` // note, pdf, epub, other
	SizeBytes  int64     `json:"size_bytes"`
	CreatedAt  time.Time `json:"created_at"`
	ModifiedAt time.Time `json:"modified_at"`
	Source     string    `json:"source"`      // supernote, boox
	DeviceInfo *string   `json:"device_info"` // e.g. "A5 X"
	JobStatus  string    `json:"job_status"`  // pending, in_progress, done, failed, skipped
	LastError  *string   `json:"last_error"`
}

// BooxFolder is one row in the Boox folder facet — the on-device folder
// label and how many notes live under it. Passed to the Boox Files tab to
// build the folder-filter pill row.
type BooxFolder struct {
	Folder string `json:"folder"`
	Count  int    `json:"count"`
}

// BooxDevice is one row in the Boox device facet — the on-device model
// string and how many notes are attributed to it. The "..", legacy-import
// field-swap artifact is excluded at the store layer.
type BooxDevice struct {
	DeviceModel string `json:"device_model"`
	Count       int    `json:"count"`
}

// BooxNoteSummary is a Boox-tab-specific view of a Boox note, surfacing the
// on-device title, folder, device model, note type, and page count that the
// merged NoteFile shape hides.
type BooxNoteSummary struct {
	Path        string    `json:"path"`
	NoteID      string    `json:"note_id"`
	Title       string    `json:"title"`
	Filename    string    `json:"filename"`
	DeviceModel string    `json:"device_model"`
	NoteType    string    `json:"note_type"`
	Folder      string    `json:"folder"`
	PageCount   int       `json:"page_count"`
	SizeBytes   int64     `json:"size_bytes"`
	CreatedAt   time.Time `json:"created_at"`
	ModifiedAt  time.Time `json:"modified_at"`
	JobStatus   string    `json:"job_status"`
}

// EmbeddingJobStatus represents the background processing state.
type EmbeddingJobStatus struct {
	Running        bool                     `json:"running"`
	PendingCount   int                      `json:"pending_count"`
	InFlightCount  int                      `json:"in_flight_count"`
	ProcessedCount int                      `json:"processed_count"`
	FailedCount    int                      `json:"failed_count"`
	ActiveTask     *ActiveTask              `json:"active_task,omitempty"`
	Boox           *booxpipeline.QueueStatus `json:"boox,omitempty"`
}

type ActiveTask struct {
	Path      string    `json:"path"`
	StartedAt time.Time `json:"started_at"`
}

// TaskPatch is a partial update to a Task. Nil pointer fields mean "leave
// unchanged"; ClearDueAt exists because a *time.Time can't distinguish
// "don't touch" from "clear to null" on its own (ClearDueAt wins over DueAt
// when both are set). Title is non-clearable — CalDAV VTODOs require a
// SUMMARY, and empty-string titles round-trip poorly to the device. Detail
// is cleared by sending an empty string ("" is a legitimate empty value).
type TaskPatch struct {
	Title      *string    `json:"title,omitempty"`
	DueAt      *time.Time `json:"due_at,omitempty"`
	ClearDueAt bool       `json:"clear_due_at,omitempty"`
	Detail     *string    `json:"detail,omitempty"`
}

// TaskService manages task-related operations.
type TaskService interface {
	List(ctx context.Context) ([]Task, error)
	Get(ctx context.Context, id string) (Task, error)
	Create(ctx context.Context, title string, dueAt *time.Time) (Task, error)
	Update(ctx context.Context, id string, patch TaskPatch) (Task, error)
	Complete(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
	PurgeCompleted(ctx context.Context) error
	BulkComplete(ctx context.Context, ids []string) error
	BulkDelete(ctx context.Context, ids []string) error
}

// NoteService manages note files and background processing.
type NoteService interface {
	ListFiles(ctx context.Context, path string, sort, order string, page, perPage int) ([]NoteFile, int, error)
	ListSupernoteFiles(ctx context.Context, path string, sort, order string, page, perPage int) ([]NoteFile, int, error)
	ListBooxNotes(ctx context.Context, device, folder, sort, order string, page, perPage int) ([]BooxNoteSummary, int, error)
	ListBooxFolders(ctx context.Context) ([]BooxFolder, error)
	ListBooxDevices(ctx context.Context) ([]BooxDevice, error)
	GetFile(ctx context.Context, path string) (NoteFile, error)
	GetBooxNote(ctx context.Context, path string) (BooxNoteSummary, error)
	GetNoteDetails(ctx context.Context, path string) (interface{}, error) // history/job info
	GetContent(ctx context.Context, path string) (interface{}, error)     // OCR text and page metadata
	RenderPage(ctx context.Context, path string, page int) (io.ReadCloser, string, error) // image stream, content-type
	
	ScanFiles(ctx context.Context) error
	Enqueue(ctx context.Context, path string, force bool) error
	Skip(ctx context.Context, path, reason string) error
	Unskip(ctx context.Context, path string) error
	RetryFailed(ctx context.Context) error
	DeleteNote(ctx context.Context, path string) error
	BulkDelete(ctx context.Context, paths []string) error
	// SetEmbedDeleter wires the RAG embedding store so deletes drop embeddings.
	SetEmbedDeleter(d EmbedDeleter)

	// Source Presence
	HasSupernoteSource() bool
	HasBooxSource() bool
	ListVersions(ctx context.Context, path string) (interface{}, error)
	
	// Pipeline Control (Supernote)
	StartProcessor(ctx context.Context) error
	StopProcessor(ctx context.Context) error
	// Pipeline Control (Boox)
	StartBooxProcessor(ctx context.Context) error
	StopBooxProcessor(ctx context.Context) error
	GetProcessorStatus(ctx context.Context) (EmbeddingJobStatus, error)
	
	// Import (Boox specific)
	ImportFiles(ctx context.Context) error
	MigrateImports(ctx context.Context) error

	// Maintenance (Boox specific)
	ReconcileBooxCreatedAt(ctx context.Context) (int64, error)
	DeleteAutoNamedNotebooks(ctx context.Context) (rows, files, versions int64, err error)
	ScanAndEnqueueUntracked(ctx context.Context) (scanned, enqueued int, err error)

	// Move (Boox specific)
	MoveBooxNote(ctx context.Context, path, destFolder string) error
	BulkMoveBooxNotes(ctx context.Context, paths []string, destFolder string) (moved, failed int, err error)
}

// SearchResult represents a single search match.
type SearchResult struct {
	Path       string  `json:"path"`
	Page       int     `json:"page"`
	Title      string  `json:"title"` // note/digest title, if any
	Snippet    string  `json:"snippet"`
	Score      float32 `json:"score"`
	SourceType string  `json:"source_type"` // supernote|boox|forestnote|digest
}

// SearchService manages search and chat interactions.
type SearchService interface {
	// Search runs hybrid (FTS5 + vector) retrieval. sources filters by source
	// type (empty = all); see rag.Source* constants for the values.
	Search(ctx context.Context, query, folder string, sources []string) ([]SearchResult, error)
	
	// Chat (SSE stream)
	Ask(ctx context.Context, question string, sessionID int) (<-chan ChatResponse, error)
	ListSessions(ctx context.Context) (interface{}, error)
	GetMessages(ctx context.Context, sessionID int) (interface{}, error)

	// Embeddings
	TriggerBackfill(ctx context.Context) error
	GetEmbeddingCount(ctx context.Context) int
	HasEmbeddingPipeline() bool
}

type ChatResponse struct {
	Type    string      `json:"type"` // session, content, error
	Content string      `json:"content,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

// DigestView is one digest ("summary") item for the web Digests tab.
type DigestView struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	Excerpt        string   `json:"excerpt"`         // the saved Content
	Comment        string   `json:"comment"`         // handwriting comment text
	Tags           []string `json:"tags"`            // split from the comma-separated column
	Group          string   `json:"group"`           // parent group name (resolved), if any
	SourceLabel    string   `json:"source_label"`    // "Note" or "PDF"
	HasHandwriting bool     `json:"has_handwriting"` // a .mark annotation exists
	CreatedAt      int64    `json:"created_at"`      // millis UTC
	ModifiedAt     int64    `json:"modified_at"`     // millis UTC
}

// DigestGroupView is a digest group/library, used for the filter pills.
type DigestGroupView struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

// DigestService is the web read surface over the digest store (Phase D2).
// Constructed only in SPC server mode with a digest store; nil otherwise, so
// the Digests tab and nav entry hide when no digests can exist.
type DigestService interface {
	ListDigests(ctx context.Context, group, tag string, page, perPage int) ([]DigestView, int, error)
	ListGroups(ctx context.Context) ([]DigestGroupView, error)
}

// ConfigService manages system configuration and sources.
type ConfigService interface {
	GetConfig(ctx context.Context) (interface{}, error)
	UpdateConfig(ctx context.Context, config interface{}) error
	IsRestartRequired() bool
	
	ListSources(ctx context.Context) (interface{}, error)
	AddSource(ctx context.Context, source interface{}) error
	UpdateSource(ctx context.Context, id string, source interface{}) error
	DeleteSource(ctx context.Context, id string) error
}
