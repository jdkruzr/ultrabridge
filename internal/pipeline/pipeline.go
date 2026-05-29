package pipeline

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/sysop/ultrabridge/internal/notestore"
	"github.com/sysop/ultrabridge/internal/processor"
)

// Pipeline owns the file detection goroutines and wires NoteStore to Processor.
type Pipeline struct {
	notesPath string
	store     notestore.NoteStore
	proc      processor.Processor
	logger    *slog.Logger
	cancel    context.CancelFunc
	done      chan struct{}
}

// Config holds Pipeline configuration.
type Config struct {
	NotesPath string
	Store     notestore.NoteStore
	Proc      processor.Processor
	Logger    *slog.Logger
}

// New creates a Pipeline. Call Start to begin watching.
func New(cfg Config) *Pipeline {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Pipeline{
		notesPath: cfg.NotesPath,
		store:     cfg.Store,
		proc:      cfg.Proc,
		logger:    logger,
	}
}

// Start launches file detection goroutines. Non-blocking. Call Close to stop.
func (p *Pipeline) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.done = make(chan struct{})
	go p.runAll(ctx)
}

// ScanNow triggers an immediate reconciliation scan (filesystem walk + orphan pruning).
func (p *Pipeline) ScanNow(ctx context.Context) {
	p.reconcile(ctx)
}

// Close stops all pipeline goroutines and waits for clean exit.
func (p *Pipeline) Close() {
	if p.cancel != nil {
		p.cancel()
		<-p.done
	}
}

func (p *Pipeline) runAll(ctx context.Context) {
	defer close(p.done)

	// Initial reconciliation scan on startup
	p.reconcile(ctx)

	go p.runWatcher(ctx)
	go p.runReconciler(ctx)

	<-ctx.Done()
}

// enqueue adds a path to the processor queue if it is a .note file.
// It first ensures the file exists in the notes table (FK constraint on jobs.note_path)
// by running a targeted upsert, then enqueues the job.
//
// Files whose last job is "done" are checked for content changes: the stored
// SHA-256 from job completion is compared against the current file. If they match
// (UB's own RECOGNTEXT injection), the file is skipped. If they differ (user edit
// on the device), the file is re-queued with a 30-second delay to debounce rapid syncs.
func (p *Pipeline) enqueue(ctx context.Context, path string) {
	if notestore.ClassifyFileType(filepath.Ext(path)) != notestore.FileTypeNote {
		return
	}
	// Skip _CONFLICT_ files created by the device when both local and cloud
	// versions changed since last sync. Processing these causes feedback loops.
	if strings.Contains(filepath.Base(path), "_CONFLICT_") {
		return
	}
	// Ensure the notes row exists before inserting the job (FK constraint on jobs.note_path).
	// UpsertFile is defined in Phase 2 NoteStore for exactly this purpose.
	if err := p.store.UpsertFile(ctx, path); err != nil {
		p.logger.Warn("pipeline upsert before enqueue failed", "path", path, "err", err)
		return
	}
	// Skip files already successfully processed unless content changed.
	// Compare stored hash with current file to distinguish UB's own write from a user edit.
	job, err := p.proc.GetJob(ctx, path)
	if err == nil && job != nil && job.Status == processor.StatusDone {
		storedHash, hashErr := p.store.GetHash(ctx, path)
		if hashErr != nil {
			p.logger.Warn("pipeline: failed to get stored hash, skipping re-enqueue", "path", path, "err", hashErr)
			return
		}
		currentHash, hashErr := notestore.ComputeSHA256(path)
		if hashErr != nil {
			p.logger.Warn("pipeline: failed to compute file hash, skipping re-enqueue", "path", path, "err", hashErr)
			return
		}
		if storedHash != "" && storedHash == currentHash {
			return // hashes match — UB wrote this file, no re-processing needed
		}
		// Hash differs or no stored hash — user edited the file, re-queue with delay.
		if enqErr := p.proc.Enqueue(ctx, path, processor.WithRequeueAfter(30*time.Second)); enqErr != nil {
			p.logger.Warn("pipeline: re-enqueue after hash change failed", "path", path, "err", enqErr)
		} else {
			p.logger.Info("pipeline: re-queued changed file", "path", path, "storedHash", storedHash, "currentHash", currentHash)
		}
		return
	}

	// Hash-based move/rename detection (best-effort: any failure falls through to normal enqueue).
	// Compute SHA-256 of the file and check if another path was already processed with
	// identical content. If so, transfer the job record rather than re-processing.
	if hash, hashErr := notestore.ComputeSHA256(path); hashErr == nil {
		if oldPath, found, _ := p.store.LookupByHash(ctx, hash); found && oldPath != path {
			if transferErr := p.store.TransferJob(ctx, oldPath, path); transferErr == nil {
				if setErr := p.store.SetHash(ctx, path, hash); setErr != nil {
					p.logger.Warn("failed to set hash after job transfer", "path", path, "err", setErr)
				}
				p.logger.Info("detected moved file, transferred job", "old", oldPath, "new", path)
				return
			}
			p.logger.Warn("job transfer failed, will re-process", "old", oldPath, "new", path)
		}
	}

	if err := p.proc.Enqueue(ctx, path); err != nil {
		p.logger.Warn("pipeline enqueue failed", "path", path, "err", err)
	}
}

// Enqueue runs the full file-detection enqueue (UpsertFile → dedup → processor
// enqueue) for a single path — the same logic the watcher and reconciler use.
// Exposed for the SPC upload write-path (Phase 4d) so a freshly-promoted upload
// is kicked through the FK-safe path; calling processor.Enqueue directly would
// violate the jobs.note_path → notes(path) foreign key (the notes row must exist
// first). Safe to call even though the watcher also covers NotesPath — the
// processor's enqueue is idempotent (ON CONFLICT) and the hash dedup skips
// re-processing UB's own writes.
func (p *Pipeline) Enqueue(ctx context.Context, path string) error {
	p.enqueue(ctx, path)
	return nil
}
