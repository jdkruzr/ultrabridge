package web

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sysop/ultrabridge/internal/appconfig"
	"github.com/sysop/ultrabridge/internal/digeststore"
	"github.com/sysop/ultrabridge/internal/fnpath"
	"github.com/sysop/ultrabridge/internal/logging"
	"github.com/sysop/ultrabridge/internal/mcpauth"
	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/rag"
	"github.com/sysop/ultrabridge/internal/service"
	"github.com/sysop/ultrabridge/internal/source"
	"github.com/sysop/ultrabridge/internal/syncstore"
	"github.com/sysop/ultrabridge/internal/taskattach"
)

//go:embed all:templates
var templateFS embed.FS

// fileRowCtx is the data shape passed to the _file_row template block.
// It pairs a single NoteFile with the containing directory's RelPath so
// per-row buttons can emit back= query strings on their hx-post URLs.
// Unexported: internal to the web package; templates access .File and
// .RelPath via reflection (field export is what matters there).
type fileRowCtx struct {
	File    service.NoteFile
	RelPath string
}

// fnRowCtx is the data shape passed into the _fn_note_row fragment: one
// ForestNote table entry plus the folder currently being browsed (so per-row
// actions can emit a back= query string for the non-HX redirect).
type fnRowCtx struct {
	Entry    service.ForestNoteEntry
	FolderID string
}

//go:embed static
var staticFS embed.FS

type Handler struct {
	tasks       service.TaskService
	notes       service.NoteService
	search      service.SearchService
	config      service.ConfigService
	digests     service.DigestService     // optional; nil when no digest store (non-server mode)
	syncDevices service.SyncDeviceService // optional; nil when no ForestNote source (hides the Sync Devices card)
	rmDevices   service.RemarkableDeviceService

	noteDB          *sql.DB
	notesPathPrefix string
	booxNotesPath   string
	booxImportPath  string
	spcFileRoot     string // UB-as-SPC file tree root; resolves digest source paths (set via SetSPCFileRoot)
	tmpl            *template.Template
	mux             *http.ServeMux
	logger          *slog.Logger
	broadcaster     *logging.LogBroadcaster

	oauthCodesMu sync.Mutex
	oauthCodes   map[string]time.Time // code -> expiry

	// Task ATTACH serving (optional; nil until SetTaskAttach is called from
	// main.go). The download/render handlers these back are mounted on the
	// top-level mux OUTSIDE the auth wrapper so third-party CalDAV clients can
	// fetch ATTACH URIs with no credentials — the URL signature is the guard.
	attachSigner  *taskattach.Signer
	attachStore   *taskattach.BlobStore
	attachBaseURL string
}

func formatDueTime(val interface{}) string {
	var t time.Time
	switch v := val.(type) {
	case int64:
		if v == 0 {
			return "No due date"
		}
		t = time.UnixMilli(v).UTC()
	case *time.Time:
		if v == nil {
			return "No due date"
		}
		t = *v
	case time.Time:
		if v.IsZero() {
			return "No due date"
		}
		t = v
	default:
		return "No due date"
	}
	return t.Format("2006-01-02")
}

func formatCreated(val interface{}) string {
	var t time.Time
	switch v := val.(type) {
	case int64:
		if v == 0 {
			return "-"
		}
		t = time.UnixMilli(v).UTC()
	case *time.Time:
		if v == nil {
			return "-"
		}
		t = *v
	case time.Time:
		if v.IsZero() {
			return "-"
		}
		t = v
	case sql.NullInt64:
		if !v.Valid || v.Int64 == 0 {
			return "-"
		}
		t = time.UnixMilli(v.Int64).UTC()
	default:
		return "-"
	}
	return t.Format("2006-01-02")
}

func NewHandler(
	tasks service.TaskService,
	notes service.NoteService,
	search service.SearchService,
	config service.ConfigService,
	noteDB *sql.DB,
	notesPathPrefix string,
	booxNotesPath string,
	logger *slog.Logger,
	broadcaster *logging.LogBroadcaster,
) *Handler {
	h := &Handler{
		tasks:           tasks,
		notes:           notes,
		search:          search,
		config:          config,
		noteDB:          noteDB,
		notesPathPrefix: notesPathPrefix,
		booxNotesPath:   booxNotesPath,
		logger:          logger,
		broadcaster:     broadcaster,
		mux:             http.NewServeMux(),
	}

	if noteDB != nil {
		h.booxImportPath, _ = notedb.GetSetting(context.Background(), noteDB, appconfig.KeyBooxImportPath)
	}

	funcMap := template.FuncMap{
		"formatDueTime":   formatDueTime,
		"formatCreated":   formatCreated,
		"formatTimestamp": formatTimestampMs,
		"syncModelFor":    source.SyncModelFor,
		"fileTypeStr":     func(ft string) string { return ft },
		"fileRowID": func(path string) string {
			sum := sha1.Sum([]byte(path))
			return "file-" + hex.EncodeToString(sum[:])[:12]
		},
		"makeFileRowCtx": func(f service.NoteFile, relPath string) fileRowCtx {
			return fileRowCtx{File: f, RelPath: relPath}
		},
		"makeFNRowCtx": func(e service.ForestNoteEntry, folderID string) fnRowCtx {
			return fnRowCtx{Entry: e, FolderID: folderID}
		},
		"noteSource": func(path string) string {
			if h.booxNotesPath != "" && strings.HasPrefix(path, h.booxNotesPath) {
				return "Boox"
			}
			if h.booxImportPath != "" && strings.HasPrefix(path, h.booxImportPath) {
				return "Boox"
			}
			return "Supernote"
		},
		"hasPrefix":  strings.HasPrefix,
		"add":        func(a, b int) int { return a + b },
		"sub":        func(a, b int) int { return a - b },
		"trimPrefix": strings.TrimPrefix,
		// taskDetailHTML renders a task's Detail string as trusted HTML.
		// Two recognized formats produce a clickable link that navigates
		// to the note's details modal:
		//
		// 1. New format (CreateTasksFromTodos post-2026-04-14):
		//       "From Boox red ink in <basename>\nOpen: /files/boox?detail=..."
		//    The second line is rendered as an <a href> link that HTMX-swaps
		//    the Boox Files tab with the details modal auto-opened.
		//
		// 2. Legacy format (CreateTasksFromTodos pre-2026-04-14):
		//       "From Boox red ink: <absolute path>"
		//    The whole string is wrapped in a link that navigates to
		//    /files/boox?detail=<urlquery path>. Lets existing CalDAV-synced
		//    tasks upgrade to clickable without a DB backfill.
		//
		// Any other string renders as-is (escaped).
		"taskDetailHTML": func(detail string) template.HTML {
			esc := template.HTMLEscapeString
			const newPrefix = "From Boox red ink in "
			const openMarker = "\nOpen: "
			if strings.HasPrefix(detail, newPrefix) {
				if openIdx := strings.Index(detail, openMarker); openIdx >= 0 {
					headerLine := detail[:openIdx]
					href := detail[openIdx+len(openMarker):]
					return template.HTML(
						esc(headerLine) + `<br><a href="` + esc(href) +
							`" hx-get="` + esc(href) +
							`" hx-target="#main-content" hx-push-url="true" style="color:#6b7280;">Open note details ⬔</a>`,
					)
				}
			}
			const legacyPrefix = "From Boox red ink: "
			if strings.HasPrefix(detail, legacyPrefix) {
				path := detail[len(legacyPrefix):]
				href := "/files/boox?detail=" + url.QueryEscape(path)
				return template.HTML(
					`<a href="` + esc(href) +
						`" hx-get="` + esc(href) +
						`" hx-target="#main-content" hx-push-url="true" style="color:#6b7280;">` +
						esc(detail) + `</a>`,
				)
			}
			return template.HTML(esc(detail))
		},
		"taskLink": func(val interface{}) map[string]interface{} {
			if val == nil {
				return nil
			}
			var link struct {
				AppName  string `json:"appName"`
				FilePath string `json:"filePath"`
				Page     int    `json:"page"`
			}
			switch v := val.(type) {
			case string:
				if v == "" {
					return nil
				}
				data, _ := base64.StdEncoding.DecodeString(v)
				json.Unmarshal(data, &link)
			case *service.TaskLink:
				if v == nil {
					return nil
				}
				link.AppName, link.FilePath, link.Page = v.AppName, v.FilePath, v.Page
			case service.TaskLink:
				link.AppName, link.FilePath, link.Page = v.AppName, v.FilePath, v.Page
			default:
				return nil
			}
			if link.FilePath == "" {
				return nil
			}
			const devicePrefix = "/storage/emulated/0/Note/"
			localPath := link.FilePath
			if h.notesPathPrefix != "" && strings.HasPrefix(link.FilePath, devicePrefix) {
				localPath = filepath.Join(h.notesPathPrefix, link.FilePath[len(devicePrefix):])
			}
			return map[string]interface{}{"Path": localPath, "Page": link.Page}
		},
	}

	tmpl, err := template.New("").Funcs(funcMap).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		panic(fmt.Sprintf("failed to parse templates: %v", err))
	}
	h.tmpl = tmpl

	h.mux.HandleFunc("GET /setup", h.handleSetup)
	h.mux.HandleFunc("POST /setup/save", h.handleSetupSave)
	h.mux.HandleFunc("GET /{$}", h.handleIndex)
	h.mux.HandleFunc("POST /tasks", h.handleCreateTask)
	h.mux.HandleFunc("POST /tasks/{id}/complete", h.handleCompleteTask)
	h.mux.HandleFunc("POST /tasks/bulk", h.handleBulkAction)
	h.mux.HandleFunc("POST /tasks/purge-completed", h.handlePurgeCompleted)
	h.mux.HandleFunc("POST /tasks/purge-deleted", h.handlePurgeDeleted)
	h.mux.HandleFunc("GET /logs", h.handleLogs)
	h.mux.HandleFunc("GET /settings", h.handleSettings)
	h.mux.HandleFunc("GET /settings/{group}", h.handleSettingsGroup)
	h.mux.HandleFunc("POST /settings/save", h.handleSettingsSave)
	h.mux.HandleFunc("POST /settings/backfill-embeddings", h.handleBackfillEmbeddings)

	if h.noteDB != nil {
		h.mux.HandleFunc("POST /settings/mcp-tokens/create", h.handleMCPTokenCreate)
		h.mux.HandleFunc("POST /settings/mcp-tokens/revoke", h.handleMCPTokenRevoke)
	}

	// Registered unconditionally; the handlers 404 when no SyncDeviceService is
	// wired (it arrives via SetSyncDeviceService after construction, so route
	// registration can't be gated on it).
	h.mux.HandleFunc("POST /settings/sync-devices/prune", h.handleSyncDevicePrune)
	h.mux.HandleFunc("POST /settings/sync-devices/compact", h.handleSyncDeviceCompact)

	h.mux.HandleFunc("GET /files", h.handleFiles)
	h.mux.HandleFunc("GET /files/supernote", h.handleFilesSupernote)
	h.mux.HandleFunc("GET /files/boox", h.handleFilesBoox)
	h.mux.HandleFunc("GET /files/forestnote", h.handleFilesForestNote)
	h.mux.HandleFunc("GET /files/remarkable", h.handleFilesRemarkable)
	h.mux.HandleFunc("GET /files/forestnote/render", h.handleForestNoteRender)
	h.mux.HandleFunc("POST /files/forestnote/delete", h.handleForestNoteDelete)
	h.mux.HandleFunc("POST /files/forestnote/reprocess", h.handleForestNoteReprocess)
	h.mux.HandleFunc("GET /files/forestnote/export", h.handleForestNoteExport)
	h.mux.HandleFunc("GET /digests", h.handleDigests)
	h.mux.HandleFunc("GET /digests/{id}", h.handleDigestDetail)
	h.mux.HandleFunc("GET /digests/{id}/render", h.handleDigestRender)
	h.mux.HandleFunc("DELETE /digests/{id}", h.handleDeleteDigest)
	h.mux.HandleFunc("GET /search", h.handleSearch)
	h.mux.HandleFunc("POST /files/queue", h.handleFilesQueue)
	h.mux.HandleFunc("POST /files/skip", h.handleFilesSkip)
	h.mux.HandleFunc("POST /files/unskip", h.handleFilesUnskip)
	h.mux.HandleFunc("POST /files/force", h.handleFilesForce)
	h.mux.HandleFunc("GET /files/status", h.handleFilesStatus)
	h.mux.HandleFunc("GET /files/history", h.handleFilesHistory)
	h.mux.HandleFunc("GET /files/render", h.handleFilesRender)
	h.mux.HandleFunc("GET /files/boox/render", h.handleBooxRender)
	h.mux.HandleFunc("GET /files/boox/versions", h.handleBooxVersions)
	h.mux.HandleFunc("POST /processor/supernote/start", h.handleProcessorStart)
	h.mux.HandleFunc("POST /processor/supernote/stop", h.handleProcessorStop)
	h.mux.HandleFunc("POST /processor/boox/start", h.handleBooxProcessorStart)
	h.mux.HandleFunc("POST /processor/boox/stop", h.handleBooxProcessorStop)
	h.mux.HandleFunc("POST /files/scan", h.handleFilesScan)
	h.mux.HandleFunc("POST /files/import", h.handleFilesImport)
	h.mux.HandleFunc("POST /files/retry-failed", h.handleFilesRetryFailed)
	h.mux.HandleFunc("POST /files/delete-note", h.handleFilesDeleteNote)
	h.mux.HandleFunc("POST /files/delete-bulk", h.handleFilesDeleteBulk)
	h.mux.HandleFunc("POST /files/migrate-imports", h.handleFilesMigrateImports)
	h.mux.HandleFunc("POST /files/move", h.handleFilesMove)
	h.mux.HandleFunc("POST /files/move-bulk", h.handleFilesMoveBulk)
	h.mux.HandleFunc("POST /maintenance/boox/reconcile-dates", h.handleMaintenanceBooxReconcileDates)
	h.mux.HandleFunc("POST /maintenance/boox/delete-untitled", h.handleMaintenanceBooxDeleteUntitled)
	h.mux.HandleFunc("POST /maintenance/boox/scan-untracked", h.handleMaintenanceBooxScanUntracked)

	h.registerLogStreamHandler(broadcaster)

	h.mux.HandleFunc("GET /api/search", func(w http.ResponseWriter, r *http.Request) {
		if h.search == nil || !h.search.HasEmbeddingPipeline() {
			http.NotFound(w, r)
			return
		}
		h.handleAPISearch(w, r)
	})
	h.mux.HandleFunc("GET /api/notes/pages", h.handleAPIGetPages)
	h.mux.HandleFunc("GET /api/notes/pages/image", h.handleAPIGetImage)
	h.mux.HandleFunc("GET /api/forestnote/text-boxes", h.handleAPIForestNoteTextBoxes)
	h.mux.HandleFunc("POST /api/forestnote/text-boxes/edit", h.handleAPIForestNoteEditTextBox)

	if h.noteDB != nil {
		h.mux.HandleFunc("GET /api/config", h.handleGetConfig)
		h.mux.HandleFunc("PUT /api/config", h.handlePutConfig)
		h.mux.HandleFunc("GET /api/sources", h.handleListSources)
		h.mux.HandleFunc("POST /api/sources", h.handleAddSource)
		h.mux.HandleFunc("PUT /api/sources/{id}", h.handleUpdateSource)
		h.mux.HandleFunc("DELETE /api/sources/{id}", h.handleDeleteSource)
	}

	h.mux.HandleFunc("GET /chat", h.handleChat)
	h.mux.HandleFunc("POST /chat/ask", h.handleAsk)
	h.mux.HandleFunc("GET /chat/sessions", h.handleChatSessions)
	h.mux.HandleFunc("GET /chat/messages", h.handleChatMessages)

	h.RegisterAPIv1()

	subFS, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(subFS))

	h.mux.Handle("GET /manifest.json", fileServer)
	h.mux.Handle("GET /sw.js", fileServer)
	h.mux.Handle("GET /htmx.min.js", fileServer)
	h.mux.Handle("GET /static/", http.StripPrefix("/static/", fileServer))
	h.mux.Handle("GET /erb.png", fileServer)

	return h
}

