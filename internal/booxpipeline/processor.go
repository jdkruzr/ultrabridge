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

const (
	// maxJobAttempts caps retries of a transient failure. attempts counts
	// claims (ClaimNextJob bumps it), so this is "give up after the Nth try".
	maxJobAttempts = 5
	// retryBackoffBase doubles per attempt — 1m, 2m, 4m, 8m — up to the cap.
	// Long enough that a restarting OCR backend gets a chance to come up,
	// short enough that a blip doesn't visibly stall the queue.
	retryBackoffBase = time.Minute
	retryBackoffMax  = 30 * time.Minute
)

// retryDelay returns the backoff before the next claim of a job that has been
// claimed `attempts` times. Saturates at retryBackoffMax rather than shifting
// into overflow on a pathological attempt count.
func retryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 16 {
		return retryBackoffMax
	}
	d := retryBackoffBase << (attempts - 1)
	if d > retryBackoffMax {
		return retryBackoffMax
	}
	return d
}

// handleJobError decides what a failed job's terminal state should be:
// leave it alone (shutdown), requeue it with backoff (transient), or fail it
// (permanent, or out of attempts).
func (p *Processor) handleJobError(ctx context.Context, job *BooxJob, err error) {
	// Shutdown mid-job is not a defect in the note. Leave the row in_progress
	// and let the startup reclaim requeue it; failing it here would mark a
	// perfectly good note failed just because the operator stopped the worker.
	if ctx.Err() != nil {
		p.logger.Info("boox job interrupted by shutdown, left for startup reclaim",
			"job_id", job.ID, "path", job.NotePath)
		return
	}

	// A transient failure means the backend never rendered a verdict on this
	// note, so failing it terminally would strand it until a human pressed
	// Retry Failed. Requeue with backoff instead, up to the attempt cap.
	if processor.IsTransient(err) && job.Attempts < maxJobAttempts {
		delay := retryDelay(job.Attempts)
		p.logger.Warn("boox job transient failure, requeueing",
			"job_id", job.ID, "path", job.NotePath, "attempt", job.Attempts,
			"retry_in", delay, "error", err)
		if rerr := p.store.RequeueJob(ctx, job.ID, err.Error(), time.Now().Add(delay)); rerr != nil {
			p.logger.Error("requeue boox job", "job_id", job.ID, "error", rerr)
		}
		return
	}

	p.logger.Error("boox job failed", "job_id", job.ID, "attempts", job.Attempts, "error", err)
	if ferr := p.store.FailJob(ctx, job.ID, err.Error()); ferr != nil {
		p.logger.Error("fail boox job", "job_id", job.ID, "error", ferr)
	}
}

func (p *Processor) processJob(ctx context.Context, job *BooxJob) {
	p.logger.Info("processing boox note", "path", job.NotePath, "job_id", job.ID)

	if err := p.executeJob(ctx, job); err != nil {
		p.handleJobError(ctx, job, err)
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
