package service

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"image/jpeg"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gosnote "github.com/jdkruzr/go-sn/note"
	"github.com/sysop/ultrabridge/internal/appconfig"
	"github.com/sysop/ultrabridge/internal/booxpipeline"
	"github.com/sysop/ultrabridge/internal/fnpath"
	"github.com/sysop/ultrabridge/internal/forestpdf"
	"github.com/sysop/ultrabridge/internal/forestrender"
	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/notestore"
	"github.com/sysop/ultrabridge/internal/processor"
	"github.com/sysop/ultrabridge/internal/search"
	"github.com/sysop/ultrabridge/internal/syncbridge"
	"github.com/sysop/ultrabridge/internal/syncstore"
)

// BooxStore is the interface required by the NoteService for Boox notes.
type BooxStore interface {
	ListNotes(ctx context.Context) ([]booxpipeline.BooxNoteEntry, error)
	GetVersions(ctx context.Context, path string) ([]booxpipeline.BooxVersion, error)
	GetNoteID(ctx context.Context, path string) (string, error)
	EnqueueJob(ctx context.Context, notePath string) error
	GetLatestJob(ctx context.Context, notePath string) (*booxpipeline.BooxJob, error)
	RetryAllFailed(ctx context.Context) (int64, error)
	DeleteNote(ctx context.Context, path string) error
	SkipNote(ctx context.Context, path, reason string) error
	UnskipNote(ctx context.Context, path string) error
	GetQueueStatus(ctx context.Context) (booxpipeline.QueueStatus, error)
	ListFolders(ctx context.Context) ([]booxpipeline.FolderCount, error)
	ListDevices(ctx context.Context) ([]booxpipeline.DeviceCount, error)
	ReconcileCreatedAtFromFilename(ctx context.Context) (int64, error)
	ListAutoNamedNotebooks(ctx context.Context) ([]booxpipeline.BooxNote, error)
	MoveNote(ctx context.Context, oldPath, newPath, newFolder string) error
}

// BooxImporter is the interface required by the NoteService for Boox imports.
type BooxImporter interface {
	ScanAndEnqueue(ctx context.Context, cfg booxpipeline.ImportConfig, logger *slog.Logger) booxpipeline.ImportResult
	MigrateImportedFiles(ctx context.Context, importPath, notesPath string, logger *slog.Logger) booxpipeline.MigrateResult
	Enqueue(ctx context.Context, notePath string) error
}

// BooxProcessor is the narrow handle the NoteService needs to start and stop
// the Boox pipeline worker on demand. Implemented by *booxpipeline.Processor.
// Kept separate from BooxImporter so tests can mock the controls without
// having to build importer plumbing too.
type BooxProcessor interface {
	Start(ctx context.Context) error
	Stop()
}

// FileScanner triggers a filesystem scan.
type FileScanner interface {
	ScanNow(ctx context.Context)
}

// EmbedIndex keeps a note's RAG embeddings in step with the filesystem: Delete
// drops every chunk (and evicts the in-memory cache), Rename repoints them to a
// new path. *rag.Store satisfies it. Used on Boox delete/move, where the boox
// store updates FTS content + inventory but not embeddings.
type EmbedIndex interface {
	Delete(ctx context.Context, path string) error
	Rename(ctx context.Context, oldPath, newPath string) error
}

// ForestNoteReader is the narrow surface the note service needs from the
// syncstore mirror to browse ForestNote notebooks and render their pages on the
// fly. *syncstore.Store satisfies it. Set via SetForestNoteReader only when a
// ForestNote source is active (nil → the FN Files tab is empty / render fails).
type ForestNoteReader interface {
	ListFolders(ctx context.Context) ([]syncstore.FolderRow, error)
	ListNotebooks(ctx context.Context) ([]syncstore.NotebookRow, error)
	NotebookPages(ctx context.Context, notebookID string) ([]syncstore.PageRef, error)
	NotebookMeta(ctx context.Context, notebookID string) (syncstore.NotebookRow, error)
	LivePage(ctx context.Context, pagePK string) (notebookID string, live bool, err error)
	LivePageStrokes(ctx context.Context, pagePK string) ([]syncstore.StrokeData, error)
	LivePageTextBoxes(ctx context.Context, pagePK string) ([]syncstore.TextBoxData, error)
	ListFolderContents(ctx context.Context, folderID string) ([]syncstore.FolderRow, []syncstore.NotebookRow, error)
	FolderPath(ctx context.Context, folderID string) ([]syncstore.FolderRow, error)
	SoftDeleteNotebook(ctx context.Context, notebookID string) ([]string, error)
	LiveNotebookPageIDs(ctx context.Context, notebookID string) ([]string, error)
	ListNotebookTextBoxes(ctx context.Context, notebookID string) ([]syncstore.TextBoxRef, error)
}

// ForestNoteReprocessor is the source-level write/render seam (it holds both the
// store and the bridge, which the narrow ForestNoteReader does not).
// *forestnote.Source satisfies it. Set via SetForestNoteReprocessor only when a
// ForestNote source is active.
type ForestNoteReprocessor interface {
	// ReprocessNotebook re-enqueues a notebook's live pages for a fresh
	// render → OCR → index → embed pass.
	ReprocessNotebook(ctx context.Context, notebookID string) error
	// EditTextBox authors a server-side text-box edit (relayed to devices) and
	// re-renders/re-indexes the affected page.
	EditTextBox(ctx context.Context, boxID, newText string) error
	// Status returns the sync bridge's current work snapshot for the
	// /files/status poller. Counters are monotonic since process start; the
	// caller diffs across polls if they want a rate. Implementations should
	// return a zero value rather than erroring when the underlying bridge is
	// nil (source not started).
	Status() syncbridge.Status
}

type noteService struct {
	noteStore     notestore.NoteStore
	proc          processor.Processor
	booxStore     BooxStore
	booxImporter  BooxImporter
	booxProc      BooxProcessor
	searchIndex   search.SearchIndex
	embedIndex    EmbedIndex            // optional; set via SetEmbedIndex
	fnReader      ForestNoteReader      // optional; set via SetForestNoteReader
	fnReprocessor ForestNoteReprocessor // optional; set via SetForestNoteReprocessor
	scanner       FileScanner
	noteDB        *sql.DB // for settings
	booxCachePath string
	booxNotesPath string
	logger        *slog.Logger
}

func NewNoteService(
	ns notestore.NoteStore,
	p processor.Processor,
	bs BooxStore,
	bi BooxImporter,
	bp BooxProcessor,
	si search.SearchIndex,
	scanner FileScanner,
	noteDB *sql.DB,
	booxCachePath string,
	booxNotesPath string,
	logger *slog.Logger,
) NoteService {
	return &noteService{
		noteStore:     ns,
		proc:          p,
		booxStore:     bs,
		booxImporter:  bi,
		booxProc:      bp,
		searchIndex:   si,
		scanner:       scanner,
		noteDB:        noteDB,
		booxCachePath: booxCachePath,
		booxNotesPath: booxNotesPath,
		logger:        logger,
	}
}

// SetEmbedIndex wires the RAG embedding store so deletes drop embeddings and
// moves repoint them. Nil-safe: pass a non-nil *rag.Store only when embedding is
// enabled (a typed-nil would be a non-nil interface and panic on use). Mirrors
// the SetDigestService pattern — keeps NewNoteService's signature (and all its
// callers) untouched.
func (s *noteService) SetEmbedIndex(d EmbedIndex) { s.embedIndex = d }

