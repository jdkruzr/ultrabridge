package service

import (
	"context"
	"io"
	"time"

	"github.com/sysop/ultrabridge/internal/booxpipeline"
	"github.com/sysop/ultrabridge/internal/syncstore"
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
	// Links is reserved (was never populated); see URL/Priority/Categories/ForestNote
	// below for the structured task-metadata that mapInternalTask actually fills in.
	Links *TaskLink `json:"links,omitempty"`
	// URL surfaces the VTODO URL property (stored in tasks.links). FN auto-fills this
	// with an https://<ub-host>/forestnote/n/.../p/... deep link; other CalDAV clients
	// can put anything URI-ish here.
	URL *string `json:"url,omitempty"`
	// Priority surfaces VTODO PRIORITY (RFC 5545 §3.8.1.9: integer "1"-"9" as string,
	// 1=highest, 5=normal, 9=lowest). Stored verbatim in tasks.importance.
	Priority *string `json:"priority,omitempty"`
	// Categories surfaces VTODO CATEGORIES, parsed from the ical_blob at response time
	// (no structured column). Empty list when absent or blob is empty.
	Categories []string `json:"categories,omitempty"`
	// Comment surfaces VTODO COMMENT (RFC 5545 §3.8.1.4), parsed from the
	// ical_blob at response time. Multi-COMMENT VTODOs are joined with blank
	// lines. FN may write the full recognized-text body here when the user
	// opts in to "include recognized text"; clients render as preformatted.
	Comment string `json:"comment,omitempty"`
	// ForestNote provenance, populated when the task carried X-FORESTNOTE-* properties
	// on its inbound VTODO. Nil for non-FN clients (Apple Reminders, Tasks.org, etc.).
	ForestNote *TaskForestNote `json:"forestnote,omitempty"`
	// Attachments surfaces VTODO ATTACH properties (RFC 5545 §3.8.1.1), parsed
	// from the ical_blob at response time. Empty for tasks with no attachments.
	Attachments []Attachment `json:"attachments,omitempty"`
	// Deleted is true for soft-deleted rows (is_deleted='Y'). Default queries hide
	// these entirely — only surfaced when the caller opts in via ListIncludingDeleted
	// / ?include_deleted=true / the equivalent MCP flag. Useful for "what's in the
	// trash" queries and for the hard-purge tool to confirm targets before running.
	Deleted bool `json:"deleted,omitempty"`
}

type TaskLink struct {
	AppName  string `json:"app_name"`
	FilePath string `json:"file_path"`
	Page     int    `json:"page"`
}

// Attachment is one VTODO ATTACH (RFC 5545 §3.8.1.1) surfaced on a task. The
// inline bytes are never embedded here: for an inline-binary attachment the
// payload is described (Size/FmtType/Filename) and URL is UB's signed download
// endpoint (set once the attachment has been de-bloated; empty only in a
// deployment where ATTACH serving is unconfigured). For a URI attachment, URL
// is the link verbatim.
type Attachment struct {
	URL      string `json:"url,omitempty"`
	FmtType  string `json:"fmt_type,omitempty"`
	Filename string `json:"filename,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Inline   bool   `json:"inline,omitempty"`
}

// TaskForestNote carries the structured columns extracted from X-FORESTNOTE-*
// properties at PUT time, plus NativeURL (X-FORESTNOTE-NATIVE-URL) which is
// parsed from the blob since it's blob-only. All fields are optional — a task
// may have the notebook IDs without the name, or vice versa.
type TaskForestNote struct {
	NotebookID   string `json:"notebook_id,omitempty"`
	PageID       string `json:"page_id,omitempty"`
	NotebookName string `json:"notebook_name,omitempty"`
	Source       string `json:"source,omitempty"`
	NativeURL    string `json:"native_url,omitempty"`
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

// ForestNoteNotebook is one synced ForestNote notebook in the Files tab. Path is
// the notebook-level URI (forestnote://{notebook_id}); pages are addressed
// individually via ForestNotePage.Path.
type ForestNoteNotebook struct {
	NotebookID string `json:"notebook_id"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	FolderID   string `json:"folder_id"`
	PageCount  int    `json:"page_count"`
	CreatedAt  int64  `json:"created_at"`  // ms UTC, 0 = unset
	ModifiedAt int64  `json:"modified_at"` // ms UTC, derived last-activity
}

