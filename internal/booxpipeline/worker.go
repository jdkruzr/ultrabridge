package booxpipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/sysop/ultrabridge/internal/booxnote"
	"github.com/sysop/ultrabridge/internal/booxrender"
	"github.com/sysop/ultrabridge/internal/pdfrender"
	"github.com/sysop/ultrabridge/internal/processor"
	"github.com/sysop/ultrabridge/internal/rag"
	ubwebdav "github.com/sysop/ultrabridge/internal/webdav"
)

// OCRer abstracts the OCR capability. processor.OCRClient satisfies this interface.
type OCRer interface {
	Recognize(ctx context.Context, jpegData []byte, prompt string) (string, error)
}

// ContentDeleter removes indexed content for a note path. search.Store satisfies this.
type ContentDeleter interface {
	Delete(ctx context.Context, path string) error
}

// TodoItem represents a to-do extracted from red ink on a page.
type TodoItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// WorkerConfig configures the Boox processing worker.
type WorkerConfig struct {
	Indexer        processor.Indexer
	ContentDeleter ContentDeleter // for clearing old content on re-process
	OCR            OCRer          // nil = OCR disabled
	OCRPrompt      func() string  // returns current OCR prompt; nil = use default
	TodoEnabled    func() bool    // returns whether red ink to-do extraction is on
	TodoPrompt     func() string  // returns current to-do extraction prompt
	OnTodosFound   func(ctx context.Context, notePath string, todos []TodoItem) // callback for extracted todos
	CachePath      string         // base dir for rendered page cache
	Embedder       rag.Embedder   // nil = embedding disabled
	EmbedModel     string         // model name for note_embeddings.model column
	EmbedStore     rag.EmbedStore // nil = embedding disabled
}

func (p *Processor) executeJob(ctx context.Context, job *BooxJob) error {
	ext := strings.ToLower(filepath.Ext(job.NotePath))
	switch ext {
	case ".note":
		return p.executeNoteJob(ctx, job)
	case ".pdf":
		return p.executePDFJob(ctx, job)
	default:
		return fmt.Errorf("unsupported file type: %s", ext)
	}
}

func (p *Processor) executeNoteJob(ctx context.Context, job *BooxJob) error {
	notePath := job.NotePath

	// 1. Open and compute file hash for dedup.
	f, err := os.Open(notePath)
	if err != nil {
		return fmt.Errorf("open note: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat note: %w", err)
	}

	h := sha256.New()
	io.Copy(h, f)
	fileHash := hex.EncodeToString(h.Sum(nil))

	// Early exit: if we've already processed this exact content at this
	// path, skip re-parsing, re-rendering, re-OCRing. Covers WebDAV
	// re-uploads of unchanged notes (common when a Boox device re-syncs).
	if done, _ := p.store.HasDoneJobWithHash(ctx, notePath, fileHash); done {
		p.logger.Info("boox: skipping re-process, hash unchanged", "path", notePath)
		return nil
	}

	// 2. Parse the .note file.
	f.Seek(0, io.SeekStart)
	note, err := booxnote.Open(f, info.Size())
	if err != nil {
		return fmt.Errorf("parse note: %w", err)
	}

	// 3. Extract path metadata. Preserve existing metadata (from importer) if present.
	deviceModel, noteType, folder := p.resolveMetadata(ctx, notePath)

	// 4. Update boox_notes row (note.NoteID is the top-level directory name from the ZIP).
	if err := p.store.UpsertNote(ctx, notePath, note.NoteID, note.Title, deviceModel, noteType, folder, len(note.Pages), fileHash, NoteCreatedAt(note.Title, notePath)); err != nil {
		return fmt.Errorf("upsert note: %w", err)
	}

	// 5. Clear old cached renders and indexed content for re-processing.
	cacheDir := filepath.Join(p.cfg.CachePath, note.NoteID)
	os.RemoveAll(cacheDir)
	os.MkdirAll(cacheDir, 0755)
	if p.cfg.ContentDeleter != nil {
		p.cfg.ContentDeleter.Delete(ctx, notePath)
	}

	// 6. Render, OCR, and index each page.
	for i, page := range note.Pages {
		img, err := booxrender.RenderPage(page)
		if err != nil {
			return fmt.Errorf("render page %d: %w", i, err)
		}

		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
			return fmt.Errorf("encode page %d: %w", i, err)
		}

		cachePath := filepath.Join(cacheDir, fmt.Sprintf("page_%d.jpg", i))
		if err := os.WriteFile(cachePath, buf.Bytes(), 0644); err != nil {
			return fmt.Errorf("cache page %d: %w", i, err)
		}

		var ocrText string
		if p.cfg.OCR != nil {
			prompt := ""
			if p.cfg.OCRPrompt != nil {
				prompt = p.cfg.OCRPrompt()
			}
			text, err := p.cfg.OCR.Recognize(ctx, buf.Bytes(), prompt)
			if err != nil {
				return fmt.Errorf("ocr page %d: %w", i, err)
			}
			ocrText = text
		}

		titleText := ""
		keywords := ""
		if i == 0 {
			titleText = note.Title
		}
		if err := p.cfg.Indexer.IndexPage(ctx, notePath, i, "api", ocrText, titleText, keywords); err != nil {
			return fmt.Errorf("index page %d: %w", i, err)
		}
		// Embed page text if embedder is available (chunked; long pages → many vectors)
		if p.cfg.Embedder != nil && p.cfg.EmbedStore != nil && ocrText != "" {
			rag.EmbedAndStorePage(ctx, p.cfg.Embedder, p.cfg.EmbedStore, notePath, i, ocrText, p.cfg.EmbedModel, p.logger)
		}
	}

	// 7. Second OCR pass: red ink to-do extraction.
	p.runTodoPass(ctx, notePath, len(note.Pages))

	return nil
}

