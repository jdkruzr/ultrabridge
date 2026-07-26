package remarkable

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sysop/ultrabridge/internal/processor"
	"github.com/sysop/ultrabridge/internal/rag"
)

const (
	ocrStatusPending    = "pending"
	ocrStatusInProgress = "in_progress"
	ocrStatusDone       = "done"
	ocrStatusFailed     = "failed"
)

const (
	// ocrStuckAfter is how long a claimed job may sit in_progress before the
	// watchdog assumes the worker that claimed it is never coming back.
	// Deliberately generous against a single page's vision-API round trip: a
	// premature reclaim only costs duplicate work (indexing is an upsert),
	// but too tight a bound would churn against a slow OCR backend.
	ocrStuckAfter = 15 * time.Minute
	// ocrWatchdogInterval is how often the sweep for stuck jobs runs.
	ocrWatchdogInterval = time.Minute
	// ocrMaxAttempts caps reclaim retries. Without it, a page that reliably
	// wedges the worker would cycle pending -> in_progress -> reclaimed
	// forever; past this many claims the job lands in `failed` where it is
	// visible instead of churning.
	ocrMaxAttempts = 5
)

// OCRQueueStatus is the reMarkable render-to-fulltext queue snapshot surfaced
// through /files/status.
type OCRQueueStatus struct {
	Pending    int `json:"pending"`
	InProgress int `json:"in_progress"`
	Done       int `json:"done"`
	Failed     int `json:"failed"`
}

type ocrJob struct {
	ID         int64
	DocumentID string
	Page       int
	Revision   string
	Manual     bool
}

type ocrProcessor struct {
	store      *store
	indexer    pageIndexer
	ocrClient  *processor.OCRClient
	embedder   rag.Embedder
	embedStore rag.EmbedStore
	embedModel string
	logger     *slog.Logger

	cancel context.CancelFunc
	wake   chan struct{}
	done   chan struct{}
	wdDone chan struct{}
}

func newOCRProcessor(st *store, deps ocrDeps, logger *slog.Logger) *ocrProcessor {
	if st == nil || deps.indexer == nil || deps.ocrClient == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ocrProcessor{
		store:      st,
		indexer:    deps.indexer,
		ocrClient:  deps.ocrClient,
		embedder:   deps.embedder,
		embedStore: deps.embedStore,
		embedModel: deps.embedModel,
		logger:     logger,
		wake:       make(chan struct{}, 1),
	}
}

type ocrDeps struct {
	indexer    pageIndexer
	ocrClient  *processor.OCRClient
	embedder   rag.Embedder
	embedStore rag.EmbedStore
	embedModel string
}

func (p *ocrProcessor) Start(ctx context.Context) {
	if p == nil || p.cancel != nil {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	// Minted per Start, not once at construction, so a Stop/Start cycle
	// doesn't wait on an already-closed channel.
	p.done, p.wdDone = make(chan struct{}), make(chan struct{})

	// No job can legitimately be in flight before the worker exists, so every
	// in_progress row here is an orphan from a process that died mid-job.
	// Reclaim before the loop starts so it can pick the requeued work up on
	// its first pass.
	if requeued, failed, err := p.store.reclaimStuckOCRJobs(ctx, 0); err != nil {
		p.logger.Warn("remarkable OCR startup reclaim failed", "error", err)
	} else if requeued > 0 || failed > 0 {
		p.logger.Info("reclaimed orphaned remarkable OCR jobs",
			"requeued", requeued, "failed", failed)
	}

	go p.loop(runCtx)
	go p.watchdog(runCtx)
	go func() {
		if err := p.EnqueueMissingStale(context.Background()); err != nil {
			p.logger.Warn("remarkable OCR initial enqueue failed", "error", err)
		}
	}()
}

func (p *ocrProcessor) Stop() {
	if p == nil || p.cancel == nil {
		return
	}
	cancel, done, wdDone := p.cancel, p.done, p.wdDone
	cancel()
	<-done
	<-wdDone
	// Clearing cancel is what lets a later Start run at all — and with it the
	// startup reclaim that cleans up whatever this Stop interrupted.
	p.cancel = nil
}

// watchdog requeues jobs whose claiming worker never finished. Without it a
// crash mid-job orphans the row permanently: the ordinary re-enqueue path is
// revision-gated (see enqueueOCRPage), so it will never rescue one, and the
// count sits in the pipeline status bar forever.
func (p *ocrProcessor) watchdog(ctx context.Context) {
	defer close(p.wdDone)
	ticker := time.NewTicker(ocrWatchdogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			requeued, failed, err := p.store.reclaimStuckOCRJobs(ctx, ocrStuckAfter)
			if err != nil {
				p.logger.Warn("remarkable OCR watchdog reclaim failed", "error", err)
				continue
			}
			if requeued > 0 || failed > 0 {
				p.logger.Warn("reclaimed stuck remarkable OCR jobs",
					"requeued", requeued, "failed", failed, "stuck_after", ocrStuckAfter)
				p.notify()
			}
		}
	}
}

