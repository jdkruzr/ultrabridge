package pipeline

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/sysop/ultrabridge/internal/notestore"
	"github.com/sysop/ultrabridge/internal/processor"
)

// Pipeline owns the file detection goroutines and wires NoteStore to Processor.
type Pipeline struct {
	notesPath string
	store     notestore.NoteStore
	proc      processor.Processor
	events    <-chan []byte // inbound Engine.IO messages; nil = disabled
	logger    *slog.Logger
	cancel    context.CancelFunc
	done      chan struct{}
}

// Config holds Pipeline configuration.
type Config struct {
	NotesPath string
	Store     notestore.NoteStore
	Proc      processor.Processor
	Events    <-chan []byte // from notifier.Events(); nil = no Engine.IO listener
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
		events:    cfg.Events,
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
	if p.events != nil {
		go p.runEngineIOListener(ctx)
	}

	<-ctx.Done()
}

// enqueue adds a path to the processor queue if it is a .note file.
// It first ensures the file exists in the notes table (FK constraint on jobs.note_path)
// by running a targeted scan, then enqueues the job.
//
// Files whose last job is already "done" are intentionally skipped: the change
// was most likely caused by the worker writing RECOGNTEXT back into the file.
// Re-queuing those would create an infinite inject→detect→re-queue loop.
// A user who wants to re-process a completed file should use the UI "Queue" button.
func (p *Pipeline) enqueue(ctx context.Context, path string) {
	if notestore.ClassifyFileType(filepath.Ext(path)) != notestore.FileTypeNote {
		return
	}
	// Ensure the notes row exists before inserting the job (FK constraint on jobs.note_path).
	// UpsertFile is defined in Phase 2 NoteStore for exactly this purpose.
	if err := p.store.UpsertFile(ctx, path); err != nil {
		p.logger.Warn("pipeline upsert before enqueue failed", "path", path, "err", err)
		return
	}
	// Skip files already successfully processed. Automatic detection should not
	// re-queue completed files — the mtime change was caused by our own write.
	job, err := p.proc.GetJob(ctx, path)
	if err == nil && job != nil && job.Status == processor.StatusDone {
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