// SetForestNoteReader wires the syncstore mirror so the Files tab can browse
// synced ForestNote notebooks and render pages on the fly. Nil-safe in the same
// way as SetEmbedIndex; keeps NewNoteService's signature untouched.
func (s *noteService) SetForestNoteReader(r ForestNoteReader) { s.fnReader = r }

// SetForestNoteReprocessor wires the source's re-OCR trigger. Nil-safe in the
// same way as SetForestNoteReader.
func (s *noteService) SetForestNoteReprocessor(r ForestNoteReprocessor) { s.fnReprocessor = r }

// HasForestNoteSource reports whether a ForestNote source is active (a reader is
// wired). Lets the web layer render an empty state instead of failing.
func (s *noteService) HasForestNoteSource() bool { return s.fnReader != nil }

func (s *noteService) ListFiles(ctx context.Context, path string, sortField, order string, page, perPage int) ([]NoteFile, int, error) {
	var files []NoteFile

	// 1. Get Supernote files
	if s.noteStore != nil {
		snFiles, err := s.noteStore.List(ctx, path)
		if err != nil {
			s.logger.Error("list supernote files", "path", path, "error", err)
		} else {
			for _, f := range snFiles {
				files = append(files, mapSupernoteFile(f))
			}
		}
	}

	// 2. Merge Boox notes (only at root level per current implementation)
	if s.booxStore != nil && path == "" {
		booxNotes, err := s.booxStore.ListNotes(ctx)
		if err != nil {
			s.logger.Error("list boox notes", "error", err)
		} else {
			for _, bn := range booxNotes {
				files = append(files, mapBooxFile(bn))
			}
		}
	}

	// 3. Sorting
	if sortField == "" {
		sortField = "name"
	}
	if order == "" {
		order = "asc"
	}

	sort.Slice(files, func(i, j int) bool {
		if files[i].IsDir != files[j].IsDir {
			return files[i].IsDir
		}

		var cmp int
		switch sortField {
		case "created":
			cmp = compareTime(files[i].CreatedAt, files[j].CreatedAt)
		case "modified":
			cmp = compareTime(files[i].ModifiedAt, files[j].ModifiedAt)
		case "size":
			if files[i].SizeBytes == files[j].SizeBytes {
				cmp = strings.Compare(files[i].Name, files[j].Name)
			} else if files[i].SizeBytes < files[j].SizeBytes {
				cmp = -1
			} else {
				cmp = 1
			}
		default: // "name"
			cmp = strings.Compare(files[i].Name, files[j].Name)
		}

		if order == "desc" {
			return cmp > 0
		}
		return cmp < 0
	})

	// 4. Pagination
	totalFiles := len(files)
	if perPage <= 0 {
		perPage = 25
	}
	if page <= 0 {
		page = 1
	}

	totalPages := (totalFiles + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}

	start := (page - 1) * perPage
	if start > totalFiles {
		start = totalFiles
	}
	end := start + perPage
	if end > totalFiles {
		end = totalFiles
	}

	return files[start:end], totalFiles, nil
}

// ListSupernoteFiles returns only Supernote-sourced files (directory tree
// browser model). Sort/pagination matches ListFiles; no Boox notes are mixed
// in. Returns an empty page with total=0 when no Supernote store is wired.
func (s *noteService) ListSupernoteFiles(ctx context.Context, path, sortField, order string, page, perPage int) ([]NoteFile, int, error) {
	if s.noteStore == nil {
		return nil, 0, nil
	}
	snFiles, err := s.noteStore.List(ctx, path)
	if err != nil {
		s.logger.Error("list supernote files", "path", path, "error", err)
		return nil, 0, err
	}
	files := make([]NoteFile, 0, len(snFiles))
	for _, f := range snFiles {
		files = append(files, mapSupernoteFile(f))
	}
	sortNoteFiles(files, sortField, order)
	return paginateNoteFiles(files, page, perPage)
}

// computeBooxMoveDestPath returns the destination path when a Boox note at
// oldPath is moved into newFolder. newFolder may be empty (move to device-
// root, "unfiled") or a non-empty folder name. The path is computed by
// stripping the existing folder component (if any) and inserting the new
// one. Returns ("", error) if oldPath isn't under root or the structure
// isn't recognizable.
func computeBooxMoveDestPath(root, oldPath, newFolder string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("boox notes root not configured")
	}
	rel, err := filepath.Rel(root, oldPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path %q not under boox notes root %q", oldPath, root)
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	prefix := ""
	if len(parts) > 0 && parts[0] == "onyx" {
		prefix = "onyx"
		parts = parts[1:]
	}
	// Need at minimum [model, type, file] (3 parts) — anything less and we
	// can't safely re-parent into a folder.
	if len(parts) < 3 {
		return "", fmt.Errorf("path %q does not match {model}/{type}/[folder/]{file}", oldPath)
	}
	model, noteType, file := parts[0], parts[1], parts[len(parts)-1]
	var newRel string
	if newFolder == "" {
		// Unfiled: model/type/file
		newRel = filepath.Join(model, noteType, file)
	} else {
		newRel = filepath.Join(model, noteType, newFolder, file)
	}
	if prefix != "" {
		newRel = filepath.Join(prefix, newRel)
	}
	return filepath.Join(root, newRel), nil
}

// FolderFilterUnfiled is the sentinel value the web layer passes to filter
// to notes whose folder field is empty. URL semantics distinguish "no
// folder param" (All) from "folder=" (which is ambiguous with All on the
// HTTP side), so the Unfiled pill encodes itself as ?folder=__unfiled__.
const FolderFilterUnfiled = "__unfiled__"

