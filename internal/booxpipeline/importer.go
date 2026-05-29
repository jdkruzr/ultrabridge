package booxpipeline

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// ImportConfig holds settings for a bulk import scan.
type ImportConfig struct {
	ImportPath  string // filesystem root to scan
	ImportNotes bool   // include .note files
	ImportPDFs  bool   // include .pdf files
	OnyxPaths   bool   // use {model}/{type}/{folder}/{file} path structure
}

// ImportResult summarizes a completed import scan.
type ImportResult struct {
	Scanned  int // total matching files found
	Enqueued int // files actually enqueued (skipping already-done)
	Skipped  int // files skipped (already processed)
	Errors   int // files that failed to enqueue
}

// ExtractImportMetadata derives device metadata from a file's relative path
// within the import directory.
//
// Onyx mode: expects {model}/{type}/{folder}/{file}
//
//	e.g., "Palma2_Pro_C/Notebooks/Moffitt/notes.pdf"
//	→ model="Palma2_Pro_C", noteType="Notebooks", folder="Moffitt"
//
// Generic mode: uses parent directory as folder, no device model.
//
//	e.g., "some/path/Work/notes.pdf"
//	→ model="", noteType="", folder="Work"
func ExtractImportMetadata(relPath string, onyxPaths bool) (deviceModel, noteType, folder string) {
	parts := strings.Split(filepath.ToSlash(relPath), "/")

	if onyxPaths && len(parts) >= 4 {
		// {model}/{type}/{folder}/{file} — possibly deeper nesting
		deviceModel = parts[0]
		noteType = parts[1]
		folder = parts[2]
		return
	}

	// Generic: folder = immediate parent directory.
	if len(parts) >= 2 {
		folder = parts[len(parts)-2]
	}
	return
}

// ScanAndEnqueue walks the import path, finds matching files, and enqueues
// them into the Boox pipeline. Files with an existing "done" job are skipped.
func (p *Processor) ScanAndEnqueue(ctx context.Context, cfg ImportConfig, logger *slog.Logger) ImportResult {
	var result ImportResult

	if cfg.ImportPath == "" {
		logger.Warn("import: no import path configured")
		return result
	}
	if !cfg.ImportNotes && !cfg.ImportPDFs {
		logger.Warn("import: no file types selected")
		return result
	}

	// Collect matching files.
	var files []string
	err := filepath.WalkDir(cfg.ImportPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable dirs
		}
		if d.IsDir() {
			// Skip hidden directories (like .cache, .versions).
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(d.Name()))
		if (cfg.ImportNotes && ext == ".note") || (cfg.ImportPDFs && ext == ".pdf") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		logger.Error("import: walk failed", "path", cfg.ImportPath, "error", err)
		return result
	}

	result.Scanned = len(files)
	logger.Info("import: scan complete", "path", cfg.ImportPath, "found", len(files))

	// Enqueue each file, skipping already-processed ones.
	for _, path := range files {
		select {
		case <-ctx.Done():
			logger.Warn("import: cancelled", "enqueued", result.Enqueued)
			return result
		default:
		}

		// Extract metadata and pre-populate the note record.
		// Done before the skip check so metadata is always refreshed.
		relPath, _ := filepath.Rel(cfg.ImportPath, path)
		model, nType, fldr := ExtractImportMetadata(relPath, cfg.OnyxPaths)
		title := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))

		noteID := ""
		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".pdf" {
			noteID = title
		}
		if err := p.store.UpsertNote(ctx, path, noteID, title, model, nType, fldr, 0, "", NoteCreatedAt(title, path)); err != nil {
			logger.Error("import: upsert note", "path", path, "error", err)
			result.Errors++
			continue
		}

		// Check if already processed.
		job, err := p.store.GetLatestJob(ctx, path)
		if err != nil {
			logger.Error("import: check job", "path", path, "error", err)
			result.Errors++
			continue
		}
		if job != nil && job.Status == "done" {
			result.Skipped++
			continue
		}

		if err := p.store.EnqueueJob(ctx, path); err != nil {
			logger.Error("import: enqueue", "path", path, "error", err)
			result.Errors++
			continue
		}
		result.Enqueued++
	}

	logger.Info("import: complete",
		"scanned", result.Scanned,
		"enqueued", result.Enqueued,
		"skipped", result.Skipped,
		"errors", result.Errors,
	)
	return result
}