func (p *ocrProcessor) Status(ctx context.Context) (OCRQueueStatus, error) {
	if p == nil {
		return OCRQueueStatus{}, nil
	}
	return p.store.ocrQueueStatus(ctx)
}

func (p *ocrProcessor) ReprocessDocument(ctx context.Context, documentID string) error {
	if p == nil {
		return fmt.Errorf("remarkable OCR is not configured")
	}
	if strings.TrimSpace(documentID) == "" {
		return fmt.Errorf("document id is required")
	}
	doc, err := p.store.renderDocument(ctx, documentID)
	if err != nil {
		return err
	}
	if !doc.Renderable {
		return fmt.Errorf("remarkable document is not renderable: %s", doc.RenderableWhy)
	}
	if err := p.enqueueDocument(ctx, doc, true, true); err != nil {
		return err
	}
	p.notify()
	return nil
}

func (p *ocrProcessor) EnqueueMissingStale(ctx context.Context) error {
	if p == nil {
		return nil
	}
	docs, err := p.store.listDocumentTree(ctx)
	if err != nil {
		return err
	}
	for _, row := range docs {
		if row.Type == "folder" {
			continue
		}
		doc, err := p.store.renderDocument(ctx, row.ID)
		if err != nil {
			if errors.Is(err, errDocumentNotFound) {
				continue
			}
			p.logger.Warn("remarkable OCR render bundle unavailable", "document_id", row.ID, "error", err)
			continue
		}
		if !doc.Renderable {
			continue
		}
		if !shouldAutoOCRDocument(doc) {
			if err := p.store.deleteAutomaticOCRJobs(ctx, doc.ID); err != nil {
				return err
			}
			continue
		}
		if err := p.enqueueDocument(ctx, doc, false, false); err != nil {
			return err
		}
	}
	p.notify()
	return nil
}

func (p *ocrProcessor) DeleteDocument(ctx context.Context, documentID string) error {
	if p == nil {
		return nil
	}
	return p.store.deleteOCRJobs(ctx, documentID)
}

func (p *ocrProcessor) enqueueDocument(ctx context.Context, doc RenderDocument, force, manual bool) error {
	pages := doc.PageCount
	if pages == 0 {
		pages = len(doc.PageOrder)
	}
	if pages == 0 && doc.PDFPath != "" {
		pages = 1
	}
	for i := 0; i < pages; i++ {
		if err := p.store.enqueueOCRPage(ctx, doc.ID, i, doc.Revision, force, manual); err != nil {
			return fmt.Errorf("enqueue %s page %d: %w", doc.ID, i, err)
		}
	}
	return nil
}

func shouldAutoOCRDocument(doc RenderDocument) bool {
	fileType := strings.ToLower(strings.TrimSpace(doc.FileType))
	if fileType == "pdf" || fileType == "epub" || doc.PDFPath != "" {
		return false
	}
	return len(doc.PageRM) > 0
}

func (p *ocrProcessor) loop(ctx context.Context) {
	defer close(p.done)
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		job, err := p.store.claimNextOCRJob(ctx)
		if err != nil {
			p.logger.Warn("remarkable OCR claim failed", "error", err)
			p.sleep(ctx, 10*time.Second)
			continue
		}
		if job == nil {
			p.sleep(ctx, 30*time.Second)
			continue
		}
		p.processJob(ctx, *job)
	}
}

func (p *ocrProcessor) sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-p.wake:
	case <-timer.C:
	}
}