// ListBooxNotes returns Boox-catalog rows with the richer per-note fields
// (Title, Folder, DeviceModel, NoteType, PageCount) that NoteFile flattens.
// Both device and folder are exact-match filters — empty means "all".
// They compose (supply both to narrow to that device+folder slice).
// Sort supports "title" (default), "folder", "pages", "size", "created",
// "modified". Returns empty/zero when no Boox store is wired.
func (s *noteService) ListBooxNotes(ctx context.Context, device, folder, sortField, order string, page, perPage int) ([]BooxNoteSummary, int, error) {
	if s.booxStore == nil {
		return nil, 0, nil
	}
	rows, err := s.booxStore.ListNotes(ctx)
	if err != nil {
		s.logger.Error("list boox notes", "error", err)
		return nil, 0, err
	}
	out := make([]BooxNoteSummary, 0, len(rows))
	filterUnfiled := folder == FolderFilterUnfiled
	for _, bn := range rows {
		if device != "" && bn.DeviceModel != device {
			continue
		}
		switch {
		case filterUnfiled:
			if bn.Folder != "" {
				continue
			}
		case folder != "":
			if bn.Folder != folder {
				continue
			}
		}
		out = append(out, mapBooxSummary(bn))
	}
	sortBooxNotes(out, sortField, order)

	totalFiles := len(out)
	if perPage <= 0 {
		perPage = 25
	}
	if page <= 0 {
		page = 1
	}
	totalPages := (totalFiles + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * perPage
	if start > totalFiles {
		start = totalFiles
	}
	end := start + perPage
	if end > totalFiles {
		end = totalFiles
	}
	return out[start:end], totalFiles, nil
}

// ListBooxFolders returns every unique Boox folder with its note count.
// Used by the Boox Files tab to build the folder-filter pill row.
// Returns nil when no Boox store is wired.
func (s *noteService) ListBooxFolders(ctx context.Context) ([]BooxFolder, error) {
	if s.booxStore == nil {
		return nil, nil
	}
	rows, err := s.booxStore.ListFolders(ctx)
	if err != nil {
		s.logger.Error("list boox folders", "error", err)
		return nil, err
	}
	out := make([]BooxFolder, 0, len(rows))
	for _, fc := range rows {
		out = append(out, BooxFolder{Folder: fc.Folder, Count: fc.Count})
	}
	return out, nil
}

// ListBooxDevices returns every unique Boox device_model with its note
// count, excluding the ".." legacy-import field-swap sentinel (filtered
// at the store layer). Returns nil when no Boox store is wired.
func (s *noteService) ListBooxDevices(ctx context.Context) ([]BooxDevice, error) {
	if s.booxStore == nil {
		return nil, nil
	}
	rows, err := s.booxStore.ListDevices(ctx)
	if err != nil {
		s.logger.Error("list boox devices", "error", err)
		return nil, err
	}
	out := make([]BooxDevice, 0, len(rows))
	for _, dc := range rows {
		out = append(out, BooxDevice{DeviceModel: dc.DeviceModel, Count: dc.Count})
	}
	return out, nil
}

// GetBooxNote returns the Boox-tab summary for a single path. Returns
// sql.ErrNoRows if the path is not in the Boox catalog.
func (s *noteService) GetBooxNote(ctx context.Context, path string) (BooxNoteSummary, error) {
	if s.booxStore == nil {
		return BooxNoteSummary{}, fmt.Errorf("boox store not available")
	}
	rows, err := s.booxStore.ListNotes(ctx)
	if err != nil {
		return BooxNoteSummary{}, err
	}
	for _, bn := range rows {
		if bn.Path == path {
			return mapBooxSummary(bn), nil
		}
	}
	return BooxNoteSummary{}, sql.ErrNoRows
}

// GetFile returns a single NoteFile by path, dispatching to the Boox or
// Supernote branch based on isBooxPath. Returns sql.ErrNoRows when the path
// is not found in the relevant store.
func (s *noteService) GetFile(ctx context.Context, path string) (NoteFile, error) {
	if s.isBooxPath(path) {
		if s.booxStore == nil {
			return NoteFile{}, fmt.Errorf("boox store not available")
		}
		notes, err := s.booxStore.ListNotes(ctx)
		if err != nil {
			return NoteFile{}, err
		}
		for _, bn := range notes {
			if bn.Path == path {
				return mapBooxFile(bn), nil
			}
		}
		return NoteFile{}, sql.ErrNoRows
	}
	if s.noteStore == nil {
		return NoteFile{}, fmt.Errorf("note store not available")
	}
	f, err := s.noteStore.Get(ctx, path)
	if err != nil {
		return NoteFile{}, err
	}
	if f == nil {
		return NoteFile{}, sql.ErrNoRows
	}
	return mapSupernoteFile(*f), nil
}

func (s *noteService) GetNoteDetails(ctx context.Context, path string) (interface{}, error) {
	if s.isBooxPath(path) {
		if s.booxStore == nil {
			return nil, fmt.Errorf("boox store not available")
		}
		return s.booxStore.GetLatestJob(ctx, path)
	}
	if s.proc == nil {
		return nil, fmt.Errorf("supernote processor not available")
	}
	return s.proc.GetJob(ctx, path)
}

func (s *noteService) GetContent(ctx context.Context, path string) (interface{}, error) {
	if s.searchIndex == nil {
		return nil, fmt.Errorf("search index not available")
	}
	return s.searchIndex.GetContent(ctx, path)
}

// GetNotePages returns the indexed OCR content for a note as typed page views,
// ordered by page, for the in-tab detail grid. A note that was never indexed
// yields an empty slice (not an error) so the caller can render an empty state.
func (s *noteService) GetNotePages(ctx context.Context, path string) ([]NotePageView, error) {
	if s.searchIndex == nil {
		return nil, nil
	}
	docs, err := s.searchIndex.GetContent(ctx, path)
	if err != nil {
		return nil, err
	}
	pages := make([]NotePageView, 0, len(docs))
	for _, d := range docs {
		pages = append(pages, NotePageView{
			Page:      d.Page,
			Source:    d.Source,
			BodyText:  d.BodyText,
			Keywords:  d.Keywords,
			TitleText: d.TitleText,
		})
	}
	return pages, nil
}

func (s *noteService) RenderPage(ctx context.Context, path string, pageIdx int) (io.ReadCloser, string, error) {
	// ForestNote paths are an opaque scheme (never a filesystem path), so branch
	// on them first. The page ULID is in the path; pageIdx is ignored.
	if fnpath.Is(path) {
		return s.renderForestNotePage(ctx, path)
	}
	if s.isBooxPath(path) {
		return s.renderBooxPage(ctx, path, pageIdx)
	}
	return s.renderSupernotePage(ctx, path, pageIdx)
}

// renderForestNotePage renders a single ForestNote page to JPEG on the fly from
// its live strokes in the syncstore mirror — no disk cache. The path is
// forestnote://{notebook_id}/{page_id}; the trailing segment is the page ULID.
func (s *noteService) renderForestNotePage(ctx context.Context, path string) (io.ReadCloser, string, error) {
	if s.fnReader == nil {
		return nil, "", fmt.Errorf("forestnote source not available")
	}
	pageID := fnpath.PageID(path)
	if pageID == "" {
		return nil, "", fmt.Errorf("forestnote path missing page id: %s", path)
	}
	// Liveness gate: LivePageStrokes returns an empty slice for BOTH a live-but-
	// blank page and a missing/soft-deleted one. Without this check the latter
	// would render a blank 200 (reachable via a stale tab or a search deep-link to
	// a since-deleted page). A live page with zero strokes still renders blank.
	_, live, err := s.fnReader.LivePage(ctx, pageID)
	if err != nil {
		return nil, "", fmt.Errorf("page liveness: %w", err)
	}
	if !live {
		return nil, "", fmt.Errorf("forestnote page not found: %s", pageID)
	}
	strokes, err := s.fnReader.LivePageStrokes(ctx, pageID)
	if err != nil {
		return nil, "", fmt.Errorf("load page strokes: %w", err)
	}
	boxes, err := s.fnReader.LivePageTextBoxes(ctx, pageID)
	if err != nil {
		return nil, "", fmt.Errorf("load page text boxes: %w", err)
	}
	img, err := forestrender.RenderPage(syncbridge.MapStrokes(strokes), syncbridge.MapTextBoxes(boxes))
	if err != nil {
		return nil, "", fmt.Errorf("render forestnote page: %w", err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		return nil, "", err
	}
	return io.NopCloser(&buf), "image/jpeg", nil
}

// ListForestNoteTree returns the live folder tree (roots) plus notebooks that
// sit at the root with no folder (unfiled). The tree is assembled purely from
// the two flat reads.
func (s *noteService) ListForestNoteTree(ctx context.Context) ([]ForestNoteTreeNode, []ForestNoteNotebook, error) {
	if s.fnReader == nil {
		return nil, nil, nil
	}
	folders, err := s.fnReader.ListFolders(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list folders: %w", err)
	}
	notebooks, err := s.fnReader.ListNotebooks(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list notebooks: %w", err)
	}
	roots, unfiled := buildForestNoteTree(folders, notebooks)
	return roots, unfiled, nil
}

// ListForestNotePages returns a notebook's display name and its live pages in
// order, each carrying the forestnote://{nb}/{page} render path.
func (s *noteService) ListForestNotePages(ctx context.Context, notebookID string) (string, []ForestNotePage, error) {
	if s.fnReader == nil {
		return "", nil, fmt.Errorf("forestnote source not available")
	}
	meta, err := s.fnReader.NotebookMeta(ctx, notebookID)
	if err != nil {
		return "", nil, err
	}
	refs, err := s.fnReader.NotebookPages(ctx, notebookID)
	if err != nil {
		return "", nil, fmt.Errorf("notebook pages: %w", err)
	}
	pages := make([]ForestNotePage, len(refs))
	for i, r := range refs {
		pages[i] = ForestNotePage{
			PageID:  r.ID,
			Path:    fnpath.Page(notebookID, r.ID),
			Ordinal: i,
		}
	}
	return meta.Name, pages, nil
}

// ListForestNoteFolder lists the direct children of folderID (empty = root) as a
// Supernote-style table (folders first, then notebooks), plus the breadcrumb
// trail to that folder. Each notebook carries a derived index Status. Returns
// empty results (no error) when no ForestNote source is wired.
func (s *noteService) ListForestNoteFolder(ctx context.Context, folderID, sortField, order string) ([]ForestNoteCrumb, []ForestNoteEntry, error) {
	if s.fnReader == nil {
		return nil, nil, nil
	}
	folders, notebooks, err := s.fnReader.ListFolderContents(ctx, folderID)
	if err != nil {
		return nil, nil, fmt.Errorf("folder contents: %w", err)
	}
	chain, err := s.fnReader.FolderPath(ctx, folderID)
	if err != nil {
		return nil, nil, fmt.Errorf("folder path: %w", err)
	}
	crumbs := make([]ForestNoteCrumb, len(chain))
	for i, f := range chain {
		crumbs[i] = ForestNoteCrumb{FolderID: f.ID, Name: f.Name}
	}

	// Per-notebook index coverage for the Status column. One bulk read over all
	// ForestNote content (one row per indexed page); cheap at current scale.
	indexed := s.forestNoteIndexedCounts(ctx)

	folderEntries := make([]ForestNoteEntry, 0, len(folders))
	for _, f := range folders {
		folderEntries = append(folderEntries, ForestNoteEntry{
			IsFolder: true, ID: f.ID, Name: f.Name,
			CreatedAt: f.CreatedAt, ModifiedAt: f.ModifiedAt,
		})
	}
	nbEntries := make([]ForestNoteEntry, 0, len(notebooks))
	for _, n := range notebooks {
		nbEntries = append(nbEntries, ForestNoteEntry{
			ID: n.ID, Name: n.Name, Path: fnpath.Notebook(n.ID),
			PageCount: n.PageCount, CreatedAt: n.CreatedAt, ModifiedAt: n.ModifiedAt,
			Status: forestNoteStatus(n.PageCount, indexed[n.ID]),
		})
	}
	sortForestNoteEntries(folderEntries, sortField, order)
	sortForestNoteEntries(nbEntries, sortField, order)

	// Folders always sort ahead of notebooks (mirrors Supernote's dirs-first table).
	entries := append(folderEntries, nbEntries...)
	return crumbs, entries, nil
}

// forestNoteIndexedCounts returns, per notebook id, the number of its pages that
// have non-empty OCR body text in the search index. Best-effort: a read error
// yields an empty map (Status then reports "blank"/"partial" conservatively).
func (s *noteService) forestNoteIndexedCounts(ctx context.Context) map[string]int {
	counts := map[string]int{}
	if s.searchIndex == nil {
		return counts
	}
	docs, err := s.searchIndex.GetContentByPrefix(ctx, fnpath.Scheme+"%")
	if err != nil {
		s.logger.Warn("forestnote index coverage read failed", "error", err)
		return counts
	}
	for path, d := range docs {
		if strings.TrimSpace(d.BodyText) != "" {
			counts[fnpath.NotebookID(path)]++
		}
	}
	return counts
}

// forestNoteStatus classifies a notebook's index coverage for the table badge.
func forestNoteStatus(pageCount, indexedPages int) string {
	switch {
	case pageCount == 0:
		return "blank"
	case indexedPages >= pageCount:
		return "indexed"
	default:
		return "partial"
	}
}

// sortForestNoteEntries sorts a single group (folders or notebooks) in place by
// the requested field. Default is name ascending; an unknown field falls back to
// name. order is "asc" or "desc".
func sortForestNoteEntries(entries []ForestNoteEntry, sortField, order string) {
	desc := order == "desc"
	less := func(i, j int) bool {
		switch sortField {
		case "created":
			return entries[i].CreatedAt < entries[j].CreatedAt
		case "modified":
			return entries[i].ModifiedAt < entries[j].ModifiedAt
		case "pages":
			return entries[i].PageCount < entries[j].PageCount
		default:
			return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if desc {
			return less(j, i)
		}
		return less(i, j)
	})
}

// GetForestNoteNotebookDetail returns a notebook's header metadata and its live
// pages enriched with OCR body text (one bulk index read keyed on the notebook's
// page-path prefix) for the click-through detail view.
func (s *noteService) GetForestNoteNotebookDetail(ctx context.Context, notebookID string) (ForestNoteNotebookDetail, error) {
	if s.fnReader == nil {
		return ForestNoteNotebookDetail{}, fmt.Errorf("forestnote source not available")
	}
	meta, err := s.fnReader.NotebookMeta(ctx, notebookID)
	if err != nil {
		return ForestNoteNotebookDetail{}, err
	}
	refs, err := s.fnReader.NotebookPages(ctx, notebookID)
	if err != nil {
		return ForestNoteNotebookDetail{}, fmt.Errorf("notebook pages: %w", err)
	}

	// OCR text for every page in one query (each page is indexed under its own
	// forestnote://{nb}/{page} path).
	var content map[string]search.NoteDocument
	if s.searchIndex != nil {
		content, err = s.searchIndex.GetContentByPrefix(ctx, fnpath.Notebook(notebookID)+"/%")
		if err != nil {
			s.logger.Warn("forestnote detail content read failed", "notebook", notebookID, "error", err)
		}
	}

	pages := make([]ForestNotePage, len(refs))
	for i, r := range refs {
		path := fnpath.Page(notebookID, r.ID)
		p := ForestNotePage{PageID: r.ID, Path: path, Ordinal: i}
		if doc, ok := content[path]; ok {
			p.BodyText = doc.BodyText
			p.Source = doc.Source
		}
		pages[i] = p
	}

	var folderPath []string
	if meta.FolderID != "" {
		if chain, err := s.fnReader.FolderPath(ctx, meta.FolderID); err == nil {
			for _, f := range chain {
				folderPath = append(folderPath, f.Name)
			}
		}
	}

	return ForestNoteNotebookDetail{
		NotebookID: meta.ID, Name: meta.Name,
		CreatedAt: meta.CreatedAt, ModifiedAt: meta.ModifiedAt,
		PageCount: len(pages), FolderPath: folderPath, Pages: pages,
	}, nil
}

// DeleteForestNoteNotebook soft-deletes a notebook (authoritative) and then
// de-indexes each of its pages from search + embeddings (best-effort). It is a
// UB-local hide, not a device tombstone — see syncstore.SoftDeleteNotebook.
func (s *noteService) DeleteForestNoteNotebook(ctx context.Context, notebookID string) error {
	if s.fnReader == nil {
		return fmt.Errorf("forestnote source not available")
	}
	pageIDs, err := s.fnReader.SoftDeleteNotebook(ctx, notebookID)
	if err != nil {
		return fmt.Errorf("soft-delete notebook: %w", err)
	}
	for _, pid := range pageIDs {
		path := fnpath.Page(notebookID, pid)
		if s.searchIndex != nil {
			if err := s.searchIndex.Delete(ctx, path); err != nil {
				s.logger.Warn("forestnote de-index failed", "path", path, "error", err)
			}
		}
		if s.embedIndex != nil {
			if err := s.embedIndex.Delete(ctx, path); err != nil {
				s.logger.Warn("forestnote embedding delete failed", "path", path, "error", err)
			}
		}
	}
	return nil
}

// ReprocessForestNoteNotebook re-enqueues a notebook's pages for re-OCR/re-index.
func (s *noteService) ReprocessForestNoteNotebook(ctx context.Context, notebookID string) error {
	if s.fnReprocessor == nil {
		return fmt.Errorf("forestnote reprocessing not available")
	}
	return s.fnReprocessor.ReprocessNotebook(ctx, notebookID)
}

// ListForestNoteTextBoxes returns a notebook's live text boxes (id, page, text)
// for discovery before an edit.
func (s *noteService) ListForestNoteTextBoxes(ctx context.Context, notebookID string) ([]syncstore.TextBoxRef, error) {
	if s.fnReader == nil {
		return nil, fmt.Errorf("forestnote source not available")
	}
	return s.fnReader.ListNotebookTextBoxes(ctx, notebookID)
}

// EditForestNoteTextBox authors a server-side edit of a text box's text (relayed
// to devices) and re-renders/re-indexes the affected page.
func (s *noteService) EditForestNoteTextBox(ctx context.Context, boxID, newText string) error {
	if s.fnReprocessor == nil {
		return fmt.Errorf("forestnote editing not available")
	}
	return s.fnReprocessor.EditTextBox(ctx, boxID, newText)
}

// ExportForestNoteNotebookPDF renders a notebook's live pages to JPEG (reusing
// the on-the-fly stroke renderer) and assembles them into a single PDF.
func (s *noteService) ExportForestNoteNotebookPDF(ctx context.Context, notebookID string) (io.ReadCloser, string, error) {
	if s.fnReader == nil {
		return nil, "", fmt.Errorf("forestnote source not available")
	}
	meta, err := s.fnReader.NotebookMeta(ctx, notebookID)
	if err != nil {
		return nil, "", err
	}
	refs, err := s.fnReader.NotebookPages(ctx, notebookID)
	if err != nil {
		return nil, "", fmt.Errorf("notebook pages: %w", err)
	}
	if len(refs) == 0 {
		return nil, "", fmt.Errorf("notebook has no pages to export")
	}

	pages := make([][]byte, 0, len(refs))
	for _, r := range refs {
		stream, _, err := s.renderForestNotePage(ctx, fnpath.Page(notebookID, r.ID))
		if err != nil {
			return nil, "", fmt.Errorf("render page %s: %w", r.ID, err)
		}
		data, err := io.ReadAll(stream)
		stream.Close()
		if err != nil {
			return nil, "", fmt.Errorf("read page %s: %w", r.ID, err)
		}
		pages = append(pages, data)
	}

	var buf bytes.Buffer
	if err := forestpdf.AssemblePDF(pages, &buf); err != nil {
		return nil, "", fmt.Errorf("assemble pdf: %w", err)
	}
	return io.NopCloser(&buf), sanitizeFilename(meta.Name, notebookID) + ".pdf", nil
}

// sanitizeFilename keeps a notebook name safe for a Content-Disposition filename,
// falling back to the notebook id when the name is empty or all-unsafe.
func sanitizeFilename(name, fallback string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == ' ', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" || out == "." {
		return fallback
	}
	return out
}

// buildForestNoteTree assembles the folder tree (pure). Notebooks whose folder_id
// is empty or references a missing/deleted folder are returned as "unfiled".
func buildForestNoteTree(folders []syncstore.FolderRow, notebooks []syncstore.NotebookRow) ([]ForestNoteTreeNode, []ForestNoteNotebook) {
	live := make(map[string]bool, len(folders))
	for _, f := range folders {
		live[f.ID] = true
	}
	nbByFolder := make(map[string][]ForestNoteNotebook)
	var unfiled []ForestNoteNotebook
	for _, n := range notebooks {
		nb := ForestNoteNotebook{
			NotebookID: n.ID,
			Name:       n.Name,
			Path:       fnpath.Notebook(n.ID),
			FolderID:   n.FolderID,
			PageCount:  n.PageCount,
		}
		if n.FolderID == "" || !live[n.FolderID] {
			unfiled = append(unfiled, nb)
			continue
		}
		nbByFolder[n.FolderID] = append(nbByFolder[n.FolderID], nb)
	}

	childFolders := make(map[string][]syncstore.FolderRow)
	var rootFolders []syncstore.FolderRow
	for _, f := range folders {
		if f.ParentFolderID == "" || !live[f.ParentFolderID] {
			rootFolders = append(rootFolders, f)
			continue
		}
		childFolders[f.ParentFolderID] = append(childFolders[f.ParentFolderID], f)
	}

	var build func(f syncstore.FolderRow) ForestNoteTreeNode
	build = func(f syncstore.FolderRow) ForestNoteTreeNode {
		node := ForestNoteTreeNode{
			FolderID:  f.ID,
			Name:      f.Name,
			Notebooks: nbByFolder[f.ID],
		}
		for _, c := range childFolders[f.ID] {
			node.Children = append(node.Children, build(c))
		}
		return node
	}
	roots := make([]ForestNoteTreeNode, 0, len(rootFolders))
	for _, f := range rootFolders {
		roots = append(roots, build(f))
	}
	return roots, unfiled
}

func (s *noteService) renderBooxPage(ctx context.Context, path string, pageIdx int) (io.ReadCloser, string, error) {
	if s.booxStore == nil {
		return nil, "", fmt.Errorf("boox store not available")
	}
	noteID, err := s.booxStore.GetNoteID(ctx, path)
	if err != nil || noteID == "" {
		return nil, "", fmt.Errorf("note id not found for path: %s", path)
	}
	cachePath := filepath.Join(s.booxCachePath, noteID, fmt.Sprintf("page_%d.jpg", pageIdx))
	f, err := os.Open(cachePath)
	if err != nil {
		return nil, "", fmt.Errorf("page not rendered: %w", err)
	}
	return f, "image/jpeg", nil
}

func (s *noteService) renderSupernotePage(ctx context.Context, path string, pageIdx int) (io.ReadCloser, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()

	n, err := gosnote.Load(f)
	if err != nil {
		return nil, "", err
	}

	if pageIdx < 0 || pageIdx >= len(n.Pages) {
		return nil, "", fmt.Errorf("page index %d out of range", pageIdx)
	}

	p := n.Pages[pageIdx]
	tp, err := n.TotalPathData(p)
	if err != nil || tp == nil {
		return nil, "", fmt.Errorf("no stroke data for page")
	}
	pageW, pageH := n.PageDimensions(p)
	objs, err := gosnote.DecodeObjects(tp, pageW, pageH)
	if err != nil {
		return nil, "", err
	}
	img := gosnote.RenderObjects(objs, pageW, pageH, nil)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		return nil, "", err
	}

	return io.NopCloser(&buf), "image/jpeg", nil
}

