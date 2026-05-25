package processor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sysop/ultrabridge/internal/rag"
)

// Indexer is the interface the worker uses to index recognized text.
// Defined here (not in the search package) to avoid a circular import.
type Indexer interface {
	// IndexPage indexes a single page. titleText and keywords are populated for
	// page 0 only (they apply to the whole note); pass empty strings for other pages.
	IndexPage(ctx context.Context, path string, pageIdx int, source, bodyText, titleText, keywords string) error
}

// WorkerConfig holds runtime configuration for the OCR worker.
type WorkerConfig struct {
	OCREnabled    bool
	BackupPath    string
	MaxFileMB     int
	OCRClient     *OCRClient    // nil = OCR disabled
	OCRPrompt     func() string // returns current OCR prompt; nil = use default
	InjectEnabled func() bool   // returns whether JIIX injection is enabled; nil = enabled
	Indexer       Indexer       // nil = indexing disabled
	Embedder      rag.Embedder  // nil = embedding disabled
	EmbedModel    string        // model name for note_embeddings.model column
	EmbedStore    rag.EmbedStore // nil = embedding disabled
}

// EnqueueOption configures optional behavior for Enqueue.
type EnqueueOption func(*enqueueConfig)

type enqueueConfig struct {
	requeueAfter *time.Duration
}

// WithRequeueAfter sets a delay before the re-enqueued job can be claimed.
// claimNext will skip the job until the delay has elapsed.
func WithRequeueAfter(d time.Duration) EnqueueOption {
	return func(c *enqueueConfig) {
		c.requeueAfter = &d
	}
}

// Processor manages the background OCR job queue.
type Processor interface {
	Start(ctx context.Context) error
	Stop() error
	Status() ProcessorStatus
	Enqueue(ctx context.Context, path string, opts ...EnqueueOption) error
	Skip(ctx context.Context, path, reason string) error
	Unskip(ctx context.Context, path string) error
	// GetJob returns the latest job record for a file path, or nil if none exists.
	GetJob(ctx context.Context, path string) (*Job, error)
}

// Store implements Processor backed by SQLite.
type Store struct {
	db     *sql.DB
	cfg    WorkerConfig
	logger *slog.Logger
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// New creates a Processor Store.
func New(db *sql.DB, cfg WorkerConfig) *Store {
	logger := slog.Default()
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(nil, nil))
	}
	return &Store{db: db, cfg: cfg, logger: logger}
}

func (s *Store) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return errors.New("processor already running")
	}
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.done = make(chan struct{})
	go s.run(ctx)
	go s.watchdog(ctx)
	return nil
}

func (s *Store) Stop() error {
	s.mu.Lock()
	cancel := s.cancel
	done := s.done
	s.cancel = nil
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	<-done // wait for run() to exit
	return nil
}

func (s *Store) Status() ProcessorStatus {
	s.mu.Lock()
	running := s.cancel != nil
	s.mu.Unlock()

	var pending, inFlight, done, failed int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM jobs WHERE status=?", StatusPending).Scan(&pending); err != nil {
		s.logger.Error("failed to count pending jobs", "error", err)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM jobs WHERE status=?", StatusInProgress).Scan(&inFlight); err != nil {
		s.logger.Error("failed to count in-flight jobs", "error", err)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM jobs WHERE status=?", StatusDone).Scan(&done); err != nil {
		s.logger.Error("failed to count done jobs", "error", err)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM jobs WHERE status=?", StatusFailed).Scan(&failed); err != nil {
		s.logger.Error("failed to count failed jobs", "error", err)
	}
	return ProcessorStatus{
		Running:  running,
		Pending:  pending,
		InFlight: inFlight,
		Done:     done,
		Failed:   failed,
	}
}