func (p *Processor) executePDFJob(ctx context.Context, job *BooxJob) error {
	pdfPath := job.NotePath

	// 1. Compute file hash for dedup.
	f, err := os.Open(pdfPath)
	if err != nil {
		return fmt.Errorf("open pdf: %w", err)
	}
	h := sha256.New()
	io.Copy(h, f)
	f.Close()
	fileHash := hex.EncodeToString(h.Sum(nil))

	// Early exit: skip re-processing if content unchanged since last done job.
	if done, _ := p.store.HasDoneJobWithHash(ctx, pdfPath, fileHash); done {
		p.logger.Info("boox: skipping re-process, hash unchanged", "path", pdfPath)
		return nil
	}

	// 2. Get page count.
	pageCount, err := pdfrender.PageCount(pdfPath)
	if err != nil {
		return fmt.Errorf("pdf page count: %w", err)
	}

	// 3. Extract path metadata. Preserve existing metadata (from importer) if present.
	deviceModel, noteType, folder := p.resolveMetadata(ctx, pdfPath)
	title := strings.TrimSuffix(filepath.Base(pdfPath), filepath.Ext(pdfPath))

	// 4. Use filename (without extension) as noteID for cache path consistency.
	noteID := title

	// 5. Upsert note record.
	if err := p.store.UpsertNote(ctx, pdfPath, noteID, title, deviceModel, noteType, folder, pageCount, fileHash, NoteCreatedAt(title, pdfPath)); err != nil {
		return fmt.Errorf("upsert note: %w", err)
	}

	// 6. Clear old cached renders and indexed content.
	cacheDir := filepath.Join(p.cfg.CachePath, noteID)
	os.RemoveAll(cacheDir)
	os.MkdirAll(cacheDir, 0755)
	if p.cfg.ContentDeleter != nil {
		p.cfg.ContentDeleter.Delete(ctx, pdfPath)
	}

	// 7. Render all pages, OCR, and index.
	// Start at 150 DPI; scale down if image is too large for the vision model.
	// Max ~4 megapixels keeps images within typical VLM context limits.
	const maxPixels = 4_000_000

	for i := 0; i < pageCount; i++ {
		jpegData, err := renderPDFPageScaled(pdfPath, i, 150, maxPixels)
		if err != nil {
			return fmt.Errorf("render pdf page %d: %w", i, err)
		}

		// Cache rendered JPEG.
		cachePath := filepath.Join(cacheDir, fmt.Sprintf("page_%d.jpg", i))
		if err := os.WriteFile(cachePath, jpegData, 0644); err != nil {
			return fmt.Errorf("cache page %d: %w", i, err)
		}

		// OCR if client available.
		var ocrText string
		if p.cfg.OCR != nil {
			prompt := ""
			if p.cfg.OCRPrompt != nil {
				prompt = p.cfg.OCRPrompt()
			}
			text, err := p.cfg.OCR.Recognize(ctx, jpegData, prompt)
			if err != nil {
				return fmt.Errorf("ocr page %d: %w", i, err)
			}
			ocrText = text
		}

		// Index OCR'd text.
		titleText := ""
		if i == 0 {
			titleText = title
		}
		if err := p.cfg.Indexer.IndexPage(ctx, pdfPath, i, "api", ocrText, titleText, ""); err != nil {
			return fmt.Errorf("index page %d: %w", i, err)
		}
		// Embed page text if embedder is available (chunked; long pages → many vectors)
		if p.cfg.Embedder != nil && p.cfg.EmbedStore != nil && ocrText != "" {
			rag.EmbedAndStorePage(ctx, p.cfg.Embedder, p.cfg.EmbedStore, pdfPath, i, ocrText, p.cfg.EmbedModel, p.logger)
		}
	}

	// 8. Second OCR pass: red ink to-do extraction.
	p.runTodoPass(ctx, pdfPath, pageCount)

	return nil
}