func (p *ocrProcessor) notify() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *ocrProcessor) processJob(ctx context.Context, job ocrJob) {
	doc, err := p.store.renderDocument(ctx, job.DocumentID)
	if err != nil {
		p.fail(ctx, job, fmt.Errorf("resolve document: %w", err))
		return
	}
	if doc.Revision != "" && job.Revision != "" && doc.Revision != job.Revision {
		_ = p.store.enqueueOCRPage(ctx, doc.ID, job.Page, doc.Revision, true, job.Manual)
		_ = p.store.completeOCRJob(ctx, job.ID)
		p.notify()
		return
	}
	jpegData, err := RenderPageJPEG(ctx, doc, job.Page)
	if err != nil {
		p.fail(ctx, job, fmt.Errorf("render page: %w", err))
		return
	}
	text, err := p.ocrClient.Recognize(ctx, jpegData, "")
	if err != nil {
		p.fail(ctx, job, fmt.Errorf("recognize page: %w", err))
		return
	}
	title := ""
	if job.Page == 0 {
		title = doc.Name
	}
	path := remarkablePath(doc.ID)
	if err := p.indexer.IndexPage(ctx, path, job.Page, "api", text, title, ""); err != nil {
		p.fail(ctx, job, fmt.Errorf("index page: %w", err))
		return
	}
	if p.embedder != nil && p.embedStore != nil {
		rag.EmbedAndStorePage(ctx, p.embedder, p.embedStore, path, job.Page, text, p.embedModel, p.logger)
	}
	if err := p.store.completeOCRJob(ctx, job.ID); err != nil {
		p.logger.Warn("remarkable OCR complete failed", "job_id", job.ID, "error", err)
	}
}

func (p *ocrProcessor) fail(ctx context.Context, job ocrJob, err error) {
	if markErr := p.store.failOCRJob(ctx, job.ID, err.Error()); markErr != nil {
		p.logger.Warn("remarkable OCR failure mark failed", "job_id", job.ID, "error", markErr)
	}
	p.logger.Warn("remarkable OCR job failed", "document_id", job.DocumentID, "page", job.Page, "error", err)
}

func (s *store) enqueueOCRPage(ctx context.Context, documentID string, page int, revision string, force, manual bool) error {
	now := time.Now().UnixMilli()
	manualInt := 0
	if manual {
		manualInt = 1
	}
	if force {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO remarkable_ocr_jobs(document_id, page, revision, manual, status, attempts, last_error, queued_at, started_at, finished_at)
			VALUES(?, ?, ?, ?, ?, 0, '', ?, 0, 0)
			ON CONFLICT(document_id, page) DO UPDATE SET
				revision=excluded.revision,
				manual=excluded.manual,
				status=excluded.status,
				attempts=0,
				last_error='',
				queued_at=excluded.queued_at,
				started_at=0,
				finished_at=0`,
			documentID, page, revision, manualInt, ocrStatusPending, now)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO remarkable_ocr_jobs(document_id, page, revision, manual, status, attempts, last_error, queued_at, started_at, finished_at)
		VALUES(?, ?, ?, ?, ?, 0, '', ?, 0, 0)
		ON CONFLICT(document_id, page) DO UPDATE SET
			revision=excluded.revision,
			manual=excluded.manual,
			status=excluded.status,
			attempts=0,
			last_error='',
			queued_at=excluded.queued_at,
			started_at=0,
			finished_at=0
		WHERE remarkable_ocr_jobs.revision <> excluded.revision`,
		documentID, page, revision, manualInt, ocrStatusPending, now)
	return err
}

func (s *store) claimNextOCRJob(ctx context.Context) (*ocrJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var job ocrJob
	var manual int
	err = tx.QueryRowContext(ctx, `
		SELECT id, document_id, page, revision, manual
		FROM remarkable_ocr_jobs
		WHERE status = ?
		ORDER BY queued_at ASC, id ASC
		LIMIT 1`, ocrStatusPending).
		Scan(&job.ID, &job.DocumentID, &job.Page, &job.Revision, &manual)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	job.Manual = manual != 0
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `
		UPDATE remarkable_ocr_jobs
		SET status = ?, attempts = attempts + 1, started_at = ?, last_error = ''
		WHERE id = ?`, ocrStatusInProgress, now, job.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &job, nil
}