// ForestNoteEntry is one row in the Supernote-style ForestNote Files table: a
// folder (IsFolder) the user can navigate into, or a notebook they can open.
// For a folder, ID is the folder id (the ?folder= navigation target) and Path/
// PageCount/Status are empty/zero. For a notebook, ID is the notebook id and
// Path is its forestnote:// URI.
type ForestNoteEntry struct {
	IsFolder   bool   `json:"is_folder"`
	ID         string `json:"id"`
	Name       string `json:"name"`
	Path       string `json:"path,omitempty"`
	PageCount  int    `json:"page_count"`
	CreatedAt  int64  `json:"created_at"`
	ModifiedAt int64  `json:"modified_at"`
	Status     string `json:"status,omitempty"` // notebook only: blank|partial|indexed
}

// ForestNoteCrumb is one hop in the folder breadcrumb trail (root→current).
type ForestNoteCrumb struct {
	FolderID string `json:"folder_id"`
	Name     string `json:"name"`
}

// ForestNoteNotebookDetail is the enriched click-through view of one notebook:
// header metadata plus its pages, each carrying OCR body text for display.
type ForestNoteNotebookDetail struct {
	NotebookID string           `json:"notebook_id"`
	Name       string           `json:"name"`
	CreatedAt  int64            `json:"created_at"`
	ModifiedAt int64            `json:"modified_at"`
	PageCount  int              `json:"page_count"`
	FolderPath []string         `json:"folder_path"` // ancestor folder names root→leaf
	Pages      []ForestNotePage `json:"pages"`
}

// NotePageView is one indexed page of a Supernote/Boox note, surfaced for the
// in-tab detail page grid. It mirrors the OCR-bearing fields ForestNotePage
// carries so the three sources share one detail renderer.
type NotePageView struct {
	Page      int    `json:"page"`
	Source    string `json:"source"`     // OCR provenance ("myScript" | "api")
	BodyText  string `json:"body_text"`  // recognized text for the page
	Keywords  string `json:"keywords"`   // page-0 keyword annotations, if any
	TitleText string `json:"title_text"` // page-0 title annotation, if any
}

// ForestNoteTreeNode is a folder in the ForestNote browse tree, holding its child
// folders and the notebooks filed directly under it.
type ForestNoteTreeNode struct {
	FolderID  string               `json:"folder_id"`
	Name      string               `json:"name"`
	Children  []ForestNoteTreeNode `json:"children,omitempty"`
	Notebooks []ForestNoteNotebook `json:"notebooks,omitempty"`
}

// ForestNotePage is one renderable page of a notebook. Path is the render target
// (forestnote://{notebook_id}/{page_id}); Ordinal is its display position.
type ForestNotePage struct {
	PageID   string `json:"page_id"`
	Path     string `json:"path"`
	Ordinal  int    `json:"ordinal"`
	BodyText string `json:"body_text,omitempty"` // OCR text from the search index
	Source   string `json:"source,omitempty"`    // OCR provenance (e.g. "forestnote")
}

// RemarkableCrumb is one hop in the reMarkable folder breadcrumb trail.
type RemarkableCrumb struct {
	FolderID string `json:"folder_id"`
	Name     string `json:"name"`
}

// RemarkableEntry is one metadata-only row in the reMarkable Files tab.
type RemarkableEntry struct {
	IsFolder  bool   `json:"is_folder"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"` // "folder" | "document"
	Parent    string `json:"parent"`
	Path      string `json:"path,omitempty"`
	PageCount int    `json:"page_count"`
}

// RemarkableDocumentDetail is the first structural detail view for a synced
// reMarkable node. Render/OCR availability stay explicit false until later
// chunks wire real page rendering and indexing.
type RemarkableDocumentDetail struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Type            string   `json:"type"`
	Parent          string   `json:"parent"`
	Path            string   `json:"path"`
	PageCount       int      `json:"page_count"`
	FolderPath      []string `json:"folder_path,omitempty"`
	RenderAvailable bool     `json:"render_available"`
	OCRAvailable    bool     `json:"ocr_available"`
}

// EmbeddingJobStatus represents the background processing state.
type EmbeddingJobStatus struct {
	Running        bool                      `json:"running"`
	PendingCount   int                       `json:"pending_count"`
	InFlightCount  int                       `json:"in_flight_count"`
	ProcessedCount int                       `json:"processed_count"`
	FailedCount    int                       `json:"failed_count"`
	ActiveTask     *ActiveTask               `json:"active_task,omitempty"`
	Boox           *booxpipeline.QueueStatus `json:"boox,omitempty"`
	ForestNote     *ForestNoteQueueStatus    `json:"forestnote,omitempty"`
}