func (s *noteService) HasSupernoteSource() bool {
	return s.noteStore != nil
}

func (s *noteService) HasBooxSource() bool {
	return s.booxStore != nil
}

func (s *noteService) ListVersions(ctx context.Context, path string) (interface{}, error) {
	if !s.isBooxPath(path) || s.booxStore == nil {
		return nil, nil
	}
	return s.booxStore.GetVersions(ctx, path)
}

func (s *noteService) ScanFiles(ctx context.Context) error {
	if s.scanner != nil {
		s.scanner.ScanNow(ctx)
	}
	return nil
}

func (s *noteService) Enqueue(ctx context.Context, path string, force bool) error {
	if s.isBooxPath(path) {
		if s.booxStore == nil {
			return fmt.Errorf("boox store not available")
		}
		return s.booxStore.EnqueueJob(ctx, path)
	}
	if s.proc == nil {
		return fmt.Errorf("supernote processor not available")
	}
	return s.proc.Enqueue(ctx, path)
}

func (s *noteService) Skip(ctx context.Context, path, reason string) error {
	if s.isBooxPath(path) {
		if s.booxStore == nil {
			return fmt.Errorf("boox store not available")
		}
		return s.booxStore.SkipNote(ctx, path, reason)
	}
	if s.proc == nil {
		return fmt.Errorf("supernote processor not available")
	}
	return s.proc.Skip(ctx, path, reason)
}