func (s *Store) Enqueue(ctx context.Context, path string, opts ...EnqueueOption) error {
	var cfg enqueueConfig
	for _, o := range opts {
		o(&cfg)
	}

	now := time.Now()
	var requeueAfter any
	if cfg.requeueAfter != nil {
		requeueAfter = now.Add(*cfg.requeueAfter).Unix()
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO jobs (note_path, status, queued_at, requeue_after)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(note_path) DO UPDATE SET status=excluded.status, queued_at=excluded.queued_at, requeue_after=excluded.requeue_after
		WHERE status IN (?, ?, ?)`,
		path, StatusPending, now.Unix(), requeueAfter,
		StatusDone, StatusFailed, StatusSkipped,
	)
	if err != nil {
		return fmt.Errorf("enqueue %s: %w", path, err)
	}
	return nil
}

func (s *Store) Skip(ctx context.Context, path, reason string) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO jobs (note_path, status, skip_reason, queued_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(note_path) DO UPDATE SET status=excluded.status, skip_reason=excluded.skip_reason`,
		path, StatusSkipped, reason, now,
	)
	return err
}

func (s *Store) Unskip(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status=?, skip_reason=NULL WHERE note_path=? AND status=?`,
		StatusPending, path, StatusSkipped,
	)
	return err
}

func (s *Store) GetJob(ctx context.Context, path string) (*Job, error) {
	var j Job
	var startedAt, finishedAt, queuedAt, requeueAfter sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, note_path, status, COALESCE(skip_reason,''), attempts, COALESCE(last_error,''),
		       queued_at, started_at, finished_at, requeue_after
		FROM jobs WHERE note_path=? LIMIT 1`, path).
		Scan(&j.ID, &j.NotePath, &j.Status, &j.SkipReason, &j.Attempts, &j.LastError,
			&queuedAt, &startedAt, &finishedAt, &requeueAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetJob: %w", err)
	}
	if queuedAt.Valid {
		j.QueuedAt = time.Unix(queuedAt.Int64, 0)
	}
	if startedAt.Valid {
		j.StartedAt = time.Unix(startedAt.Int64, 0)
	}
	if finishedAt.Valid {
		j.FinishedAt = time.Unix(finishedAt.Int64, 0)
	}
	if requeueAfter.Valid {
		t := time.Unix(requeueAfter.Int64, 0)
		j.RequeueAfter = &t
	}
	return &j, nil
}

// claimNext atomically claims the oldest pending job.
// Returns nil, nil if no jobs are pending.
// Skips jobs whose requeue_after is in the future.
func (s *Store) claimNext(ctx context.Context) (*Job, error) {
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status=?, started_at=?
		WHERE id = (SELECT id FROM jobs WHERE status=?
		    AND (requeue_after IS NULL OR requeue_after <= ?)
		    ORDER BY queued_at ASC LIMIT 1)`,
		StatusInProgress, now, StatusPending, now,
	)
	if err != nil {
		return nil, fmt.Errorf("claimNext: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return nil, nil
	}

	var j Job
	err = s.db.QueryRowContext(ctx, `
		SELECT id, note_path, status, attempts FROM jobs
		WHERE status=? ORDER BY started_at DESC LIMIT 1`,
		StatusInProgress,
	).Scan(&j.ID, &j.NotePath, &j.Status, &j.Attempts)
	if err != nil {
		return nil, fmt.Errorf("claimNext lookup: %w", err)
	}
	return &j, nil
}

func (s *Store) markDone(ctx context.Context, jobID int64, errMsg string) {
	status := StatusDone
	if errMsg != "" {
		status = StatusFailed
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status=?, last_error=?, finished_at=?, attempts=attempts+1
		WHERE id=?`,
		status, errMsg, time.Now().Unix(), jobID,
	)
	if err != nil {
		s.logger.Error("failed to mark job done", "job_id", jobID, "error", err)
	}
}

// Requeue sets the job back to pending with a delay before it can be reclaimed.
// Increments attempts counter. Only operates on in_progress jobs to prevent
// accidental status regression if the job was already marked done/failed.
func (s *Store) Requeue(ctx context.Context, jobID int64, delay time.Duration) error {
	requeueAt := time.Now().Add(delay).Unix()
	result, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status = ?, requeue_after = ?, attempts = attempts + 1
		WHERE id = ? AND status = ?`, StatusPending, requeueAt, jobID, StatusInProgress)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("requeue job %d: job not in in_progress status", jobID)
	}
	return nil
}

func (s *Store) run(ctx context.Context) {
	defer close(s.done)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		job, err := s.claimNext(ctx)
		if err != nil || job == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		s.processJob(ctx, job)
	}
}