// ForestNoteQueueStatus is the ForestNote sync bridge's work snapshot for
// the /files/status poller. Mirrors the Boox queue shape conceptually
// (pending / in_flight / processed) but is plumbed from a separate source:
// the syncbridge worker that turns inbound page strokes into OCR text. Nil
// in the parent struct when no ForestNote source is wired (server-mode
// without the FN source, or pre-source-start polls).
type ForestNoteQueueStatus struct {
	Pending   int   `json:"pending"`   // pages waiting in the bridge channel
	InFlight  int   `json:"in_flight"` // pages currently being OCR'd
	Processed int64 `json:"processed"` // pages finished since process start
	Dropped   int64 `json:"dropped"`   // enqueues lost to channel-full
	Capacity  int   `json:"capacity"`  // channel buffer size
}

type ActiveTask struct {
	Path      string    `json:"path"`
	StartedAt time.Time `json:"started_at"`
}

// TaskPatch is a partial update to a Task. Nil pointer fields mean "leave
// unchanged"; ClearXxx flags exist because a *string / *time.Time can't
// distinguish "don't touch" from "clear to null" on its own (ClearXxx wins
// over the value pointer when both are set). Title is non-clearable —
// CalDAV VTODOs require a SUMMARY, and empty-string titles round-trip poorly
// to the device. Detail is cleared by sending an empty string ("" is a
// legitimate empty value, distinct from absent).
//
// URL, Priority, Categories, and Comment cover the metadata that REST/MCP
// writers (not CalDAV PUT — those still go through the iCal blob round-trip)
// can supply alongside the basics. Categories is wholesale (the patch
// replaces the entire list; partial add/remove would need a richer API).
type TaskPatch struct {
	Title         *string    `json:"title,omitempty"`
	DueAt         *time.Time `json:"due_at,omitempty"`
	ClearDueAt    bool       `json:"clear_due_at,omitempty"`
	Detail        *string    `json:"detail,omitempty"`
	URL           *string    `json:"url,omitempty"`
	ClearURL      bool       `json:"clear_url,omitempty"`
	Priority      *string    `json:"priority,omitempty"`
	ClearPriority bool       `json:"clear_priority,omitempty"`
	// Categories: nil = leave alone, non-nil (incl. empty slice) = replace.
	// A nil-vs-empty distinction is expressible in Go JSON decoding via a
	// pointer, but []string already encodes the same thing — callers send
	// `"categories": []` to clear, or omit the field to leave unchanged.
	Categories   *[]string `json:"categories,omitempty"`
	Comment      *string   `json:"comment,omitempty"`
	ClearComment bool      `json:"clear_comment,omitempty"`
}

// TaskCreate is the input shape for creating a new task via the
// service/REST/MCP write path. Title is required; everything else is
// optional. CalDAV-PUT-created tasks bypass this struct entirely — they
// flow through VTODOToTask with the full iCal blob preserved.
type TaskCreate struct {
	Title      string     `json:"title"`
	DueAt      *time.Time `json:"due_at,omitempty"`
	Detail     string     `json:"detail,omitempty"`
	URL        string     `json:"url,omitempty"`
	Priority   string     `json:"priority,omitempty"`
	Categories []string   `json:"categories,omitempty"`
	Comment    string     `json:"comment,omitempty"`
}