func (s *noteService) Unskip(ctx context.Context, path string) error {
	if s.isBooxPath(path) {
		if s.booxStore == nil {
			return fmt.Errorf("boox store not available")
		}
		return s.booxStore.UnskipNote(ctx, path)
	}
	if s.proc == nil {
		return fmt.Errorf("supernote processor not available")
	}
	return s.proc.Unskip(ctx, path)
}

func (s *noteService) RetryFailed(ctx context.Context) error {
	if s.booxStore != nil {
		_, _ = s.booxStore.RetryAllFailed(ctx)
	}
	return nil
}

func (s *noteService) DeleteNote(ctx context.Context, path string) error {
	if s.isBooxPath(path) {
		if s.booxStore == nil {
			return fmt.Errorf("boox store not available")
		}
		noteID, _ := s.booxStore.GetNoteID(ctx, path)
		if err := s.booxStore.DeleteNote(ctx, path); err != nil {
			return err
		}
		// DeleteNote clears note_content (FTS) but not note_embeddings; drop the
		// RAG embeddings too so the note stops surfacing in chat retrieval.
		// Best-effort: the note is already gone, so a failure is logged not fatal.
		if s.embedIndex != nil {
			if err := s.embedIndex.Delete(ctx, path); err != nil {
				s.logger.Warn("delete boox embeddings", "path", path, "error", err)
			}
		}
		if noteID != "" && s.booxCachePath != "" {
			os.RemoveAll(filepath.Join(s.booxCachePath, noteID))
		}
		// Remove the source file and its .versions archive directory.
		// Leaving them on disk means the scan-untracked maintenance button
		// would re-enqueue the note, undoing the delete.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			s.logger.Warn("delete boox file", "path", path, "error", err)
		}
		if s.booxNotesPath != "" {
			if rel, err := filepath.Rel(s.booxNotesPath, path); err == nil {
				relDir := filepath.Dir(rel)
				nameNoExt := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
				versionsDir := filepath.Join(s.booxNotesPath, ".versions", relDir, nameNoExt)
				if _, statErr := os.Stat(versionsDir); statErr == nil {
					if err := os.RemoveAll(versionsDir); err != nil {
						s.logger.Warn("delete versions dir", "dir", versionsDir, "error", err)
					}
				}
			}
		}
		return nil
	}
	return nil
}