func (s *store) completeOCRJob(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE remarkable_ocr_jobs
		SET status = ?, finished_at = ?, last_error = ''
		WHERE id = ?`, ocrStatusDone, time.Now().UnixMilli(), id)
	return err
}

func (s *store) failOCRJob(ctx context.Context, id int64, msg string) error {
	if len(msg) > 1000 {
		msg = msg[:1000]
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE remarkable_ocr_jobs
		SET status = ?, finished_at = ?, last_error = ?
		WHERE id = ?`, ocrStatusFailed, time.Now().UnixMilli(), msg, id)
	return err
}

func (s *store) deleteOCRJobs(ctx context.Context, documentID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM remarkable_ocr_jobs WHERE document_id = ?`, documentID)
	return err
}

func (s *store) deleteAutomaticOCRJobs(ctx context.Context, documentID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM remarkable_ocr_jobs WHERE document_id = ? AND manual = 0`, documentID)
	return err
}

// reclaimStuckOCRJobs returns abandoned in_progress jobs to the pending queue.
//
// This is the ONLY cleanup path for in_progress rows. completeOCRJob and
// failOCRJob are reachable only from the goroutine that claimed the job, so a
// process that dies mid-job orphans the row, and enqueueOCRPage's ordinary
// (non-forced) upsert is gated on `revision <> excluded.revision` — an
// unchanged page will never be re-enqueued over the orphan. Left alone, such
// a row is in_progress forever.
//
// olderThan <= 0 reclaims every in_progress row; that's what start-up wants,
// since nothing can be legitimately in flight before the worker exists. A
// positive value bounds the sweep to jobs the watchdog considers stuck.
//
// Jobs that have burned through ocrMaxAttempts are failed rather than
// requeued. attempts is NOT incremented here — claimNextOCRJob already bumps
// it on every claim, so the counter stays a true claim count.
func (s *store) reclaimStuckOCRJobs(ctx context.Context, olderThan time.Duration) (requeued, failed int64, err error) {
	now := time.Now().UnixMilli()

	failSQL := `
		UPDATE remarkable_ocr_jobs
		SET status = ?, finished_at = ?, last_error = ?
		WHERE status = ? AND attempts >= ?`
	failArgs := []any{
		ocrStatusFailed, now,
		fmt.Sprintf("abandoned in_progress after %d attempts", ocrMaxAttempts),
		ocrStatusInProgress, ocrMaxAttempts,
	}

	requeueSQL := `
		UPDATE remarkable_ocr_jobs
		SET status = ?, started_at = 0
		WHERE status = ?`
	requeueArgs := []any{ocrStatusPending, ocrStatusInProgress}

	if olderThan > 0 {
		cutoff := time.Now().Add(-olderThan).UnixMilli()
		failSQL += ` AND started_at < ?`
		failArgs = append(failArgs, cutoff)
		requeueSQL += ` AND started_at < ?`
		requeueArgs = append(requeueArgs, cutoff)
	}

	// Fail the exhausted ones first so the requeue below can't pick them up.
	res, err := s.db.ExecContext(ctx, failSQL, failArgs...)
	if err != nil {
		return 0, 0, fmt.Errorf("fail exhausted remarkable OCR jobs: %w", err)
	}
	failed, _ = res.RowsAffected()

	res, err = s.db.ExecContext(ctx, requeueSQL, requeueArgs...)
	if err != nil {
		return 0, failed, fmt.Errorf("reclaim stuck remarkable OCR jobs: %w", err)
	}
	requeued, _ = res.RowsAffected()

	return requeued, failed, nil
}

func (s *store) ocrQueueStatus(ctx context.Context) (OCRQueueStatus, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT status, COUNT(*)
		FROM remarkable_ocr_jobs
		GROUP BY status`)
	if err != nil {
		return OCRQueueStatus{}, err
	}
	defer rows.Close()
	var out OCRQueueStatus
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return OCRQueueStatus{}, err
		}
		switch status {
		case ocrStatusPending:
			out.Pending = count
		case ocrStatusInProgress:
			out.InProgress = count
		case ocrStatusDone:
			out.Done = count
		case ocrStatusFailed:
			out.Failed = count
		}
	}
	return out, rows.Err()
}