// SetDigestService wires the optional Digests read surface (Phase D2). Set from
// main only in SPC server mode; when unset the Digests tab and nav entry hide.
// A setter (not a constructor arg) keeps the many NewHandler call sites stable.
func (h *Handler) SetDigestService(d service.DigestService) {
	h.digests = d
}

// SetSPCFileRoot wires the UB-as-SPC file-tree root used to resolve a digest's
// device-relative SourcePath into an on-disk note for the digest render route.
// Set from main alongside SetDigestService (both SPC-server-mode only); a setter
// keeps the NewHandler call sites stable.
func (h *Handler) SetSPCFileRoot(root string) {
	h.spcFileRoot = root
}

// SetSyncDeviceService wires the ForestNote sync device-management surface. Set
// from main only when a ForestNote source is active; when unset the Sync
// Devices settings card hides and its routes 404. A setter (not a constructor
// arg) keeps the NewHandler call sites stable, mirroring SetDigestService.
func (h *Handler) SetSyncDeviceService(s service.SyncDeviceService) {
	h.syncDevices = s
}

func (h *Handler) SetRemarkableDeviceService(s service.RemarkableDeviceService) {
	h.rmDevices = s
}

func (h *Handler) renderTemplate(w http.ResponseWriter, r *http.Request, name string, data map[string]interface{}) {
	if data == nil {
		data = h.baseTemplateData(r.Context())
	} else {
		base := h.baseTemplateData(r.Context())
		for k, v := range base {
			if _, ok := data[k]; !ok {
				data[k] = v
			}
		}
	}
	if _, ok := data["activeTab"]; !ok {
		data["activeTab"] = name
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Clone to avoid race condition when defining "content" template
	t, err := h.tmpl.Clone()
	if err != nil {
		h.logger.Error("failed to clone template", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Define "content" as the specific fragment being rendered
	fragmentPath := "templates/" + name + ".html"
	content, err := templateFS.ReadFile(fragmentPath)
	if err != nil {
		h.logger.Error("failed to read fragment", "path", fragmentPath, "error", err)
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}

	_, err = t.New("content").Parse(string(content))
	if err != nil {
		h.logger.Error("failed to parse fragment", "name", name, "error", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		t.ExecuteTemplate(w, "content", data)
		return
	}
	t.ExecuteTemplate(w, "layout.html", data)
}

// renderFragment executes a named, pre-parsed template block (e.g. "_task_row")
// without the layout shell. It Clones h.tmpl before executing so that h.tmpl
// remains Clone-able: html/template permanently locks a template tree against
// future Clones once ExecuteTemplate has run on it, and renderTemplate relies
// on Clone per request.
func (h *Handler) renderFragment(w http.ResponseWriter, r *http.Request, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	t, err := h.tmpl.Clone()
	if err != nil {
		h.logger.Error("failed to clone template for fragment", "name", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		h.logger.Error("failed to execute fragment", "name", name, "error", err)
	}
}

func (h *Handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	h.renderTemplate(w, r, "tasks", nil)
}

// handleFiles is the legacy /files entry point. It now redirects to the
// appropriate source-specific tab, preserving any query string so existing
// bookmarks like /files?path=Moffitt land on /files/supernote?path=Moffitt.
// When neither source is configured, it renders a combined empty-state
// placeholder so the user sees "configure a source in Settings" rather than
// a 404.
func (h *Handler) handleFiles(w http.ResponseWriter, r *http.Request) {
	query := ""
	if r.URL.RawQuery != "" {
		query = "?" + r.URL.RawQuery
	}
	switch {
	case h.notes.HasSupernoteSource():
		http.Redirect(w, r, "/files/supernote"+query, http.StatusSeeOther)
	case h.notes.HasBooxSource():
		http.Redirect(w, r, "/files/boox"+query, http.StatusSeeOther)
	case h.notes.HasForestNoteSource():
		http.Redirect(w, r, "/files/forestnote"+query, http.StatusSeeOther)
	case h.notes.HasRemarkableSource():
		http.Redirect(w, r, "/files/remarkable"+query, http.StatusSeeOther)
	default:
		data := map[string]interface{}{
			"activeTab":  "files",
			"filesError": "No note sources configured. Add a source in Settings.",
		}
		h.renderTemplate(w, r, "files_supernote", data)
	}
}

// handleFilesSupernote renders the Supernote-only Files view. Mirrors the
// directory/breadcrumb browser model of the legacy /files route but excludes
// Boox notes entirely.
func (h *Handler) handleFilesSupernote(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	data := map[string]interface{}{"activeTab": "files-supernote"}
	if !h.notes.HasSupernoteSource() {
		data["filesError"] = "No Supernote source configured. Add a source in Settings."
		h.renderTemplate(w, r, "files_supernote", data)
		return
	}
	if detail := r.URL.Query().Get("detail"); detail != "" {
		if !h.validNotePath(detail) {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		back := r.URL.Query().Get("back")
		dv, err := h.buildNoteDetail(ctx, detail, filepath.Base(detail),
			"/files/supernote?path="+url.QueryEscape(back), "", nil)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		data["detail"] = dv
		h.renderTemplate(w, r, "files_supernote", data)
		return
	}
	rawPath := r.URL.Query().Get("path")
	relPath, ok := safeRelPath(rawPath)
	if !ok {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	sortField, sortOrder := r.URL.Query().Get("sort"), r.URL.Query().Get("order")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if perPage <= 0 {
		perPage = 25
	}
	if page <= 0 {
		page = 1
	}

	files, total, err := h.notes.ListSupernoteFiles(ctx, relPath, sortField, sortOrder, page, perPage)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	bs := buildBreadcrumbs(relPath)
	data["files"], data["relPath"], data["breadcrumbs"], data["filesTotalFiles"] = files, relPath, bs, total
	data["navCrumbs"] = supernoteCrumbs(bs)
	data["filesPage"], data["filesPerPage"] = page, perPage
	data["filesSort"], data["filesOrder"] = sortField, sortOrder
	totalPages := (total + perPage - 1) / perPage
	if totalPages == 0 {
		totalPages = 1
	}
	data["filesTotalPages"] = totalPages
	data["pager"] = pager("/files/supernote", page, totalPages, map[string]string{"path": relPath})
	data["pipelinePanel"] = pipelinePanel{Source: "supernote", StartStop: true}
	data["syncModel"] = source.SyncModelFor("supernote")
	h.renderTemplate(w, r, "files_supernote", data)
}

// handleFilesBoox renders the Boox-only Files view. Flat catalog list — no
// directory navigation — with per-note Title/Folder/Device/NoteType/Pages
// columns surfaced from BooxNoteSummary.
func (h *Handler) handleFilesBoox(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	data := map[string]interface{}{"activeTab": "files-boox"}
	if !h.notes.HasBooxSource() {
		data["filesError"] = "No Boox source configured. Add a source in Settings."
		h.renderTemplate(w, r, "files_boox", data)
		return
	}
	if detail := r.URL.Query().Get("detail"); detail != "" {
		if !h.validNotePath(detail) {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		title := filepath.Base(detail)
		if note, err := h.notes.GetBooxNote(ctx, detail); err == nil && note.Title != "" {
			title = note.Title
		}
		delAction := detailAction{
			Label:   "✗ Delete",
			Danger:  true,
			HxPost:  "/files/delete-note",
			Vals:    mustJSON(map[string]string{"path": detail}),
			Confirm: "Delete this note, all job records, and cached renders?",
			OnAfter: "if(event.detail.successful){window.location='/files/boox';}",
		}
		dv, err := h.buildNoteDetail(ctx, detail, title, "/files/boox",
			"/files/boox/versions?path="+url.QueryEscape(detail), []detailAction{delAction})
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		data["detail"] = dv
		h.renderTemplate(w, r, "files_boox", data)
		return
	}
	sortField, sortOrder := r.URL.Query().Get("sort"), r.URL.Query().Get("order")
	folder := r.URL.Query().Get("folder")
	device := r.URL.Query().Get("device")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if perPage <= 0 {
		perPage = 25
	}
	if page <= 0 {
		page = 1
	}

	rows, total, err := h.notes.ListBooxNotes(ctx, device, folder, sortField, sortOrder, page, perPage)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	folders, err := h.notes.ListBooxFolders(ctx)
	if err != nil {
		// Non-fatal: the folder-filter row is a convenience. Log and continue
		// so the file list still renders.
		h.logger.Error("list boox folders", "error", err)
	}
	devices, err := h.notes.ListBooxDevices(ctx)
	if err != nil {
		h.logger.Error("list boox devices", "error", err)
	}
	data["booxNotes"], data["filesTotalFiles"] = rows, total
	data["booxFolders"], data["booxFolderFilter"] = folders, folder
	data["unfiledFolderSentinel"] = service.FolderFilterUnfiled
	data["booxDevices"], data["booxDeviceFilter"] = devices, device
	data["filesPage"], data["filesPerPage"] = page, perPage
	data["filesSort"], data["filesOrder"] = sortField, sortOrder
	totalPages := (total + perPage - 1) / perPage
	if totalPages == 0 {
		totalPages = 1
	}
	data["filesTotalPages"] = totalPages
	data["pager"] = pager("/files/boox", page, totalPages, map[string]string{"folder": folder, "device": device})
	data["pipelinePanel"] = pipelinePanel{Source: "boox", StartStop: true}
	data["syncModel"] = source.SyncModelFor("boox")
	h.renderTemplate(w, r, "files_boox", data)
}

// handleFilesForestNote renders the ForestNote Files tab. ForestNote has no
// filesystem; the inventory is a live projection of the syncstore mirror, and
// page images render on the fly (see handleForestNoteRender). Two modes:
//   - ?notebook=<id> → the enriched detail view (page thumbnails + OCR text +
//     metadata header + actions);
//   - otherwise ?folder=<id> (default "") → a Supernote-style table of that
//     folder's subfolders + notebooks, with a breadcrumb trail.
func (h *Handler) handleFilesForestNote(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	data := map[string]interface{}{"activeTab": "files-forestnote"}
	if !h.notes.HasForestNoteSource() {
		data["filesError"] = "No ForestNote source configured. Enable device sync in Settings."
		h.renderTemplate(w, r, "files_forestnote", data)
		return
	}

	if nb := r.URL.Query().Get("notebook"); nb != "" {
		detail, err := h.notes.GetForestNoteNotebookDetail(ctx, nb)
		if err != nil {
			http.Error(w, "notebook not found", http.StatusNotFound)
			return
		}
		data["detail"] = forestNoteDetailView(detail, r.URL.Query().Get("page"))
		h.renderTemplate(w, r, "files_forestnote", data)
		return
	}

	folderID := r.URL.Query().Get("folder")
	sortField, sortOrder := r.URL.Query().Get("sort"), r.URL.Query().Get("order")
	crumbs, entries, err := h.notes.ListForestNoteFolder(ctx, folderID, sortField, sortOrder)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data["fnEntries"], data["fnCrumbs"], data["fnFolderID"] = entries, crumbs, folderID
	data["navCrumbs"] = forestNoteCrumbs(crumbs)
	data["filesSort"], data["filesOrder"] = sortField, sortOrder
	data["pipelinePanel"] = pipelinePanel{Note: "Re-OCR is per-notebook — open a notebook to reprocess it."}
	data["syncModel"] = source.SyncModelFor("forestnote")
	h.renderTemplate(w, r, "files_forestnote", data)
}

// handleFilesRemarkable renders the metadata-only reMarkable Files tab. Real
// page rendering/OCR are intentionally left to later chunks; this establishes
// the first-class source structure and stable remarkable:// paths.
func (h *Handler) handleFilesRemarkable(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	data := map[string]interface{}{"activeTab": "files-remarkable"}
	if !h.notes.HasRemarkableSource() {
		data["filesError"] = "No reMarkable source configured. Add a source in Settings."
		h.renderTemplate(w, r, "files_remarkable", data)
		return
	}

	if docID := r.URL.Query().Get("document"); docID != "" {
		detail, err := h.notes.GetRemarkableDocumentDetail(ctx, docID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "document not found", http.StatusNotFound)
				return
			}
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		data["detail"] = detail
		data["syncModel"] = source.SyncModelFor("remarkable")
		h.renderTemplate(w, r, "files_remarkable", data)
		return
	}

	folderID := r.URL.Query().Get("folder")
	sortField, sortOrder := r.URL.Query().Get("sort"), r.URL.Query().Get("order")
	crumbs, entries, err := h.notes.ListRemarkableFolder(ctx, folderID, sortField, sortOrder)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "folder not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data["rmEntries"], data["rmCrumbs"], data["rmFolderID"] = entries, crumbs, folderID
	data["filesSort"], data["filesOrder"] = sortField, sortOrder
	data["syncModel"] = source.SyncModelFor("remarkable")
	h.renderTemplate(w, r, "files_remarkable", data)
}

// handleForestNoteDelete soft-deletes a notebook (UB-local) and de-indexes its
// pages. HX requests get an empty 200 (the row swaps out); others land back on
// the folder the delete came from.
func (h *Handler) handleForestNoteDelete(w http.ResponseWriter, r *http.Request) {
	nb := r.FormValue("notebook")
	if nb == "" {
		http.Error(w, "missing notebook", http.StatusBadRequest)
		return
	}
	if err := h.notes.DeleteForestNoteNotebook(r.Context(), nb); err != nil {
		http.Error(w, "failed to delete notebook", http.StatusInternalServerError)
		return
	}
	h.respondEmptyOrRedirect(w, r, forestNoteFolderURL(r.FormValue("back")))
}

// handleForestNoteReprocess re-enqueues a notebook's pages for re-OCR/re-index.
// Fire-and-forget: the work runs async on the sync bridge.
func (h *Handler) handleForestNoteReprocess(w http.ResponseWriter, r *http.Request) {
	nb := r.FormValue("notebook")
	if nb == "" {
		http.Error(w, "missing notebook", http.StatusBadRequest)
		return
	}
	if err := h.notes.ReprocessForestNoteNotebook(r.Context(), nb); err != nil {
		http.Error(w, "failed to reprocess notebook", http.StatusInternalServerError)
		return
	}
	h.respondEmptyOrRedirect(w, r, "/files/forestnote?notebook="+url.QueryEscape(nb))
}

// handleForestNoteExport streams a notebook's pages as a single PDF.
func (h *Handler) handleForestNoteExport(w http.ResponseWriter, r *http.Request) {
	nb := r.URL.Query().Get("notebook")
	if nb == "" {
		http.Error(w, "missing notebook", http.StatusBadRequest)
		return
	}
	stream, filename, err := h.notes.ExportForestNoteNotebookPDF(r.Context(), nb)
	if err != nil {
		http.Error(w, "export failed", http.StatusInternalServerError)
		return
	}
	defer stream.Close()
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
	io.Copy(w, stream)
}

// forestNoteFolderURL builds the folder-browse URL for a (possibly empty) folder.
func forestNoteFolderURL(folderID string) string {
	if folderID == "" {
		return "/files/forestnote"
	}
	return "/files/forestnote?folder=" + url.QueryEscape(folderID)
}

// handleForestNoteRender streams a ForestNote page as JPEG, rendered on the fly
// from the syncstore mirror. Path is forestnote://{notebook_id}/{page_id}; the
// page index is carried in the path, so no page query param is needed.
func (h *Handler) handleForestNoteRender(w http.ResponseWriter, r *http.Request) {
	notePath := r.URL.Query().Get("path")
	if !fnpath.Is(notePath) {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	stream, ct, err := h.notes.RenderPage(r.Context(), notePath, 0)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer stream.Close()
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=300")
	io.Copy(w, stream)
}

// handleDigests renders the Digests tab: the Supernote "summary" excerpts
// synced from the device. Flat list with optional group/tag filter pills.
func (h *Handler) handleDigests(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	data := map[string]interface{}{"activeTab": "digests"}
	if h.digests == nil {
		data["digestsError"] = "Digests sync is only available when the UB-as-SPC device sync server is enabled."
		h.renderTemplate(w, r, "digests", data)
		return
	}

	group := r.URL.Query().Get("group")
	tag := r.URL.Query().Get("tag")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if perPage <= 0 {
		perPage = 25
	}
	if page <= 0 {
		page = 1
	}

	rows, total, err := h.digests.ListDigests(ctx, group, tag, page, perPage)
	if err != nil {
		h.logger.Error("list digests", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	groups, err := h.digests.ListGroups(ctx)
	if err != nil {
		// Non-fatal: the group-filter pills are a convenience.
		h.logger.Error("list digest groups", "error", err)
	}
	data["digests"], data["digestsTotal"] = rows, total
	data["digestGroups"], data["digestGroupFilter"] = groups, group
	data["digestTagFilter"] = tag
	data["filesPage"], data["filesPerPage"] = page, perPage
	totalPages := (total + perPage - 1) / perPage
	if totalPages == 0 {
		totalPages = 1
	}
	data["filesTotalPages"] = totalPages
	data["pager"] = pager("/digests", page, totalPages, map[string]string{"group": group, "tag": tag})
	h.renderTemplate(w, r, "digests", data)
}

// handleDeleteDigest soft-deletes a digest and propagates the delete to the
// device (D2 tombstone). On HX it returns an empty 200 so the row swaps out
// (hx-target="closest tr"); non-HX redirects back to the tab.
func (h *Handler) handleDeleteDigest(w http.ResponseWriter, r *http.Request) {
	if h.digests == nil {
		http.NotFound(w, r) // digest sync disabled (no SPC server)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad digest id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := h.digests.DeleteDigest(ctx, id); err != nil {
		if errors.Is(err, digeststore.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.logger.Error("delete digest", "id", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.respondEmptyOrRedirect(w, r, "/digests")
}

// handleDigestDetail renders the in-tab detail page for one digest: full
// (untruncated) excerpt, the OCR'd handwriting comment, group/tags/dates, and
// a rendered image of the source page the excerpt came from.
func (h *Handler) handleDigestDetail(w http.ResponseWriter, r *http.Request) {
	if h.digests == nil {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad digest id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	d, err := h.digests.GetDigest(ctx, id)
	if err != nil {
		if errors.Is(err, digeststore.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.logger.Error("get digest", "id", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// A source image is available only for Note digests whose source .note we
	// can resolve under the SPC file root (PDF digests render text-only).
	hasImage := d.SourceType == 2 && h.digestSourceAbsPath(d.SourcePath) != ""
	data := map[string]interface{}{
		"activeTab": "digests",
		"digest":    d,
		"hasImage":  hasImage,
	}
	h.renderTemplate(w, r, "digest_detail", data)
}

// handleDigestRender streams the source note page a digest excerpt came from,
// reusing the Supernote render path. Baseline: Note digests only (the .mark
// handwriting blob — RATTA_RLE — is not yet decoded; see the Phase 4 plan).
func (h *Handler) handleDigestRender(w http.ResponseWriter, r *http.Request) {
	if h.digests == nil {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad digest id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	d, err := h.digests.GetDigest(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	abs := h.digestSourceAbsPath(d.SourcePath)
	if d.SourceType != 2 || abs == "" {
		http.NotFound(w, r) // PDF digest, or source not resolvable/present
		return
	}
	// NOTE: NotePage is treated as a 0-based ordinal (see service.parseNotePage);
	// if hardware shows the device numbers pages from 1, subtract one here.
	// RenderSupernotePage (not RenderPage) so an SPC-server-only deployment with
	// a Boox source but no filesystem Supernote source doesn't misroute this
	// .note to the Boox renderer.
	stream, contentType, err := h.notes.RenderSupernotePage(ctx, abs, d.NotePage)
	if err != nil {
		h.logger.Error("digest render", "id", id, "path", abs, "page", d.NotePage, "error", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	defer stream.Close()
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=300")
	io.Copy(w, stream)
}

// digestSourceAbsPath resolves a digest's device-relative SourcePath (e.g.
// "NOTE/Note/foo.note") to an absolute path under the SPC file root, or "" if
// the root is unset, the path escapes the root, or the file is missing.
func (h *Handler) digestSourceAbsPath(sourcePath string) string {
	if h.spcFileRoot == "" || sourcePath == "" {
		return ""
	}
	rel, ok := safeRelPath(strings.TrimPrefix(sourcePath, "/"))
	if !ok {
		return ""
	}
	abs := filepath.Join(h.spcFileRoot, rel)
	if fi, err := os.Stat(abs); err != nil || fi.IsDir() {
		return ""
	}
	return abs
}

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{"activeTab": "search"}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	opts, selectedSources, submitted := h.searchOptionsFromRequest(r)
	data["searchQuery"] = query
	data["searchFolder"] = opts.Folder
	data["searchSort"] = opts.Sort
	data["searchCreatedFrom"] = r.URL.Query().Get("created_from")
	data["searchCreatedTo"] = r.URL.Query().Get("created_to")
	data["searchModifiedFrom"] = r.URL.Query().Get("modified_from")
	data["searchModifiedTo"] = r.URL.Query().Get("modified_to")
	data["searchSelectedLocations"] = selectedLocations(r.URL.Query()["location"])
	// Echo selections back so the facet checkboxes stay checked across submits.
	selected := map[string]bool{}
	for _, s := range selectedSources {
		selected[s] = true
	}
	data["searchSources"] = selected
	if h.notes != nil {
		if locations, err := h.notes.ListSearchLocations(r.Context()); err == nil {
			data["searchLocations"] = locations
		} else {
			h.logger.Warn("search locations unavailable", "error", err)
		}
	}
	if submitted && len(opts.Sources) == 0 {
		data["searchError"] = "Select at least one source to search."
		h.renderTemplate(w, r, "search", data)
		return
	}
	if query != "" {
		results, _ := h.search.SearchAdvanced(r.Context(), query, opts)
		data["searchResults"] = results
	}
	h.renderTemplate(w, r, "search", data)
}

func (h *Handler) searchOptionsFromRequest(r *http.Request) (service.SearchOptions, []string, bool) {
	q := r.URL.Query()
	submitted := q.Get("sources_submitted") == "1"
	sources := q["source"]
	if !submitted && len(sources) == 0 {
		sources = h.enabledSearchSources()
	}
	opts := service.SearchOptions{
		Folder:       strings.TrimSpace(q.Get("folder")),
		Sources:      sources,
		Sort:         normalizeSearchSort(q.Get("sort")),
		CreatedFrom:  parseSearchDateStart(q.Get("created_from")),
		CreatedTo:    parseSearchDateEnd(q.Get("created_to")),
		ModifiedFrom: parseSearchDateStart(q.Get("modified_from")),
		ModifiedTo:   parseSearchDateEnd(q.Get("modified_to")),
	}
	if raw := q.Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			opts.Limit = n
		}
	}
	for _, raw := range q["location"] {
		if loc, ok := service.ParseSearchLocation(raw); ok {
			opts.Locations = append(opts.Locations, loc)
		}
	}
	return opts, sources, submitted
}

func (h *Handler) enabledSearchSources() []string {
	var out []string
	if h.notes != nil {
		if h.notes.HasSupernoteSource() {
			out = append(out, rag.SourceSupernote)
		}
		if h.notes.HasBooxSource() {
			out = append(out, rag.SourceBoox)
		}
		if h.notes.HasForestNoteSource() {
			out = append(out, rag.SourceForestNote)
		}
		if h.notes.HasRemarkableSource() {
			out = append(out, rag.SourceRemarkable)
		}
	}
	if h.digests != nil {
		out = append(out, rag.SourceDigest)
	}
	return out
}

func normalizeSearchSort(raw string) string {
	switch raw {
	case "date_asc", "date_desc":
		return raw
	default:
		return "relevance"
	}
}

func parseSearchDateStart(raw string) time.Time {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func parseSearchDateEnd(raw string) time.Time {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}
	}
	return t.Add(24*time.Hour - time.Nanosecond).UTC()
}

func selectedLocations(values []string) map[string]bool {
	out := map[string]bool{}
	for _, v := range values {
		out[v] = true
	}
	return out
}

func (h *Handler) handleChat(w http.ResponseWriter, r *http.Request) {
	sessions, _ := h.search.ListSessions(r.Context())
	h.renderTemplate(w, r, "chat", map[string]interface{}{"chatSessions": sessions})
}

func (h *Handler) handleLogs(w http.ResponseWriter, r *http.Request) {
	h.renderTemplate(w, r, "logs", nil)
}

// forestNoteSourceRow returns the (first) ForestNote source row, if one is configured. The
// relay-log compaction toggle lives in this row's config_json (the source reads it at Start),
// so both the Settings prefill and the save path need it. ok=false when no ForestNote source
// exists yet (the toggle is hidden — there's nothing to compact).
func (h *Handler) forestNoteSourceRow(ctx context.Context) (source.SourceRow, bool) {
	return h.sourceRowByType(ctx, "forestnote")
}

func (h *Handler) sourceRowByType(ctx context.Context, wantType string) (source.SourceRow, bool) {
	listed, err := h.config.ListSources(ctx)
	if err != nil {
		return source.SourceRow{}, false
	}
	rows, ok := listed.([]source.SourceRow)
	if !ok {
		return source.SourceRow{}, false
	}
	for _, row := range rows {
		if row.Type == wantType {
			return row, true
		}
	}
	return source.SourceRow{}, false
}

// sourceConfigBool reads a named bool key out of a source config_json blob, defaulting to false
// when the blob is empty, unparseable, or the key is absent/non-bool. Used to prefill the
// compaction toggle from the ForestNote source row without imposing the source's Config struct.
func sourceConfigBool(configJSON, key string) bool {
	if configJSON == "" {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(configJSON), &m); err != nil {
		return false
	}
	b, _ := m[key].(bool)
	return b
}

// setSourceConfigBool merges key=val into the source row's config_json — preserving every other
// key (batch_limit, compaction_interval_sec, …) by round-tripping through a map — and persists it
// via UpdateSource (which flags RestartRequired). The running source re-reads config_json only at
// Start, so the change takes effect on the next app restart.
func (h *Handler) setSourceConfigBool(ctx context.Context, row source.SourceRow, key string, val bool) error {
	m := map[string]any{}
	if row.ConfigJSON != "" {
		if err := json.Unmarshal([]byte(row.ConfigJSON), &m); err != nil {
			return fmt.Errorf("parse config_json: %w", err)
		}
	}
	m[key] = val
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal config_json: %w", err)
	}
	row.ConfigJSON = string(b)
	return h.config.UpdateSource(ctx, fmt.Sprint(row.ID), &row)
}

// settingsGroups is the canonical ordered set of Settings sub-pages.
var settingsGroups = []string{"devices", "ai", "integrations", "system"}

func validSettingsGroup(g string) bool {
	for _, s := range settingsGroups {
		if s == g {
			return true
		}
	}
	return false
}

// handleSettings redirects the legacy /settings entry point to the default
// Settings group. The query string is preserved — flash params like
// ?new_token= (MCP token one-time display) must survive the hop.
func (h *Handler) handleSettings(w http.ResponseWriter, r *http.Request) {
	target := "/settings/devices"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleSettingsGroup renders one Settings group. Unknown groups fall back to
// the default (a clean 303, never a 500).
func (h *Handler) handleSettingsGroup(w http.ResponseWriter, r *http.Request) {
	group := r.PathValue("group")
	if !validSettingsGroup(group) {
		http.Redirect(w, r, "/settings/devices", http.StatusSeeOther)
		return
	}
	data := h.settingsData(r)
	data["activeTab"] = "settings-" + group
	data["SettingsGroup"] = group
	h.renderTemplate(w, r, "settings_"+group, data)
}

// settingsData builds the Settings page's template data. Extracted from
// handleSettings so mutation handlers that re-render the page on HX (sync
// device prune/compact) can add their own flash keys on top.
func (h *Handler) settingsData(r *http.Request) map[string]interface{} {
	ctx := r.Context()
	cfg, _ := h.config.GetConfig(ctx)
	srcs, _ := h.config.ListSources(ctx)
	data := map[string]interface{}{"Config": cfg, "Sources": srcs, "activeTab": "settings"}
	if h.syncDevices != nil {
		data["SyncDevicesEnabled"] = true
		devices, err := h.syncDevices.ListSyncDevices(ctx)
		if err != nil {
			h.logger.Error("failed to list sync devices", "error", err)
		}
		data["SyncDevices"] = devices
	}
	if h.rmDevices != nil {
		data["RemarkableDevicesEnabled"] = true
		devices, err := h.rmDevices.ListDevices(ctx)
		if err != nil {
			h.logger.Error("failed to list remarkable devices", "error", err)
		}
		data["RemarkableDevices"] = devices
	}
	if h.noteDB != nil {
		tokens, _ := mcpauth.ListTokens(ctx, h.noteDB)
		data["MCPTokens"], data["MCPTokensEnabled"] = tokens, true

		// Populate Boox-specific runtime settings. The Settings template
		// references these as top-level fields (not Config.X) because
		// they're stored in the settings table but not in the Config
		// struct — read them on demand so the form fields render with
		// current values.
		data["SNPipelineActive"] = h.notes != nil && h.notes.HasSupernoteSource()
		data["BooxActive"] = h.notes != nil && h.notes.HasBooxSource()
		if _, ok := h.sourceRowByType(ctx, "remarkable"); ok {
			data["RemarkableSourceActive"] = true
		}
		fnOCRPrompt, _ := notedb.GetSetting(ctx, h.noteDB, appconfig.KeyForestNoteOCRPrompt)
		data["ForestNoteOCRPrompt"] = fnOCRPrompt
		// Relay-log compaction lives in the ForestNote source row's config_json (not appconfig);
		// surface whether a source row exists (to gate the checkbox) and its current state (to
		// prefill it). Read here so the save handler doesn't have to round-trip on every render.
		if row, ok := h.forestNoteSourceRow(ctx); ok {
			data["ForestNoteSourceActive"] = true
			data["ForestNoteCompaction"] = sourceConfigBool(row.ConfigJSON, "compaction")
		}
		ocrPrompt, _ := notedb.GetSetting(ctx, h.noteDB, appconfig.KeyBooxOCRPrompt)
		todoEnabled, _ := notedb.GetSetting(ctx, h.noteDB, appconfig.KeyBooxTodoEnabled)
		todoPrompt, _ := notedb.GetSetting(ctx, h.noteDB, appconfig.KeyBooxTodoPrompt)
		importNotes, _ := notedb.GetSetting(ctx, h.noteDB, appconfig.KeyBooxImportNotes)
		importPDFs, _ := notedb.GetSetting(ctx, h.noteDB, appconfig.KeyBooxImportPDFs)
		importOnyx, _ := notedb.GetSetting(ctx, h.noteDB, appconfig.KeyBooxImportOnyxPaths)
		extBaseURL, _ := notedb.GetSetting(ctx, h.noteDB, appconfig.KeyBooxExternalBaseURL)
		data["BooxOCRPrompt"] = ocrPrompt
		data["BooxTodoEnabled"] = todoEnabled == "true"
		data["BooxTodoPrompt"] = todoPrompt
		data["BooxImportNotes"] = importNotes == "true"
		data["BooxImportPDFs"] = importPDFs == "true"
		data["BooxImportOnyxPaths"] = importOnyx == "true"
		data["BooxExternalBaseURL"] = extBaseURL
	}
	if nt := r.URL.Query().Get("new_token"); nt != "" {
		data["NewMCPToken"] = nt
	}
	if mcpCfg, ok := cfg.(*appconfig.Config); ok && mcpCfg != nil && mcpCfg.MCPPort > 0 {
		host := r.Host
		if colon := strings.LastIndex(host, ":"); colon >= 0 && !strings.Contains(host[colon:], "]") {
			host = host[:colon]
		}
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		data["MCPEnabled"] = true
		data["MCPHTTPURL"] = fmt.Sprintf("%s://%s:%d/sse", scheme, host, mcpCfg.MCPPort)
		data["MCPStdioCommand"] = "docker exec -i ub-mcp ub-mcp"
	}
	return data
}

// handleSyncDevicePrune deletes a ForestNote sync device's registry row (spec
// §4.3 prune: cleanup only — its authored ops stay, and a still-alive device
// re-registers on its next sync). 404s when no ForestNote source is wired.
func (h *Handler) handleSyncDevicePrune(w http.ResponseWriter, r *http.Request) {
	if h.syncDevices == nil {
		http.NotFound(w, r)
		return
	}
	siteID := strings.TrimSpace(r.FormValue("site_id"))
	if !syncstore.IsULID(siteID) {
		http.Error(w, "site_id must be a ULID", http.StatusBadRequest)
		return
	}
	if err := h.syncDevices.PruneSyncDevice(r.Context(), siteID); err != nil {
		if errors.Is(err, service.ErrSyncDeviceNotFound) {
			http.Error(w, "no such device", http.StatusNotFound)
			return
		}
		h.logger.Error("failed to prune sync device", "site_id", siteID, "error", err)
		http.Error(w, "failed to prune device", http.StatusInternalServerError)
		return
	}
	h.logger.Info("pruned sync device", "site_id", siteID)
	if r.Header.Get("HX-Request") == "true" {
		data := h.settingsData(r)
		data["activeTab"] = "settings-devices"
		data["SettingsGroup"] = "devices"
		h.renderTemplate(w, r, "settings_devices", data)
		return
	}
	http.Redirect(w, r, "/settings/devices", http.StatusSeeOther)
}

// handleSyncDeviceCompact runs one relay-log compaction pass on demand (the
// prune → reclaim-history loop). 404s when no ForestNote source is wired.
func (h *Handler) handleSyncDeviceCompact(w http.ResponseWriter, r *http.Request) {
	if h.syncDevices == nil {
		http.NotFound(w, r)
		return
	}
	result, err := h.syncDevices.CompactNow(r.Context())
	if err != nil {
		h.logger.Error("manual sync compaction failed", "error", err)
		http.Error(w, "compaction failed", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		data := h.settingsData(r)
		data["activeTab"] = "settings-devices"
		data["SettingsGroup"] = "devices"
		data["SyncCompactResult"] = result
		h.renderTemplate(w, r, "settings_devices", data)
		return
	}
	http.Redirect(w, r, "/settings/devices", http.StatusSeeOther)
}

func (h *Handler) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}
	dueDateStr := strings.TrimSpace(r.FormValue("due_date"))
	var dueAt *time.Time
	if dueDateStr != "" {
		if t, err := time.Parse("2006-01-02", dueDateStr); err == nil {
			utc := t.UTC()
			dueAt = &utc
		} else {
			http.Error(w, "invalid due date", http.StatusBadRequest)
			return
		}
	}
	created, err := h.tasks.Create(r.Context(), service.TaskCreate{Title: title, DueAt: dueAt})
	if err != nil {
		http.Error(w, "failed to create task", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		h.renderFragment(w, r, "_task_row", created)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) handleCompleteTask(w http.ResponseWriter, r *http.Request) {
	if h.tasks == nil {
		http.NotFound(w, r)
		return
	}
	taskID := r.PathValue("id")
	if err := h.tasks.Complete(r.Context(), taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "task not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to complete task", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		t, err := h.tasks.Get(r.Context(), taskID)
		if err != nil {
			h.logger.Error("failed to fetch completed task for fragment render", "id", taskID, "error", err)
			http.Error(w, "failed to render row", http.StatusInternalServerError)
			return
		}
		h.renderFragment(w, r, "_task_row", t)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) handleBulkAction(w http.ResponseWriter, r *http.Request) {
	action, ids := r.FormValue("action"), r.Form["task_ids"]
	if action != "complete" && action != "delete" {
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	if len(ids) > 0 {
		var err error
		if action == "complete" {
			err = h.tasks.BulkComplete(r.Context(), ids)
		} else {
			err = h.tasks.BulkDelete(r.Context(), ids)
		}
		if err != nil {
			http.Error(w, "bulk action failed", http.StatusInternalServerError)
			return
		}
	}
	if r.Header.Get("HX-Request") == "true" {
		if action == "complete" {
			for _, id := range ids {
				t, err := h.tasks.Get(r.Context(), id)
				if err != nil {
					h.logger.Error("bulk complete: failed to fetch task for fragment render", "id", id, "error", err)
					continue
				}
				h.renderFragment(w, r, "_task_row", t)
			}
		}
		// action=delete: empty response body; client removes checked rows.
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) handlePurgeCompleted(w http.ResponseWriter, r *http.Request) {
	// Legacy form route — keeps the empty-200 / 303 shape the web UI
	// expects. The count returned by PurgeCompleted is intentionally
	// discarded here; the page reloads / DOM-sweeps anyway. The
	// /api/v1/tasks/purge-completed path is the one that returns
	// {"deleted": N} for MCP/CLI callers.
	if _, err := h.tasks.PurgeCompleted(r.Context()); err != nil {
		http.Error(w, "purge failed", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// webPurgeDeletedDays is the cutoff the legacy /tasks/purge-deleted form
// uses. Hard-coded at 30 days so the UI button matches the REST default
// (purgeDeletedDefaultDays in api_v1.go) without exposing the knob to the
// web operator — power users wanting a different window go through the
// REST endpoint or MCP tool directly.
const webPurgeDeletedDays = 30

// handlePurgeDeleted is the form-target sibling of /api/v1/tasks/purge-deleted.
// HTMX response is empty 200; non-HX is a redirect home so a plain browser
// also gets sensible behavior. Matches handlePurgeCompleted's pattern.
func (h *Handler) handlePurgeDeleted(w http.ResponseWriter, r *http.Request) {
	if _, _, err := h.tasks.PurgeDeleted(r.Context(), webPurgeDeletedDays); err != nil {
		http.Error(w, "purge failed", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// settingsGroupForSection maps a /settings/save section value to the Settings
// group page that owns its form, so saves land back where they came from.
func settingsGroupForSection(section string) string {
	switch section {
	case "supernote", "ub-spc", "sync", "boox":
		return "devices"
	case "ai", "integrations", "system":
		return section
	default:
		return "devices"
	}
}

func (h *Handler) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	cObj, _ := h.config.GetConfig(r.Context())
	cfg := cObj.(*appconfig.Config)
	switch r.FormValue("section") {
	case "supernote":
		// JIIX injection + OCR prompt are runtime-configurable keys read at job
		// time by the Supernote source (notedb.GetSetting), not Config fields —
		// write them directly so they take effect without a restart.
		if h.noteDB != nil {
			ctx := r.Context()
			_ = notedb.SetSetting(ctx, h.noteDB, appconfig.KeySNInjectEnabled, r.FormValue("inject_enabled"))
			if v := r.FormValue("ocr_prompt"); v != "" {
				_ = notedb.SetSetting(ctx, h.noteDB, appconfig.KeySNOCRPrompt, v)
			}
		}
		http.Redirect(w, r, "/settings/devices", http.StatusSeeOther)
		return
	case "ub-spc":
		// UB-as-SPC device-sync server config. Every field is restart-required
		// (the server is constructed once at startup), so UpdateConfig below
		// flags the restart banner automatically.
		// "Enable device sync server" checkbox: present only when checked.
		// (Internally still the SPCMode string: server = on, client = off — the
		// legacy SPC client was removed in PR #16, so "client" now just means
		// the SPC server is disabled.)
		if r.FormValue("spc_enabled") != "" {
			cfg.SPCMode = "server"
		} else {
			cfg.SPCMode = "client"
		}
		cfg.SPCListenAddr = r.FormValue("spc_listen_addr")
		cfg.SPCFileRoot = r.FormValue("spc_file_root")
		cfg.SPCDeviceAccount = r.FormValue("spc_device_account")
		cfg.SPCTLSCert = r.FormValue("spc_tls_cert")
		cfg.SPCTLSKey = r.FormValue("spc_tls_key")
		if v := strings.TrimSpace(r.FormValue("spc_quota_bytes")); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				cfg.SPCQuotaBytes = n
			}
		}
		// Secrets: an empty field means "keep the current stored value"
		// (mirrors the password / OCR-API-key write-only pattern).
		if v := r.FormValue("spc_device_password"); v != "" {
			cfg.SPCDevicePassword = v
		}
		if v := r.FormValue("spc_jwt_secret"); v != "" {
			cfg.SPCJWTSecret = v
		}
		if v := r.FormValue("spc_oss_secret"); v != "" {
			cfg.SPCOssSecret = v
		}
	case "sync":
		// SyncEnabled + SyncBatchLimit are restart-required (route + service are
		// wired once at startup), so UpdateConfig flags the banner.
		cfg.SyncEnabled = r.FormValue("sync_enabled") == "true"
		if v := strings.TrimSpace(r.FormValue("sync_batch_limit")); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.SyncBatchLimit = n
			}
		}
		// OCR prompt is a runtime key read per page via closure (no restart);
		// store it directly like the Boox prompt.
		if h.noteDB != nil {
			_ = notedb.SetSetting(r.Context(), h.noteDB, appconfig.KeyForestNoteOCRPrompt, r.FormValue("forestnote_ocr_prompt"))
		}
		// Relay-log compaction is read from the ForestNote SOURCE row's config_json at source Start
		// (NOT appconfig — unlike SyncBatchLimit above, which only seeds a brand-new row and is a
		// no-op for an already-seeded one). Merge the flag into the row's config_json so batch_limit
		// and any other keys survive; UpdateSource flags the RestartRequired banner. Skipped silently
		// when no ForestNote source row exists (nothing to compact).
		if row, ok := h.forestNoteSourceRow(r.Context()); ok {
			if err := h.setSourceConfigBool(r.Context(), row, "compaction", r.FormValue("sync_compaction") == "true"); err != nil {
				h.logger.Warn("forestnote compaction toggle save failed", "err", err)
			}
		}
	case "ai":
		cfg.EmbedEnabled = r.FormValue("embed_enabled") == "true"
		cfg.OllamaURL, cfg.OllamaEmbedModel = r.FormValue("ollama_url"), r.FormValue("ollama_embed_model")
		cfg.ChatEnabled = r.FormValue("chat_enabled") == "true"
		cfg.ChatAPIURL, cfg.ChatModel = r.FormValue("chat_api_url"), r.FormValue("chat_model")
	case "integrations":
		if v := strings.TrimSpace(r.FormValue("caldav_collection_name")); v != "" {
			cfg.CalDAVCollectionName = v
		}
	case "system":
		cfg.LogVerboseAPI = r.FormValue("log_verbose_api") == "true"
	case "boox":
		// Boox settings are stored as runtime-configurable keys in the
		// settings table (not on the Config struct); write them
		// directly via notedb so they take effect on the next
		// processor run without a restart.
		if h.noteDB != nil {
			ctx := r.Context()
			if v := r.FormValue("ocr_prompt"); v != "" {
				_ = notedb.SetSetting(ctx, h.noteDB, appconfig.KeyBooxOCRPrompt, v)
			}
			_ = notedb.SetSetting(ctx, h.noteDB, appconfig.KeyBooxTodoEnabled, r.FormValue("todo_enabled"))
			if v := r.FormValue("todo_prompt"); v != "" {
				_ = notedb.SetSetting(ctx, h.noteDB, appconfig.KeyBooxTodoPrompt, v)
			}
			_ = notedb.SetSetting(ctx, h.noteDB, appconfig.KeyBooxImportNotes, r.FormValue("import_notes"))
			_ = notedb.SetSetting(ctx, h.noteDB, appconfig.KeyBooxImportPDFs, r.FormValue("import_pdfs"))
			// external_base_url: explicit empty string clears it (so the
			// user can go back to relative-path mode); otherwise trim
			// trailing slash and save.
			extURL := strings.TrimSpace(r.FormValue("external_base_url"))
			extURL = strings.TrimRight(extURL, "/")
			_ = notedb.SetSetting(ctx, h.noteDB, appconfig.KeyBooxExternalBaseURL, extURL)
		}
		http.Redirect(w, r, "/settings/devices", http.StatusSeeOther)
		return
	}
	h.config.UpdateConfig(r.Context(), cfg)
	http.Redirect(w, r, "/settings/"+settingsGroupForSection(r.FormValue("section")), http.StatusSeeOther)
}

func (h *Handler) handleBackfillEmbeddings(w http.ResponseWriter, r *http.Request) {
	if h.search == nil || !h.search.HasEmbeddingPipeline() {
		http.NotFound(w, r)
		return
	}
	h.search.TriggerBackfill(r.Context())
	http.Redirect(w, r, "/settings/ai", http.StatusSeeOther)
}

// respondEmptyOrRedirect is the shared HX/non-HX tail for broad file mutations
// (scan, import, retry-failed, migrate-imports, processor start/stop). On HX
// it emits an empty 200 body; the client-side poller picks up the effect on
// its next tick (updateProcessorStatus is also hooked via hx-on to refresh
// immediately). On non-HX it redirects to the caller-specified tab so each
// action lands back on the page it was triggered from.
func (h *Handler) respondEmptyOrRedirect(w http.ResponseWriter, r *http.Request, redirectTo string) {
	if r.Header.Get("HX-Request") == "true" {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, redirectTo, http.StatusSeeOther)
}

func (h *Handler) handleProcessorStart(w http.ResponseWriter, r *http.Request) {
	h.notes.StartProcessor(r.Context())
	h.respondEmptyOrRedirect(w, r, "/files/supernote")
}

func (h *Handler) handleProcessorStop(w http.ResponseWriter, r *http.Request) {
	h.notes.StopProcessor(r.Context())
	h.respondEmptyOrRedirect(w, r, "/files/supernote")
}

func (h *Handler) handleBooxProcessorStart(w http.ResponseWriter, r *http.Request) {
	h.notes.StartBooxProcessor(r.Context())
	h.respondEmptyOrRedirect(w, r, "/files/boox")
}

func (h *Handler) handleBooxProcessorStop(w http.ResponseWriter, r *http.Request) {
	h.notes.StopBooxProcessor(r.Context())
	h.respondEmptyOrRedirect(w, r, "/files/boox")
}

func (h *Handler) handleFilesScan(w http.ResponseWriter, r *http.Request) {
	h.notes.ScanFiles(r.Context())
	h.respondEmptyOrRedirect(w, r, "/files/supernote")
}

func (h *Handler) handleFilesImport(w http.ResponseWriter, r *http.Request) {
	h.notes.ImportFiles(r.Context())
	h.respondEmptyOrRedirect(w, r, "/files/boox")
}

func (h *Handler) handleFilesRetryFailed(w http.ResponseWriter, r *http.Request) {
	h.notes.RetryFailed(r.Context())
	h.respondEmptyOrRedirect(w, r, "/files/boox")
}

func (h *Handler) handleFilesDeleteNote(w http.ResponseWriter, r *http.Request) {
	if err := h.notes.DeleteNote(r.Context(), r.FormValue("path")); err != nil {
		http.Error(w, "failed to delete note", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		w.WriteHeader(http.StatusOK)
		return
	}
	// DeleteNote is Boox-only (service layer no-ops Supernote paths), so
	// the non-HX landing tab is always /files/boox.
	http.Redirect(w, r, "/files/boox", http.StatusSeeOther)
}

func (h *Handler) handleFilesDeleteBulk(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	paths := r.Form["paths"]
	if len(paths) > 0 {
		if err := h.notes.BulkDelete(r.Context(), paths); err != nil {
			http.Error(w, "failed to delete", http.StatusInternalServerError)
			return
		}
	}
	if r.Header.Get("HX-Request") == "true" {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/files/boox", http.StatusSeeOther)
}

func (h *Handler) handleFilesMigrateImports(w http.ResponseWriter, r *http.Request) {
	h.notes.MigrateImports(r.Context())
	h.respondEmptyOrRedirect(w, r, "/files/boox")
}

func (h *Handler) handleFilesMove(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	path := r.FormValue("path")
	folder := r.FormValue("folder")
	if path == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	if err := h.notes.MoveBooxNote(r.Context(), path, folder); err != nil {
		h.logger.Error("move boox note", "path", path, "folder", folder, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		// Row no longer belongs to current view (folder filter changed) —
		// remove it from the table.
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/files/boox", http.StatusSeeOther)
}

func (h *Handler) handleFilesMoveBulk(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	paths := r.Form["paths"]
	folder := r.FormValue("folder")
	if len(paths) == 0 {
		http.Error(w, "no paths selected", http.StatusBadRequest)
		return
	}
	moved, failed, err := h.notes.BulkMoveBooxNotes(r.Context(), paths, folder)
	if err != nil && moved == 0 {
		h.logger.Error("bulk move boox notes", "error", err)
		http.Error(w, "all moves failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if failed > 0 {
		h.logger.Warn("bulk move partial", "moved", moved, "failed", failed)
	}
	if r.Header.Get("HX-Request") == "true" {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/files/boox", http.StatusSeeOther)
}

func (h *Handler) handleMaintenanceBooxReconcileDates(w http.ResponseWriter, r *http.Request) {
	n, err := h.notes.ReconcileBooxCreatedAt(r.Context())
	if err != nil {
		h.logger.Error("reconcile boox dates", "error", err)
		http.Error(w, "failed to reconcile dates", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<p class="text-small">Reconciled %d row(s).</p>`, n)
}

func (h *Handler) handleMaintenanceBooxDeleteUntitled(w http.ResponseWriter, r *http.Request) {
	rows, files, versions, err := h.notes.DeleteAutoNamedNotebooks(r.Context())
	if err != nil {
		h.logger.Error("delete auto-named notebooks", "error", err)
		http.Error(w, "failed to delete auto-named notebooks", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<p class="text-small">Deleted %d row(s), %d file(s), %d versions dir(s).</p>`,
		rows, files, versions)
}

func (h *Handler) handleMaintenanceBooxScanUntracked(w http.ResponseWriter, r *http.Request) {
	scanned, enqueued, err := h.notes.ScanAndEnqueueUntracked(r.Context())
	if err != nil {
		h.logger.Error("scan untracked boox files", "error", err)
		http.Error(w, "failed to scan untracked files", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<p class="text-small">Scanned %d file(s), enqueued %d previously untracked.</p>`,
		scanned, enqueued)
}

// respondFileRowOrRedirect fetches the updated file and emits a source-
// specific row fragment on HX-Request; otherwise redirects back to the
// appropriate tab with the caller-supplied `back` query string preserved.
// Boox paths dispatch to `_boox_file_row` + `/files/boox`; everything else
// goes to `_sn_file_row` + `/files/supernote`.
func (h *Handler) respondFileRowOrRedirect(w http.ResponseWriter, r *http.Request, path string) {
	isBoox := h.booxNotesPath != "" && strings.HasPrefix(path, h.booxNotesPath)
	if r.Header.Get("HX-Request") == "true" {
		if isBoox {
			bn, err := h.notes.GetBooxNote(r.Context(), path)
			if err != nil {
				h.logger.Error("failed to fetch boox note for fragment render", "path", path, "error", err)
				http.Error(w, "failed to render row", http.StatusInternalServerError)
				return
			}
			h.renderFragment(w, r, "_boox_file_row", bn)
			return
		}
		f, err := h.notes.GetFile(r.Context(), path)
		if err != nil {
			h.logger.Error("failed to fetch file for fragment render", "path", path, "error", err)
			http.Error(w, "failed to render row", http.StatusInternalServerError)
			return
		}
		h.renderFragment(w, r, "_sn_file_row", fileRowCtx{File: f, RelPath: r.FormValue("back")})
		return
	}
	if isBoox {
		http.Redirect(w, r, "/files/boox", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/files/supernote?path="+url.QueryEscape(r.FormValue("back")), http.StatusSeeOther)
}

func (h *Handler) handleFilesQueue(w http.ResponseWriter, r *http.Request) {
	path := r.FormValue("path")
	if err := h.notes.Enqueue(r.Context(), path, false); err != nil {
		http.Error(w, "failed to enqueue", http.StatusInternalServerError)
		return
	}
	h.respondFileRowOrRedirect(w, r, path)
}

func (h *Handler) handleFilesSkip(w http.ResponseWriter, r *http.Request) {
	path := r.FormValue("path")
	if err := h.notes.Skip(r.Context(), path, "manual"); err != nil {
		http.Error(w, "failed to skip", http.StatusInternalServerError)
		return
	}
	h.respondFileRowOrRedirect(w, r, path)
}

func (h *Handler) handleFilesUnskip(w http.ResponseWriter, r *http.Request) {
	path := r.FormValue("path")
	if err := h.notes.Unskip(r.Context(), path); err != nil {
		http.Error(w, "failed to unskip", http.StatusInternalServerError)
		return
	}
	h.respondFileRowOrRedirect(w, r, path)
}

func (h *Handler) handleFilesForce(w http.ResponseWriter, r *http.Request) {
	path := r.FormValue("path")
	if err := h.notes.Enqueue(r.Context(), path, true); err != nil {
		http.Error(w, "failed to force-enqueue", http.StatusInternalServerError)
		return
	}
	h.respondFileRowOrRedirect(w, r, path)
}

func (h *Handler) handleBooxRender(w http.ResponseWriter, r *http.Request) {
	notePath := r.URL.Query().Get("path")
	if !h.validNotePath(notePath) {
		http.Error(w, "path outside notes directory", http.StatusForbidden)
		return
	}
	p, _ := strconv.Atoi(r.URL.Query().Get("page"))
	stream, ct, err := h.notes.RenderPage(r.Context(), notePath, p)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer stream.Close()
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=300")
	io.Copy(w, stream)
}

func (h *Handler) handleBooxVersions(w http.ResponseWriter, r *http.Request) {
	v, _ := h.notes.ListVersions(r.Context(), r.URL.Query().Get("path"))
	if v == nil {
		v = []interface{}{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (h *Handler) handleMCPTokenCreate(w http.ResponseWriter, r *http.Request) {
	label := strings.TrimSpace(r.FormValue("label"))
	if label == "" {
		http.Error(w, "token label is required", http.StatusBadRequest)
		return
	}
	t, _, err := mcpauth.CreateToken(r.Context(), h.noteDB, label)
	if err != nil {
		h.logger.Error("failed to create token", "error", err)
		http.Error(w, "failed to create token", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		// Full settingsData, not just the flash key — rendering the group with
		// only NewMCPToken would blank every other card on the page.
		data := h.settingsData(r)
		data["activeTab"], data["SettingsGroup"], data["NewMCPToken"] = "settings-integrations", "integrations", t
		h.renderTemplate(w, r, "settings_integrations", data)
		return
	}
	http.Redirect(w, r, "/settings/integrations?new_token="+url.QueryEscape(t)+"#mcp-tokens", http.StatusSeeOther)
}

func (h *Handler) handleMCPTokenRevoke(w http.ResponseWriter, r *http.Request) {
	mcpauth.RevokeToken(r.Context(), h.noteDB, r.FormValue("token_hash"))
	if r.Header.Get("HX-Request") == "true" {
		data := h.settingsData(r)
		data["activeTab"], data["SettingsGroup"] = "settings-integrations", "integrations"
		h.renderTemplate(w, r, "settings_integrations", data)
		return
	}
	http.Redirect(w, r, "/settings/integrations#mcp-tokens", http.StatusSeeOther)
}

func (h *Handler) handleAsk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID int    `json:"session_id"`
		Question  string `json:"question"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	responses, err := h.search.Ask(r.Context(), req.Question, req.SessionID)
	if err != nil {
		http.Error(w, "chat failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	for resp := range responses {
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(resp))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func (h *Handler) handleChatSessions(w http.ResponseWriter, r *http.Request) {
	s, _ := h.search.ListSessions(r.Context())
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s)
}

func (h *Handler) handleFilesStatus(w http.ResponseWriter, r *http.Request) {
	status, err := h.notes.GetProcessorStatus(r.Context())
	if err != nil {
		h.logger.Error("failed to get processor status", "error", err)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func (h *Handler) handleFilesHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Query().Get("path")
	if path == "" {
		w.Write([]byte("null"))
		return
	}
	if !h.validNotePath(path) {
		w.Write([]byte("null"))
		return
	}
	details, err := h.notes.GetNoteDetails(r.Context(), path)
	if err != nil {
		h.logger.Error("failed to get note details", "path", path, "error", err)
		w.Write([]byte("null"))
		return
	}
	json.NewEncoder(w).Encode(details)
}

func (h *Handler) handleFilesRender(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	pageStr := r.URL.Query().Get("page")
	if path == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	if !h.validNotePath(path) {
		http.Error(w, "path outside notes directory", http.StatusForbidden)
		return
	}
	pageIdx, err := strconv.Atoi(pageStr)
	if err != nil || pageIdx < 0 {
		pageIdx = 0
	}

	stream, contentType, err := h.notes.RenderPage(r.Context(), path, pageIdx)
	if err != nil {
		h.logger.Error("render failed", "path", path, "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	defer stream.Close()

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=300")
	io.Copy(w, stream)
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (h *Handler) handleChatMessages(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.URL.Query().Get("session_id"))
	m, _ := h.search.GetMessages(r.Context(), id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(m)
}

// HandleOAuthAuthorize handles the first leg of Claude's OAuth flow.
func (h *Handler) HandleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	redirectURI := r.URL.Query().Get("redirect_uri")
	state := r.URL.Query().Get("state")

	if redirectURI == "" {
		http.Error(w, "missing redirect_uri", http.StatusBadRequest)
		return
	}

	target, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	isLocalhost := target.Hostname() == "localhost" || target.Hostname() == "127.0.0.1" || target.Hostname() == "::1"
	if target.Scheme != "https" && !isLocalhost {
		http.Error(w, "redirect_uri must use HTTPS", http.StatusBadRequest)
		return
	}

	code := h.generateOAuthCode()

	q := target.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	target.RawQuery = q.Encode()

	h.logger.Info("OAuth authorize: redirecting to client", "target", target.String())
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func (h *Handler) generateOAuthCode() string {
	b := make([]byte, 32)
	rand.Read(b)
	code := base64.RawURLEncoding.EncodeToString(b)

	h.oauthCodesMu.Lock()
	defer h.oauthCodesMu.Unlock()
	if h.oauthCodes == nil {
		h.oauthCodes = make(map[string]time.Time)
	}
	// Purge expired codes while we hold the lock.
	now := time.Now()
	for k, exp := range h.oauthCodes {
		if now.After(exp) {
			delete(h.oauthCodes, k)
		}
	}
	h.oauthCodes[code] = now.Add(5 * time.Minute)
	return code
}

func (h *Handler) consumeOAuthCode(code string) bool {
	h.oauthCodesMu.Lock()
	defer h.oauthCodesMu.Unlock()
	exp, ok := h.oauthCodes[code]
	if !ok {
		return false
	}
	delete(h.oauthCodes, code)
	return time.Now().Before(exp)
}

// HandleOAuthToken handles the token exchange leg of Claude's OAuth flow.
func (h *Handler) HandleOAuthToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	code := r.FormValue("code")
	if !h.consumeOAuthCode(code) {
		h.logger.Warn("OAuth token: invalid or expired code", "remote_ip", r.RemoteAddr)
		http.Error(w, "invalid_grant", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rawToken, _, err := mcpauth.CreateToken(ctx, h.noteDB, "Claude-OAuth")
	if err != nil {
		h.logger.Error("OAuth token: failed to create bearer token", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.logger.Info("OAuth token: issued new bearer token for Claude")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"access_token": rawToken,
		"token_type":   "Bearer",
		"expires_in":   315360000,
	})
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func (h *Handler) baseTemplateData(ctx context.Context) map[string]interface{} {
	data := map[string]interface{}{}
	if h.tasks != nil {
		// ListIncludingDeleted so the template can render the trash view
		// gated on the "Show deleted" toggle. Each row carries Deleted bool;
		// the template hides ghosts by default via inline style + data attr.
		// DeletedCount surfaces the backlog size in the toggle label so the
		// operator can decide whether to actually look at the trash.
		if t, err := h.tasks.ListIncludingDeleted(ctx); err == nil {
			data["tasks"] = t
			deletedCount := 0
			for _, tk := range t {
				if tk.Deleted {
					deletedCount++
				}
			}
			data["DeletedCount"] = deletedCount
		}
	}
	data["BooxNotesPath"] = h.booxNotesPath
	data["BooxImportPath"] = h.booxImportPath
	if h.config != nil {
		data["RestartRequired"] = h.config.IsRestartRequired()
	}
	data["chatEnabled"] = h.search != nil
	fnSourceWired := false
	if h.notes != nil {
		data["HasSupernoteSource"] = h.notes.HasSupernoteSource()
		data["HasBooxSource"] = h.notes.HasBooxSource()
		data["HasRemarkableSource"] = h.notes.HasRemarkableSource()
		fnSourceWired = h.notes.HasForestNoteSource()
	}
	// Source-type flags for the search facet and the device-grouped nav.
	// Digests are available whenever the digest store is wired (server mode).
	data["HasDigests"] = h.digests != nil
	// ForestNote is present when a source is wired (the new first-class path);
	// fall back to the legacy sync_enabled setting for older deployments.
	if fnSourceWired {
		data["HasForestNote"] = true
	} else if h.noteDB != nil {
		if v, _ := notedb.GetSetting(ctx, h.noteDB, appconfig.KeySyncEnabled); strings.EqualFold(v, "true") || v == "1" {
			data["HasForestNote"] = true
		}
	}
	return data
}

type breadcrumb struct{ Label, RelPath string }

func buildBreadcrumbs(p string) []breadcrumb {
	res := []breadcrumb{{Label: "Home", RelPath: ""}}
	if p == "" {
		return res
	}
	parts := strings.Split(p, "/")
	for i := range parts {
		res = append(res, breadcrumb{Label: parts[i], RelPath: strings.Join(parts[:i+1], "/")})
	}
	return res
}

// crumb is the normalized breadcrumb shape consumed by the shared
// _files_breadcrumb partial: a display label paired with the navigation URL.
type crumb struct{ Label, HxGet string }

// pipelinePanel is the context for the shared _files_status_panel partial.
// Sources with a processor worker (Supernote, Boox) set StartStop and a Source
// slug; ForestNote (no global worker) sets Note instead.
type pipelinePanel struct {
	Source    string
	StartStop bool
	Note      string
}

// detailView and its parts are the context for the shared _detail_page_grid
// partial — the in-tab note detail that replaced the #history-modal.
type detailView struct {
	Title       string
	BackURL     string
	Meta        []detailKV
	Pages       []detailPage
	Actions     []detailAction
	JobInfoURL  string // collapsible job-record fetch (/files/history)
	VersionsURL string // collapsible version-history fetch (Boox only)
	EmptyMsg    string
}

type detailKV struct{ Label, Value string }

type detailPage struct {
	PageID   string
	ImgURL   string
	Caption  string
	BodyText string
	Source   string
	Keywords string
	Target   bool
}

type detailAction struct {
	Label   string
	Href    string // link-style action (e.g. a PDF download); renders an <a> instead of a button
	HxPost  string
	Vals    string // hx-vals JSON
	Confirm string
	OnAfter string // hx-on:htmx:after-request body
	Danger  bool
}

// buildNoteDetail assembles the in-tab detail for a Supernote/Boox note from
// its indexed page content. Pages render lazily via /files/render; the job
// record (and Boox version history) load on demand into collapsibles.
func (h *Handler) buildNoteDetail(ctx context.Context, path, title, backURL, versionsURL string, actions []detailAction) (detailView, error) {
	pages, err := h.notes.GetNotePages(ctx, path)
	if err != nil {
		return detailView{}, err
	}
	dps := make([]detailPage, 0, len(pages))
	for _, p := range pages {
		dps = append(dps, detailPage{
			ImgURL:   "/files/render?path=" + url.QueryEscape(path) + "&page=" + strconv.Itoa(p.Page) + "&v=2",
			Caption:  "Page " + strconv.Itoa(p.Page+1),
			BodyText: p.BodyText,
			Source:   p.Source,
			Keywords: p.Keywords,
		})
	}
	return detailView{
		Title:       title,
		BackURL:     backURL,
		Meta:        []detailKV{{Label: "Pages", Value: strconv.Itoa(len(pages))}},
		Pages:       dps,
		Actions:     actions,
		JobInfoURL:  "/files/history?path=" + url.QueryEscape(path),
		VersionsURL: versionsURL,
		EmptyMsg:    "No indexed content yet — queue this note for OCR to see its pages.",
	}, nil
}

// forestNoteDetailView maps a ForestNote notebook detail onto the shared
// detailView so ForestNote renders through the same _detail_page_grid partial
// as Supernote/Boox. ForestNote pages render on the fly via
// /files/forestnote/render; its actions (PDF export, per-notebook Re-OCR with
// transient feedback, soft-delete) carry the same behavior as the old bespoke
// template.
func forestNoteDetailView(d service.ForestNoteNotebookDetail, targetPageID string) detailView {
	folder := "(unfiled)"
	if len(d.FolderPath) > 0 {
		folder = strings.Join(d.FolderPath, " / ")
	}
	pages := make([]detailPage, 0, len(d.Pages))
	for _, p := range d.Pages {
		pages = append(pages, detailPage{
			PageID:   p.PageID,
			ImgURL:   "/files/forestnote/render?path=" + url.QueryEscape(p.Path),
			Caption:  "Page " + strconv.Itoa(p.Ordinal+1),
			BodyText: p.BodyText,
			Target:   targetPageID != "" && p.PageID == targetPageID,
		})
	}
	const reocrFeedback = "if(event.detail.successful){ this.textContent='Queued ✓'; this.disabled=true; setTimeout(() => { this.textContent='↻ Re-OCR'; this.disabled=false; }, 2500); updateProcessorStatus(); } else { this.textContent='Failed ✗'; setTimeout(() => { this.textContent='↻ Re-OCR'; }, 2500); }"
	return detailView{
		Title:   d.Name,
		BackURL: "/files/forestnote",
		Meta: []detailKV{
			{Label: "Folder", Value: folder},
			{Label: "Pages", Value: strconv.Itoa(d.PageCount)},
			{Label: "Created", Value: formatTimestampMs(d.CreatedAt)},
			{Label: "Modified", Value: formatTimestampMs(d.ModifiedAt)},
		},
		Pages: pages,
		Actions: []detailAction{
			{Label: "⬇ Download PDF", Href: "/files/forestnote/export?notebook=" + url.QueryEscape(d.NotebookID)},
			{Label: "↻ Re-OCR", HxPost: "/files/forestnote/reprocess?notebook=" + url.QueryEscape(d.NotebookID), OnAfter: reocrFeedback},
			{
				Label:   "✗ Delete",
				Danger:  true,
				HxPost:  "/files/forestnote/delete",
				Vals:    mustJSON(map[string]string{"notebook": d.NotebookID}),
				Confirm: "Delete “" + d.Name + "” from UltraBridge? This removes it from your library and search index. If the notebook still exists on your device and you edit it again, it will reappear on the next sync — to delete permanently, delete it on the device.",
				OnAfter: "if(event.detail.successful){window.location='/files/forestnote';}",
			},
		},
		EmptyMsg: "This notebook has no pages yet.",
	}
}

// formatTimestampMs formats a millisecond UTC unix timestamp as the UI does
// ("Never" for 0). Shared by the formatTimestamp template func and Go-side
// detail builders.
func formatTimestampMs(ms int64) string {
	if ms == 0 {
		return "Never"
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04")
}

// supernoteCrumbs adapts the Supernote relPath breadcrumb chain to []crumb.
// RelPath is query-escaped (like the pager's path param) so folder names with
// URL-significant characters — '&', '#', spaces — don't truncate the crumb URL.
func supernoteCrumbs(bs []breadcrumb) []crumb {
	out := make([]crumb, 0, len(bs))
	for _, b := range bs {
		out = append(out, crumb{Label: b.Label, HxGet: "/files/supernote?path=" + url.QueryEscape(b.RelPath)})
	}
	return out
}

// forestNoteCrumbs adapts the ForestNote folder chain to []crumb, prepending
// the "Home" root the FN tab shows ahead of its synced folders.
func forestNoteCrumbs(cs []service.ForestNoteCrumb) []crumb {
	out := make([]crumb, 0, len(cs)+1)
	out = append(out, crumb{Label: "Home", HxGet: "/files/forestnote"})
	for _, c := range cs {
		out = append(out, crumb{Label: c.Name, HxGet: "/files/forestnote?folder=" + c.FolderID})
	}
	return out
}

// pager builds the context map for the shared _files_pagination partial. params
// holds the preserved (non-page) query params; empty-valued entries are dropped
// so the pager never emits a stray "&key=".
func pager(baseURL string, page, totalPages int, params map[string]string) map[string]any {
	clean := make(map[string]string, len(params))
	for k, v := range params {
		if v != "" {
			clean[k] = v
		}
	}
	return map[string]any{
		"BaseURL":    baseURL,
		"Page":       page,
		"TotalPages": totalPages,
		"Params":     clean,
	}
}

func safeRelPath(p string) (string, bool) {
	if p == "" {
		return "", true
	}
	c := filepath.Clean(p)
	if filepath.IsAbs(c) || strings.HasPrefix(c, "..") {
		return "", false
	}
	return c, true
}

// validNotePath returns true if path falls under one of the configured notes
// directories. Prevents arbitrary filesystem reads through path query params.
func (h *Handler) validNotePath(path string) bool {
	// ForestNote is an opaque URI scheme, not a filesystem path (and filepath.Clean
	// would mangle the "//"). The note service resolves it against the syncstore
	// mirror, so there is no directory to escape. Check the raw path first.
	if fnpath.Is(path) {
		return true
	}
	cleaned := filepath.Clean(path)
	if h.notesPathPrefix != "" && strings.HasPrefix(cleaned, h.notesPathPrefix) {
		return true
	}
	if h.booxImportPath != "" && strings.HasPrefix(cleaned, h.booxImportPath) {
		return true // imported-but-unmigrated Boox notes still live under the import path
	}
	if h.booxNotesPath != "" && strings.HasPrefix(cleaned, h.booxNotesPath) {
		return true
	}
	return false
}