// renderPDFPageScaled renders a PDF page, scaling down DPI if the resulting
// image exceeds maxPixels. Tries the requested DPI first, then reduces
// proportionally until the image fits or DPI drops to the minimum.
func renderPDFPageScaled(pdfPath string, pageIndex, startDPI, maxPixels int) ([]byte, error) {
	const minDPI = 50

	dpi := startDPI
	for {
		data, err := pdfrender.RenderPage(pdfPath, pageIndex, dpi)
		if err != nil {
			return nil, err
		}
		img, err := pdfrender.DecodeJPEG(data)
		if err != nil {
			// Can't decode to check size — return as-is.
			return data, nil
		}
		bounds := img.Bounds()
		pixels := bounds.Dx() * bounds.Dy()
		if pixels <= maxPixels || dpi <= minDPI {
			return data, nil
		}
		// Too large — reduce DPI proportionally.
		ratio := float64(maxPixels) / float64(pixels)
		newDPI := int(float64(dpi) * math.Sqrt(ratio))
		if newDPI >= dpi {
			newDPI = dpi - 10 // ensure progress
		}
		if newDPI < minDPI {
			newDPI = minDPI
		}
		dpi = newDPI
	}
}

// resolveMetadata returns device metadata for a file path. If the file already
// has metadata in the database (e.g., from the importer), those values are
// preserved. Otherwise, falls back to ExtractPathMetadata (WebDAV convention).
func (p *Processor) resolveMetadata(ctx context.Context, filePath string) (deviceModel, noteType, folder string) {
	existing, err := p.store.GetNote(ctx, filePath)
	if err == nil && existing != nil && existing.DeviceModel != "" && existing.DeviceModel != ".." {
		return existing.DeviceModel, existing.NoteType, existing.Folder
	}
	// Fallback: derive from WebDAV path convention.
	// Only valid when the file is under the WebDAV notes root.
	if p.notesPath != "" && strings.HasPrefix(filePath, p.notesPath) {
		relPath, _ := filepath.Rel(p.notesPath, filePath)
		pm := ubwebdav.ExtractPathMetadata(relPath)
		return pm.DeviceModel, pm.NoteType, pm.Folder
	}
	// File is not under WebDAV root (e.g., bulk import) — return whatever
	// the importer pre-populated, or empty strings.
	if existing != nil {
		return existing.DeviceModel, existing.NoteType, existing.Folder
	}
	return "", "", ""
}

// runTodoPass performs the second OCR pass for red ink to-do extraction.
func (p *Processor) runTodoPass(ctx context.Context, filePath string, pageCount int) {
	if p.cfg.OCR == nil || p.cfg.TodoEnabled == nil || !p.cfg.TodoEnabled() {
		return
	}

	var allTodos []TodoItem
	todoPrompt := ""
	if p.cfg.TodoPrompt != nil {
		todoPrompt = p.cfg.TodoPrompt()
	}

	// Derive cache dir from the note record.
	note, err := p.store.GetNote(ctx, filePath)
	if err != nil || note == nil {
		p.logger.Warn("todo pass: could not look up note", "path", filePath, "error", err)
		return
	}
	cacheDir := filepath.Join(p.cfg.CachePath, note.NoteID)

	for i := 0; i < pageCount; i++ {
		cachePath := filepath.Join(cacheDir, fmt.Sprintf("page_%d.jpg", i))
		jpegData, err := os.ReadFile(cachePath)
		if err != nil {
			p.logger.Warn("todo pass: read cached page", "page", i, "error", err)
			continue
		}

		resp, err := p.cfg.OCR.Recognize(ctx, jpegData, todoPrompt)
		if err != nil {
			p.logger.Warn("todo pass: OCR failed", "page", i, "error", err)
			continue
		}

		todos := parseTodoResponse(resp)
		if len(todos) > 0 {
			p.logger.Info("todos extracted", "page", i, "count", len(todos), "path", filePath)
			allTodos = append(allTodos, todos...)
		}
	}

	if len(allTodos) > 0 && p.cfg.OnTodosFound != nil {
		p.cfg.OnTodosFound(ctx, filePath, allTodos)
	}
}

// parseTodoResponse extracts TodoItem objects from the OCR response.
// Each line is tried as a JSON object; non-JSON lines are skipped.
func parseTodoResponse(resp string) []TodoItem {
	var todos []TodoItem
	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var item TodoItem
		if err := json.Unmarshal([]byte(line), &item); err == nil && item.Text != "" {
			todos = append(todos, item)
		}
	}
	return todos
}