// TaskService is the service-layer task API. List hides soft-deleted rows; use
// ListIncludingDeleted to include them (e.g. for the trash view or to find
// purge candidates).
type TaskService interface {
	List(ctx context.Context) ([]Task, error)
	ListIncludingDeleted(ctx context.Context) ([]Task, error)
	Get(ctx context.Context, id string) (Task, error)
	Create(ctx context.Context, input TaskCreate) (Task, error)
	Update(ctx context.Context, id string, patch TaskPatch) (Task, error)
	Complete(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
	// PurgeCompleted soft-deletes every completed task. Returns the count
	// affected so callers (REST handler, MCP tool) can surface "Soft-deleted
	// N completed task(s)." rather than the previous opaque all-or-nothing
	// success.
	PurgeCompleted(ctx context.Context) (int64, error)
	// PurgeDeleted hard-deletes soft-deleted rows whose last_modified is older
	// than olderThanDays. Returns (purged, skipped, error) — skipped counts
	// rows that were soft-deleted but inside the safety window (too recent),
	// so a caller can tell "0 purged because nothing was eligible" apart from
	// "0 purged because the gate broke." Irreversible.
	PurgeDeleted(ctx context.Context, olderThanDays int) (purged, skipped int64, err error)
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
	GetNoteDetails(ctx context.Context, path string) (interface{}, error)                 // history/job info
	GetContent(ctx context.Context, path string) (interface{}, error)                     // OCR text and page metadata
	GetNotePages(ctx context.Context, path string) ([]NotePageView, error)                // typed page content for the in-tab detail grid
	RenderPage(ctx context.Context, path string, page int) (io.ReadCloser, string, error) // image stream, content-type
	// RenderSupernotePage renders an absolute .note path through the Supernote
	// (go-sn) renderer unconditionally, bypassing RenderPage's source-detection.
	// Used for digest source pages, which are always Supernote notes and may need
	// to render when no filesystem Supernote source is configured (SPC-server-only
	// deployments) — where RenderPage's heuristics could misroute to Boox.
	RenderSupernotePage(ctx context.Context, path string, page int) (io.ReadCloser, string, error)

	ScanFiles(ctx context.Context) error
	Enqueue(ctx context.Context, path string, force bool) error
	Skip(ctx context.Context, path, reason string) error
	Unskip(ctx context.Context, path string) error
	RetryFailed(ctx context.Context) error
	DeleteNote(ctx context.Context, path string) error
	BulkDelete(ctx context.Context, paths []string) error
	// SetEmbedIndex wires the RAG embedding store so deletes drop embeddings and
	// moves repoint them.
	SetEmbedIndex(d EmbedIndex)
	// SetForestNoteReader wires the syncstore mirror for browsing/rendering
	// synced ForestNote notebooks.
	SetForestNoteReader(r ForestNoteReader)
	// SetForestNoteReprocessor wires the source's re-OCR trigger (re-enqueues a
	// notebook's pages onto the sync bridge). Nil-safe.
	SetForestNoteReprocessor(r ForestNoteReprocessor)

	// ForestNote (synced device source, no filesystem)
	ListForestNoteTree(ctx context.Context) (roots []ForestNoteTreeNode, unfiled []ForestNoteNotebook, err error)
	ListForestNotePages(ctx context.Context, notebookID string) (name string, pages []ForestNotePage, err error)
	// ListForestNoteFolder lists the direct contents of a folder (empty = root) as
	// a Supernote-style table, with the breadcrumb trail to that folder.
	ListForestNoteFolder(ctx context.Context, folderID, sortField, order string) (crumbs []ForestNoteCrumb, entries []ForestNoteEntry, err error)
	// GetForestNoteNotebookDetail returns a notebook's header metadata + pages
	// (with OCR body text) for the click-through detail view.
	GetForestNoteNotebookDetail(ctx context.Context, notebookID string) (ForestNoteNotebookDetail, error)
	// DeleteForestNoteNotebook soft-deletes a notebook in the mirror and de-indexes
	// its pages (UB-local; see syncstore.SoftDeleteNotebook for resurrection notes).
	DeleteForestNoteNotebook(ctx context.Context, notebookID string) error
	// ReprocessForestNoteNotebook re-enqueues a notebook's pages for re-OCR/re-index.
	ReprocessForestNoteNotebook(ctx context.Context, notebookID string) error
	// ListForestNoteTextBoxes returns a notebook's live text boxes for discovery.
	ListForestNoteTextBoxes(ctx context.Context, notebookID string) ([]syncstore.TextBoxRef, error)
	// EditForestNoteTextBox authors a server-side text-box text edit (relayed to
	// devices) and re-renders/re-indexes the affected page.
	EditForestNoteTextBox(ctx context.Context, boxID, newText string) error
	// ExportForestNoteNotebookPDF renders a notebook's live pages to a single PDF.
	ExportForestNoteNotebookPDF(ctx context.Context, notebookID string) (stream io.ReadCloser, filename string, err error)

	// reMarkable (synced cloud-protocol source, metadata-only in this chunk)
	SetRemarkableReader(r RemarkableReader)
	ListRemarkableDocuments(ctx context.Context) ([]RemarkableDocument, error)
	ListRemarkableFolder(ctx context.Context, folderID, sortField, order string) (crumbs []RemarkableCrumb, entries []RemarkableEntry, err error)
	GetRemarkableDocumentDetail(ctx context.Context, documentID string) (RemarkableDocumentDetail, error)

	// Source Presence
	HasSupernoteSource() bool
	HasBooxSource() bool
	HasForestNoteSource() bool
	HasRemarkableSource() bool
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
	// type (empty = all); see rag.Source* constants for the values. limit caps
	// the number of returned results (0 = service default; capped server-side).
	Search(ctx context.Context, query, folder string, sources []string, limit int) ([]SearchResult, error)

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

	// Detail-view fields (surfaced by GetDigest for the digest detail page).
	SourcePath         string `json:"source_path,omitempty"`          // device-relative source doc, e.g. "NOTE/Note/foo.note"
	SourceType         int    `json:"source_type,omitempty"`          // 1=PDF, 2=Note
	HandwriteInnerName string `json:"handwrite_inner_name,omitempty"` // .mark blob filename under <SPCFileRoot>/.digests, if any
	NotePage           int    `json:"note_page,omitempty"`            // page ordinal the excerpt came from (from metadata.note_page)
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
	// GetDigest returns one digest with detail-view fields populated, or the
	// store's ErrNotFound. Backs the GET /digests/{id} detail page.
	GetDigest(ctx context.Context, id int64) (DigestView, error)
	// DeleteDigest soft-deletes a digest and propagates the delete to the device
	// via a durable DELETE_DIGEST tombstone (D2).
	DeleteDigest(ctx context.Context, id int64) error
	// SetTombstoneQueue wires the durable device-tombstone seam (SPC server mode only).
	SetTombstoneQueue(q DigestTombstoneQueue)
}

// SyncDevice is one registered ForestNote sync device for the management UI
// and API (a view over syncstore.DeviceRow; redeclared with JSON tags so the
// web contract doesn't leak the syncstore package, mirroring
// ForestNoteQueueStatus).
type SyncDevice struct {
	SiteID string `json:"site_id"`
	Name   string `json:"name"` // "" if the device never sent a device_name
	// FirstSeen is decoded from the site_id ULID's embedded timestamp (when the
	// install minted it); 0 for synthetic/test ids. LastSeen is the registry
	// row's updated_at. Both millisecond UTC.
	FirstSeen   int64 `json:"first_seen"`
	LastSeen    int64 `json:"last_seen"`
	LastPullSeq int64 `json:"last_pull_seq"`
	AckedOpSeq  int64 `json:"acked_op_seq"`
	PendingOps  int64 `json:"pending_ops"`
	Stale       bool  `json:"stale"`
	// PinsWatermark marks the active laggard currently holding tombstone
	// compaction back behind another active device.
	PinsWatermark bool `json:"pins_watermark"`
}

// SyncCompactResult is one manual relay-log compaction pass's outcome.
type SyncCompactResult struct {
	Watermark           int64    `json:"watermark"`
	CollapsedSuperseded int      `json:"collapsed_superseded"`
	PurgedTombstones    int      `json:"purged_tombstones"`
	EvictedSites        []string `json:"evicted_sites"`
}

// SyncDeviceService is the device-management surface over the ForestNote sync
// source: list registered devices, prune one (cleanup-only delete of its
// registry row — spec §4.3), and run a relay-log compaction pass on demand.
// Constructed only when a ForestNote source is active; nil hides the UI card.
type SyncDeviceService interface {
	ListSyncDevices(ctx context.Context) ([]SyncDevice, error)
	// PruneSyncDevice returns ErrSyncDeviceNotFound when no such device exists.
	PruneSyncDevice(ctx context.Context, siteID string) error
	CompactNow(ctx context.Context) (SyncCompactResult, error)
}

type RemarkableDevice struct {
	DeviceID  string `json:"device_id"`
	Name      string `json:"name"`
	FirstSeen int64  `json:"first_seen"`
	LastSeen  int64  `json:"last_seen"`
}

// RemarkableDocument is a read-only view of one node (folder or document) in
// the synced reMarkable tree, surfaced on the Files tab and /api/v1.
type RemarkableDocument struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"` // "folder" | "document"
	Parent    string `json:"parent"`
	PageCount int    `json:"page_count"`
}

type RemarkableDeviceService interface {
	ListDevices(ctx context.Context) ([]RemarkableDevice, error)
	ListDocuments(ctx context.Context) ([]RemarkableDocument, error)
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