func (s *noteService) BulkDelete(ctx context.Context, paths []string) error {
	for _, p := range paths {
		_ = s.DeleteNote(ctx, p)
	}
	return nil
}

func (s *noteService) ReconcileBooxCreatedAt(ctx context.Context) (int64, error) {
	if s.booxStore == nil {
		return 0, fmt.Errorf("boox store not available")
	}
	return s.booxStore.ReconcileCreatedAtFromFilename(ctx)
}

// MoveBooxNote moves a single Boox note to destFolder ("" means unfiled).
// Renames the source file on disk, moves the .versions/ archive, and
// updates the boox_notes / boox_jobs / note_content rows. Returns an
// error if the source isn't a Boox path, the destination already exists,
// or the rename fails.
func (s *noteService) MoveBooxNote(ctx context.Context, path, destFolder string) error {
	if !s.isBooxPath(path) {
		return fmt.Errorf("not a boox path: %q", path)
	}
	if s.booxStore == nil {
		return fmt.Errorf("boox store not available")
	}
	// Treat the unfiled sentinel coming from the form layer as "no folder."
	if destFolder == FolderFilterUnfiled {
		destFolder = ""
	}
	newPath, err := computeBooxMoveDestPath(s.booxNotesPath, path, destFolder)
	if err != nil {
		return err
	}
	if newPath == path {
		return nil // already there, no-op
	}
	if _, err := os.Stat(newPath); err == nil {
		return fmt.Errorf("destination already exists: %q", newPath)
	}
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		return fmt.Errorf("create dest dir: %w", err)
	}
	if err := os.Rename(path, newPath); err != nil {
		return fmt.Errorf("rename file: %w", err)
	}
	// Move the .versions archive directory if it exists. Best-effort: a
	// failure here doesn't roll back the file rename, but is logged.
	if rel, err := filepath.Rel(s.booxNotesPath, path); err == nil {
		oldVersionsDir := filepath.Join(s.booxNotesPath, ".versions",
			filepath.Dir(rel),
			strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
		if _, statErr := os.Stat(oldVersionsDir); statErr == nil {
			if newRel, err := filepath.Rel(s.booxNotesPath, newPath); err == nil {
				newVersionsDir := filepath.Join(s.booxNotesPath, ".versions",
					filepath.Dir(newRel),
					strings.TrimSuffix(filepath.Base(newPath), filepath.Ext(newPath)))
				if err := os.MkdirAll(filepath.Dir(newVersionsDir), 0o755); err == nil {
					if err := os.Rename(oldVersionsDir, newVersionsDir); err != nil {
						s.logger.Warn("move versions dir", "old", oldVersionsDir, "new", newVersionsDir, "error", err)
					}
				}
			}
		}
	}
	if err := s.booxStore.MoveNote(ctx, path, newPath, destFolder); err != nil {
		// File moved on disk but DB update failed — log loudly. The next
		// scan-untracked run will re-add the destination as a new row.
		s.logger.Error("boox move: db update failed after file rename", "old", path, "new", newPath, "error", err)
		return fmt.Errorf("update db: %w", err)
	}
	// MoveNote repoints boox_notes/boox_jobs/note_content (FTS) but not
	// note_embeddings; repoint the RAG vectors + cache too so the moved note
	// stays retrievable at its new path. Best-effort: the move already happened.
	if s.embedIndex != nil {
		if err := s.embedIndex.Rename(ctx, path, newPath); err != nil {
			s.logger.Warn("move boox embeddings", "old", path, "new", newPath, "error", err)
		}
	}
	return nil
}

