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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sysop/ultrabridge/internal/appconfig"
	"github.com/sysop/ultrabridge/internal/logging"
	"github.com/sysop/ultrabridge/internal/mcpauth"
	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/service"
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

//go:embed static
var staticFS embed.FS

type Handler struct {
	tasks    service.TaskService
	notes    service.NoteService
	search   service.SearchService
	config   service.ConfigService
	
	noteDB          *sql.DB
	notesPathPrefix string
	booxNotesPath   string
	booxImportPath  string
	tmpl            *template.Template
	mux             *http.ServeMux
	logger          *slog.Logger
	broadcaster     *logging.LogBroadcaster

	oauthCodesMu sync.Mutex
	oauthCodes   map[string]time.Time // code -> expiry
}

func formatDueTime(val interface{}) string {
	var t time.Time
	switch v := val.(type) {
	case int64:
		if v == 0 { return "No due date" }
		t = time.UnixMilli(v).UTC()
	case *time.Time:
		if v == nil { return "No due date" }
		t = *v
	case time.Time:
		if v.IsZero() { return "No due date" }
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
		if v == 0 { return "-" }
		t = time.UnixMilli(v).UTC()
	case *time.Time:
		if v == nil { return "-" }
		t = *v
	case time.Time:
		if v.IsZero() { return "-" }
		t = v
	case sql.NullInt64:
		if !v.Valid || v.Int64 == 0 { return "-" }
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
		"formatDueTime": formatDueTime,
		"formatCreated": formatCreated,
		"formatTimestamp": func(ms int64) string {
			if ms == 0 { return "Never" }
			return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04")
		},
		"fileTypeStr": func(ft string) string { return ft },
		"fileRowID": func(path string) string {
			sum := sha1.Sum([]byte(path))
			return "file-" + hex.EncodeToString(sum[:])[:12]
		},
		"makeFileRowCtx": func(f service.NoteFile, relPath string) fileRowCtx {
			return fileRowCtx{File: f, RelPath: relPath}
		},
		"noteSource": func(path string) string {
			if h.booxNotesPath != "" && strings.HasPrefix(path, h.booxNotesPath) { return "Boox" }
			if h.booxImportPath != "" && strings.HasPrefix(path, h.booxImportPath) { return "Boox" }
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
			if val == nil { return nil }
			var link struct {
				AppName  string `json:"appName"`
				FilePath string `json:"filePath"`
				Page     int    `json:"page"`
			}
			switch v := val.(type) {
			case string:
				if v == "" { return nil }
				data, _ := base64.StdEncoding.DecodeString(v)
				json.Unmarshal(data, &link)
			case *service.TaskLink:
				if v == nil { return nil }
				link.AppName, link.FilePath, link.Page = v.AppName, v.FilePath, v.Page
			case service.TaskLink:
				link.AppName, link.FilePath, link.Page = v.AppName, v.FilePath, v.Page
			default:
				return nil
			}
			if link.FilePath == "" { return nil }
			const devicePrefix = "/storage/emulated/0/Note/"
			localPath := link.FilePath
			if h.notesPathPrefix != "" && strings.HasPrefix(link.FilePath, devicePrefix) {
				localPath = filepath.Join(h.notesPathPrefix, link.FilePath[len(devicePrefix):])
			}
			return map[string]interface{}{"Path": localPath, "Page": link.Page}
		},
	}

	tmpl, err := template.New("").Funcs(funcMap).ParseFS(templateFS, "templates/*.html")
	if err != nil { panic(fmt.Sprintf("failed to parse templates: %v", err)) }
	h.tmpl = tmpl

	h.mux.HandleFunc("GET /setup", h.handleSetup)
	h.mux.HandleFunc("POST /setup/save", h.handleSetupSave)
	h.mux.HandleFunc("GET /{$}", h.handleIndex)
	h.mux.HandleFunc("POST /tasks", h.handleCreateTask)
	h.mux.HandleFunc("POST /tasks/{id}/complete", h.handleCompleteTask)
	h.mux.HandleFunc("POST /tasks/bulk", h.handleBulkAction)
	h.mux.HandleFunc("POST /tasks/purge-completed", h.handlePurgeCompleted)
	h.mux.HandleFunc("GET /logs", h.handleLogs)
	h.mux.HandleFunc("GET /settings", h.handleSettings)
	h.mux.HandleFunc("POST /settings/save", h.handleSettingsSave)
	h.mux.HandleFunc("POST /settings/backfill-embeddings", h.handleBackfillEmbeddings)
	
	if h.noteDB != nil {
		h.mux.HandleFunc("POST /settings/mcp-tokens/create", h.handleMCPTokenCreate)
		h.mux.HandleFunc("POST /settings/mcp-tokens/revoke", h.handleMCPTokenRevoke)
	}
	
	h.mux.HandleFunc("GET /files", h.handleFiles)
	h.mux.HandleFunc("GET /files/supernote", h.handleFilesSupernote)
	h.mux.HandleFunc("GET /files/boox", h.handleFilesBoox)
	h.mux.HandleFunc("GET /search", h.handleSearch)
	h.mux.HandleFunc("POST /files/queue", h.handleFilesQueue)
	h.mux.HandleFunc("POST /files/skip", h.handleFilesSkip)
	h.mux.HandleFunc("POST /files/unskip", h.handleFilesUnskip)
	h.mux.HandleFunc("POST /files/force", h.handleFilesForce)
	h.mux.HandleFunc("GET /files/status", h.handleFilesStatus)
	h.mux.HandleFunc("GET /files/history", h.handleFilesHistory)
	h.mux.HandleFunc("GET /files/content", h.handleFilesContent)
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
	default:
		data := map[string]interface{}{
			"activeTab":  "files",
			"filesError": "No note sources configured. Add a Supernote or Boox source in Settings.",
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
	if detail := r.URL.Query().Get("detail"); detail != "" {
		data["detailPath"] = detail
	}
	if !h.notes.HasSupernoteSource() {
		data["filesError"] = "No Supernote source configured. Add a source in Settings."
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
	data["files"], data["relPath"], data["breadcrumbs"], data["filesTotalFiles"] = files, relPath, buildBreadcrumbs(relPath), total
	data["filesPage"], data["filesPerPage"] = page, perPage
	data["filesSort"], data["filesOrder"] = sortField, sortOrder
	data["filesTotalPages"] = (total + perPage - 1) / perPage
	if data["filesTotalPages"] == 0 {
		data["filesTotalPages"] = 1
	}
	h.renderTemplate(w, r, "files_supernote", data)
}

// handleFilesBoox renders the Boox-only Files view. Flat catalog list — no
// directory navigation — with per-note Title/Folder/Device/NoteType/Pages
// columns surfaced from BooxNoteSummary.
func (h *Handler) handleFilesBoox(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	data := map[string]interface{}{"activeTab": "files-boox"}
	if detail := r.URL.Query().Get("detail"); detail != "" {
		data["detailPath"] = detail
	}
	if !h.notes.HasBooxSource() {
		data["filesError"] = "No Boox source configured. Add a source in Settings."
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
	data["filesTotalPages"] = (total + perPage - 1) / perPage
	if data["filesTotalPages"] == 0 {
		data["filesTotalPages"] = 1
	}
	h.renderTemplate(w, r, "files_boox", data)
}

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{"activeTab": "search"}
	query, folder := strings.TrimSpace(r.URL.Query().Get("q")), strings.TrimSpace(r.URL.Query().Get("folder"))
	data["searchQuery"], data["searchFolder"] = query, folder
	if query != "" {
		results, _ := h.search.Search(r.Context(), query, folder)
		data["searchResults"] = results
	}
	h.renderTemplate(w, r, "search", data)
}

func (h *Handler) handleChat(w http.ResponseWriter, r *http.Request) {
	sessions, _ := h.search.ListSessions(r.Context())
	h.renderTemplate(w, r, "chat", map[string]interface{}{"chatSessions": sessions})
}

func (h *Handler) handleLogs(w http.ResponseWriter, r *http.Request) { h.renderTemplate(w, r, "logs", nil) }

func (h *Handler) handleSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg, _ := h.config.GetConfig(ctx)
	srcs, _ := h.config.ListSources(ctx)
	data := map[string]interface{}{"Config": cfg, "Sources": srcs, "activeTab": "settings"}
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
	if nt := r.URL.Query().Get("new_token"); nt != "" { data["NewMCPToken"] = nt }
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
	h.renderTemplate(w, r, "settings", data)
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
	created, err := h.tasks.Create(r.Context(), title, dueAt)
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
	if err := h.tasks.PurgeCompleted(r.Context()); err != nil {
		http.Error(w, "purge failed", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
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
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	case "ub-spc":
		// UB-as-SPC device-sync server config. Every field is restart-required
		// (the server is constructed once at startup), so UpdateConfig below
		// flags the restart banner automatically.
		if mode := r.FormValue("spc_mode"); mode == "client" || mode == "server" {
			cfg.SPCMode = mode
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
	case "general":
		cfg.EmbedEnabled = r.FormValue("embed_enabled") == "true"
		cfg.OllamaURL, cfg.OllamaEmbedModel = r.FormValue("ollama_url"), r.FormValue("ollama_embed_model")
		cfg.ChatEnabled = r.FormValue("chat_enabled") == "true"
		cfg.ChatAPIURL, cfg.ChatModel = r.FormValue("chat_api_url"), r.FormValue("chat_model")
		cfg.LogVerboseAPI = r.FormValue("log_verbose_api") == "true"
		if v := strings.TrimSpace(r.FormValue("caldav_collection_name")); v != "" {
			cfg.CalDAVCollectionName = v
		}
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
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	h.config.UpdateConfig(r.Context(), cfg)
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (h *Handler) handleBackfillEmbeddings(w http.ResponseWriter, r *http.Request) {
	if h.search == nil || !h.search.HasEmbeddingPipeline() {
		http.NotFound(w, r)
		return
	}
	h.search.TriggerBackfill(r.Context())
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
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
	if err != nil { http.Error(w, "not found", http.StatusNotFound); return }
	defer stream.Close()
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=300")
	io.Copy(w, stream)
}

func (h *Handler) handleBooxVersions(w http.ResponseWriter, r *http.Request) {
	v, _ := h.notes.ListVersions(r.Context(), r.URL.Query().Get("path"))
	if v == nil { v = []interface{}{} }
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
		h.renderTemplate(w, r, "settings", map[string]interface{}{"NewMCPToken": t})
		return
	}
	http.Redirect(w, r, "/settings?new_token="+url.QueryEscape(t)+"#mcp-tokens", http.StatusSeeOther)
}

func (h *Handler) handleMCPTokenRevoke(w http.ResponseWriter, r *http.Request) {
	mcpauth.RevokeToken(r.Context(), h.noteDB, r.FormValue("token_hash"))
	if r.Header.Get("HX-Request") == "true" { h.handleSettings(w, r); return }
	http.Redirect(w, r, "/settings#mcp-tokens", http.StatusSeeOther)
}

func (h *Handler) handleAsk(w http.ResponseWriter, r *http.Request) {
	var req struct { SessionID int `json:"session_id"`; Question string `json:"question"` }
	json.NewDecoder(r.Body).Decode(&req)
	responses, err := h.search.Ask(r.Context(), req.Question, req.SessionID)
	if err != nil { http.Error(w, "chat failed", http.StatusInternalServerError); return }
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	for resp := range responses {
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(resp))
		if flusher != nil { flusher.Flush() }
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

func (h *Handler) handleFilesContent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Query().Get("path")
	if path == "" {
		w.Write([]byte("[]"))
		return
	}
	if !h.validNotePath(path) {
		w.Write([]byte("[]"))
		return
	}
	docs, err := h.notes.GetContent(r.Context(), path)
	if err != nil {
		h.logger.Error("failed to get content", "path", path, "error", err)
		w.Write([]byte("[]"))
		return
	}
	json.NewEncoder(w).Encode(docs)
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
		if t, err := h.tasks.List(ctx); err == nil {
			data["tasks"] = t
		}
	}
	data["BooxNotesPath"] = h.booxNotesPath
	data["BooxImportPath"] = h.booxImportPath
	if h.config != nil {
		data["RestartRequired"] = h.config.IsRestartRequired()
	}
	data["chatEnabled"] = h.search != nil
	if h.notes != nil {
		data["HasSupernoteSource"] = h.notes.HasSupernoteSource()
		data["HasBooxSource"] = h.notes.HasBooxSource()
	}
	return data
}

type breadcrumb struct { Label, RelPath string }
func buildBreadcrumbs(p string) []breadcrumb {
	res := []breadcrumb{{Label: "Home", RelPath: ""}}
	if p == "" { return res }
	parts := strings.Split(p, "/")
	for i := range parts { res = append(res, breadcrumb{Label: parts[i], RelPath: strings.Join(parts[:i+1], "/")}) }
	return res
}

func safeRelPath(p string) (string, bool) {
	if p == "" { return "", true }
	c := filepath.Clean(p)
	if filepath.IsAbs(c) || strings.HasPrefix(c, "..") { return "", false }
	return c, true
}

// validNotePath returns true if path falls under one of the configured notes
// directories. Prevents arbitrary filesystem reads through path query params.
func (h *Handler) validNotePath(path string) bool {
	cleaned := filepath.Clean(path)
	if h.notesPathPrefix != "" && strings.HasPrefix(cleaned, h.notesPathPrefix) {
		return true
	}
	if h.booxNotesPath != "" && strings.HasPrefix(cleaned, h.booxNotesPath) {
		return true
	}
	return false
}
