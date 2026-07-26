package booxpipeline

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/sysop/ultrabridge/internal/processor"
)

// Processor manages the Boox notes processing pipeline.
//
// running/cancel/done are guarded by mu as a set: the worker's shutdown
// channel is minted per Start (not once at construction) so a stop -> start
// -> stop cycle from the UI doesn't wait on an already-closed channel.
type Processor struct {
	store     *Store
	cfg       WorkerConfig
	notesPath string
	logger    *slog.Logger

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// New creates a new Boox processor.
func New(db *sql.DB, notesPath string, cfg WorkerConfig, logger *slog.Logger) *Processor {
	return &Processor{
		store:     NewStoreWithRoot(db, notesPath),
		cfg:       cfg,
		notesPath: notesPath,
		logger:    logger,
	}
}

// Running reports whether the worker loop is up. Backs the ▶/⏹ glyph on the
// global pipeline status bar, symmetric with the Supernote processor's
// Status().Running.
func (p *Processor) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// Store returns the underlying Boox store for web access.
func (p *Processor) Store() *Store {
	return p.store
}

// Enqueue adds a .note file to the processing queue.
func (p *Processor) Enqueue(ctx context.Context, absPath string) error {
	return p.store.EnqueueJob(ctx, absPath)
}

// Start begins the worker loop and watchdog.
// Reclaims any orphaned in_progress jobs from a previous crash/restart.
// Idempotent: starting an already-running processor is a no-op.
func (p *Processor) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return nil
	}
	if err := p.store.ReclaimAllInProgress(ctx); err != nil {
		p.logger.Warn("reclaim orphaned jobs on startup", "error", err)
	}
	ctx, p.cancel = context.WithCancel(ctx)
	done := make(chan struct{})
	p.done, p.running = done, true
	p.mu.Unlock()

	go p.run(ctx, done)
	go p.watchdog(ctx)
	return nil
}

// Stop signals shutdown and waits for the worker to finish. Idempotent:
// stopping an already-stopped processor is a no-op.
func (p *Processor) Stop() {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return
	}
	cancel, done := p.cancel, p.done
	p.running = false
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	<-done
}

func (p *Processor) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		job, err := p.store.ClaimNextJob(ctx)
		if err != nil {
			p.logger.Error("claim boox job", "error", err)
		}
		if job == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		p.processJob(ctx, job)
	}
}

func (p *Processor) processJob(ctx context.Context, job *BooxJob) {
	p.logger.Info("processing boox note", "path", job.NotePath, "job_id", job.ID)

	if err := p.executeJob(ctx, job); err != nil {
		p.logger.Error("boox job failed", "job_id", job.ID, "error", err)
		if err := p.store.FailJob(ctx, job.ID, err.Error()); err != nil {
			p.logger.Error("fail boox job", "job_id", job.ID, "error", err)
		}
		return
	}

	ocrSource := "api"
	apiModel := ""
	if p.cfg.OCR == nil {
		ocrSource = ""
	} else if client, ok := p.cfg.OCR.(*processor.OCRClient); ok {
		apiModel = client.Model()
	}
	if err := p.store.CompleteJob(ctx, job.ID, ocrSource, apiModel); err != nil {
		p.logger.Error("complete boox job", "job_id", job.ID, "error", err)
	}
	p.logger.Info("boox note processed", "path", job.NotePath, "job_id", job.ID)
}

// watchdog reclaims stuck jobs (in_progress for >10 minutes).
func (p *Processor) watchdog(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.store.ReclaimStuckJobs(ctx, 10*time.Minute)
		}
	}
}