// BulkMoveBooxNotes moves multiple notes; returns counts and the first
// error if any (other paths still attempted).
func (s *noteService) BulkMoveBooxNotes(ctx context.Context, paths []string, destFolder string) (moved, failed int, err error) {
	for _, p := range paths {
		if e := s.MoveBooxNote(ctx, p, destFolder); e != nil {
			s.logger.Warn("bulk move: skipping", "path", p, "error", e)
			failed++
			if err == nil {
				err = e
			}
			continue
		}
		moved++
	}
	return moved, failed, err
}

// ScanAndEnqueueUntracked walks the Boox notes directory, finds .note/.pdf
// files that have no corresponding boox_notes row, and enqueues a job for
// each. Returns (scanned, enqueued, err). Files already tracked are left
// alone — this is the recovery path for files that landed on disk via
// something other than the WebDAV upload hook (rsync, manual copy, or
// pre-fix WebDAV uploads).
func (s *noteService) ScanAndEnqueueUntracked(ctx context.Context) (int, int, error) {
	if s.booxStore == nil {
		return 0, 0, fmt.Errorf("boox store not available")
	}
	if s.booxNotesPath == "" {
		return 0, 0, fmt.Errorf("boox notes path not configured")
	}

	// Snapshot tracked paths into a set.
	notes, err := s.booxStore.ListNotes(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("list tracked: %w", err)
	}
	tracked := make(map[string]struct{}, len(notes))
	for _, n := range notes {
		tracked[n.Path] = struct{}{}
	}

	var scanned, enqueued int
	walkErr := filepath.Walk(s.booxNotesPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if strings.HasPrefix(info.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(info.Name()))
		if ext != ".note" && ext != ".pdf" {
			return nil
		}
		scanned++
		if _, ok := tracked[path]; ok {
			return nil
		}
		if err := s.booxStore.EnqueueJob(ctx, path); err != nil {
			s.logger.Warn("enqueue untracked", "path", path, "error", err)
			return nil
		}
		enqueued++
		return nil
	})
	if walkErr != nil {
		return scanned, enqueued, fmt.Errorf("walk: %w", walkErr)
	}
	return scanned, enqueued, nil
}

// DeleteAutoNamedNotebooks deletes Boox firmware default-named notes
// (e.g. "Notebook-3.note") — DB row, source file, .versions archive
// directory, and rendered cache. Returns counts (deleted rows, deleted
// files, deleted version dirs) and the first error encountered, but
// continues past per-row errors so a single bad path doesn't block the
// rest of the cleanup.
func (s *noteService) DeleteAutoNamedNotebooks(ctx context.Context) (int64, int64, int64, error) {
	if s.booxStore == nil {
		return 0, 0, 0, fmt.Errorf("boox store not available")
	}
	notes, err := s.booxStore.ListAutoNamedNotebooks(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("list candidates: %w", err)
	}
	var rowsDeleted, filesDeleted, versionsDeleted int64
	for _, n := range notes {
		if err := os.Remove(n.Path); err == nil {
			filesDeleted++
		} else if !os.IsNotExist(err) {
			s.logger.Warn("delete boox file", "path", n.Path, "error", err)
		}
		if s.booxNotesPath != "" {
			if rel, err := filepath.Rel(s.booxNotesPath, n.Path); err == nil {
				relDir := filepath.Dir(rel)
				nameNoExt := strings.TrimSuffix(filepath.Base(n.Path), filepath.Ext(n.Path))
				versionsDir := filepath.Join(s.booxNotesPath, ".versions", relDir, nameNoExt)
				if _, statErr := os.Stat(versionsDir); statErr == nil {
					if err := os.RemoveAll(versionsDir); err != nil {
						s.logger.Warn("delete versions dir", "dir", versionsDir, "error", err)
					} else {
						versionsDeleted++
					}
				}
			}
		}
		if err := s.booxStore.DeleteNote(ctx, n.Path); err != nil {
			s.logger.Warn("delete boox row", "path", n.Path, "error", err)
			continue
		}
		rowsDeleted++
		if n.NoteID != "" && s.booxCachePath != "" {
			os.RemoveAll(filepath.Join(s.booxCachePath, n.NoteID))
		}
	}
	return rowsDeleted, filesDeleted, versionsDeleted, nil
}

func (s *noteService) StartProcessor(ctx context.Context) error {
	if s.proc != nil {
		return s.proc.Start(ctx)
	}
	return nil
}

func (s *noteService) StopProcessor(ctx context.Context) error {
	if s.proc != nil {
		return s.proc.Stop()
	}
	return nil
}

// StartBooxProcessor starts the Boox pipeline worker. Nil-safe: returns nil
// when no Boox source is wired.
func (s *noteService) StartBooxProcessor(ctx context.Context) error {
	if s.booxProc != nil {
		return s.booxProc.Start(ctx)
	}
	return nil
}

// StopBooxProcessor signals shutdown on the Boox worker. Nil-safe.
func (s *noteService) StopBooxProcessor(ctx context.Context) error {
	if s.booxProc != nil {
		s.booxProc.Stop()
	}
	return nil
}

func (s *noteService) GetProcessorStatus(ctx context.Context) (EmbeddingJobStatus, error) {
	status := EmbeddingJobStatus{}

	if s.proc != nil {
		procStatus := s.proc.Status()
		status.Running = procStatus.Running
		status.PendingCount = procStatus.Pending
		status.InFlightCount = procStatus.InFlight
		status.ProcessedCount = procStatus.Done
		status.FailedCount = procStatus.Failed
	}

	if s.booxStore != nil {
		booxStatus, err := s.booxStore.GetQueueStatus(ctx)
		if err == nil {
			status.Boox = &booxStatus
		} else {
			s.logger.Error("failed to get boox queue status", "error", err)
		}
	}

	// ForestNote sync-bridge counters surface here when a source is wired.
	// Status() is safe to call when the underlying bridge is nil (returns the
	// zero value); we only attach the block when there's an actual bridge,
	// so the JSON omitempty hides the field for non-FN-wired deployments.
	if s.fnReprocessor != nil {
		bs := s.fnReprocessor.Status()
		// Capacity==0 means the bridge was never constructed (zero value
		// passthrough); treat as "no FN block to render" so the UI stays
		// silent until the source is actually live.
		if bs.Capacity > 0 || bs.Processed > 0 || bs.Dropped > 0 || bs.Pending > 0 || bs.InFlight > 0 {
			status.ForestNote = &ForestNoteQueueStatus{
				Pending:   bs.Pending,
				InFlight:  bs.InFlight,
				Processed: bs.Processed,
				Dropped:   bs.Dropped,
				Capacity:  bs.Capacity,
			}
		}
	}

	return status, nil
}

func (s *noteService) ImportFiles(ctx context.Context) error {
	if s.booxImporter == nil || s.noteDB == nil {
		return fmt.Errorf("boox importer or database not available")
	}
	importPath, _ := notedb.GetSetting(ctx, s.noteDB, appconfig.KeyBooxImportPath)
	if importPath == "" {
		return fmt.Errorf("no import path configured")
	}
	importNotes, _ := notedb.GetSetting(ctx, s.noteDB, appconfig.KeyBooxImportNotes)
	importPDFs, _ := notedb.GetSetting(ctx, s.noteDB, appconfig.KeyBooxImportPDFs)
	onyxPaths, _ := notedb.GetSetting(ctx, s.noteDB, appconfig.KeyBooxImportOnyxPaths)

	cfg := booxpipeline.ImportConfig{
		ImportPath:  importPath,
		ImportNotes: importNotes == "true",
		ImportPDFs:  importPDFs == "true",
		OnyxPaths:   onyxPaths == "true",
	}

	s.booxImporter.ScanAndEnqueue(ctx, cfg, s.logger)
	return nil
}

func (s *noteService) MigrateImports(ctx context.Context) error {
	if s.booxImporter == nil || s.noteDB == nil {
		return fmt.Errorf("boox importer or database not available")
	}
	importPath, _ := notedb.GetSetting(ctx, s.noteDB, appconfig.KeyBooxImportPath)
	if importPath == "" || s.booxNotesPath == "" {
		return fmt.Errorf("import or notes path not configured")
	}

	go func() {
		s.booxImporter.MigrateImportedFiles(context.Background(), importPath, s.booxNotesPath, s.logger)
	}()
	return nil
}

// Helpers

func (s *noteService) isBooxPath(path string) bool {
	if s.booxStore == nil {
		return false
	}
	if s.booxNotesPath != "" && strings.HasPrefix(path, s.booxNotesPath) {
		return true
	}
	// Check for settings-based import path too if needed, but for now
	// let's rely on booxNotesPath which covers the main store.
	// Heuristic: if it's explicitly boox-y or we have no supernote store
	return (strings.Contains(path, "boox") || strings.HasSuffix(path, ".note")) && s.noteStore == nil
}

func mapSupernoteFile(f notestore.NoteFile) NoteFile {
	return NoteFile{
		Name:       f.Name,
		Path:       f.Path,
		RelPath:    f.RelPath,
		IsDir:      f.IsDir,
		FileType:   string(f.FileType),
		SizeBytes:  f.SizeBytes,
		CreatedAt:  f.CTime,
		ModifiedAt: f.MTime,
		Source:     "supernote",
		DeviceInfo: &f.DeviceInfo,
		JobStatus:  f.JobStatus,
	}
}

func mapBooxFile(bn booxpipeline.BooxNoteEntry) NoteFile {
	var mtime time.Time
	if bn.UpdatedAt > 0 {
		mtime = time.UnixMilli(bn.UpdatedAt)
	}
	var ctime time.Time
	if bn.CreatedAt > 0 {
		ctime = time.UnixMilli(bn.CreatedAt)
	}
	var sizeBytes int64
	if info, err := os.Stat(bn.Path); err == nil {
		sizeBytes = info.Size()
	}
	deviceInfo := bn.DeviceModel
	if bn.Folder != "" {
		deviceInfo += " / " + bn.Folder
	}

	return NoteFile{
		Name:       bn.Title,
		Path:       bn.Path,
		RelPath:    bn.Title,
		IsDir:      false,
		FileType:   "note",
		SizeBytes:  sizeBytes,
		CreatedAt:  ctime,
		ModifiedAt: mtime,
		Source:     "boox",
		DeviceInfo: &deviceInfo,
		JobStatus:  bn.JobStatus,
	}
}

func mapBooxSummary(bn booxpipeline.BooxNoteEntry) BooxNoteSummary {
	var mtime, ctime time.Time
	if bn.UpdatedAt > 0 {
		mtime = time.UnixMilli(bn.UpdatedAt)
	}
	if bn.CreatedAt > 0 {
		ctime = time.UnixMilli(bn.CreatedAt)
	}
	var size int64
	if info, err := os.Stat(bn.Path); err == nil {
		size = info.Size()
	}
	return BooxNoteSummary{
		Path:        bn.Path,
		NoteID:      bn.NoteID,
		Title:       bn.Title,
		Filename:    filepath.Base(bn.Path),
		DeviceModel: bn.DeviceModel,
		NoteType:    bn.NoteType,
		Folder:      bn.Folder,
		PageCount:   bn.PageCount,
		SizeBytes:   size,
		CreatedAt:   ctime,
		ModifiedAt:  mtime,
		JobStatus:   bn.JobStatus,
	}
}

// sortNoteFiles sorts files in place: directories first, then by the named
// field. Matches the ordering ListFiles applies to its merged result.
func sortNoteFiles(files []NoteFile, sortField, order string) {
	if sortField == "" {
		sortField = "name"
	}
	if order == "" {
		order = "asc"
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].IsDir != files[j].IsDir {
			return files[i].IsDir
		}
		var cmp int
		switch sortField {
		case "created":
			cmp = compareTime(files[i].CreatedAt, files[j].CreatedAt)
		case "modified":
			cmp = compareTime(files[i].ModifiedAt, files[j].ModifiedAt)
		case "size":
			if files[i].SizeBytes == files[j].SizeBytes {
				cmp = strings.Compare(files[i].Name, files[j].Name)
			} else if files[i].SizeBytes < files[j].SizeBytes {
				cmp = -1
			} else {
				cmp = 1
			}
		default:
			cmp = strings.Compare(files[i].Name, files[j].Name)
		}
		if order == "desc" {
			return cmp > 0
		}
		return cmp < 0
	})
}

// paginateNoteFiles returns the page slice and the total count before paging.
func paginateNoteFiles(files []NoteFile, page, perPage int) ([]NoteFile, int, error) {
	total := len(files)
	if perPage <= 0 {
		perPage = 25
	}
	if page <= 0 {
		page = 1
	}
	totalPages := (total + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	return files[start:end], total, nil
}

// sortBooxNotes sorts in place by the requested field. Supports "title",
// "folder", "pages", "size", "created", "modified". Default "title" asc.
func sortBooxNotes(rows []BooxNoteSummary, sortField, order string) {
	if sortField == "" {
		sortField = "created"
	}
	if order == "" {
		order = "desc"
	}
	sort.Slice(rows, func(i, j int) bool {
		var cmp int
		switch sortField {
		case "folder":
			cmp = strings.Compare(rows[i].Folder, rows[j].Folder)
			if cmp == 0 {
				cmp = strings.Compare(rows[i].Title, rows[j].Title)
			}
		case "pages":
			cmp = rows[i].PageCount - rows[j].PageCount
			if cmp == 0 {
				cmp = strings.Compare(rows[i].Title, rows[j].Title)
			}
		case "size":
			if rows[i].SizeBytes < rows[j].SizeBytes {
				cmp = -1
			} else if rows[i].SizeBytes > rows[j].SizeBytes {
				cmp = 1
			} else {
				cmp = strings.Compare(rows[i].Title, rows[j].Title)
			}
		case "created":
			cmp = compareTime(rows[i].CreatedAt, rows[j].CreatedAt)
		case "modified":
			cmp = compareTime(rows[i].ModifiedAt, rows[j].ModifiedAt)
		default:
			cmp = strings.Compare(rows[i].Title, rows[j].Title)
		}
		if order == "desc" {
			return cmp > 0
		}
		return cmp < 0
	})
}

func compareTime(a, b time.Time) int {
	if a.Equal(b) {
		return 0
	}
	if a.Before(b) {
		return -1
	}
	return 1
}
