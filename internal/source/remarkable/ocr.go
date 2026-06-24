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
		done:       make(chan struct{}),
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
	go p.loop(runCtx)
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
	p.cancel()
	<-p.done
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
	if err := p.enqueueDocument(ctx, doc, true); err != nil {
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
		if err := p.enqueueDocument(ctx, doc, false); err != nil {
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

func (p *ocrProcessor) enqueueDocument(ctx context.Context, doc RenderDocument, force bool) error {
	pages := doc.PageCount
	if pages == 0 {
		pages = len(doc.PageOrder)
	}
	if pages == 0 && doc.PDFPath != "" {
		pages = 1
	}
	for i := 0; i < pages; i++ {
		if err := p.store.enqueueOCRPage(ctx, doc.ID, i, doc.Revision, force); err != nil {
			return fmt.Errorf("enqueue %s page %d: %w", doc.ID, i, err)
		}
	}
	return nil
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
		_ = p.store.enqueueOCRPage(ctx, doc.ID, job.Page, doc.Revision, true)
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

func (s *store) enqueueOCRPage(ctx context.Context, documentID string, page int, revision string, force bool) error {
	now := time.Now().UnixMilli()
	if force {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO remarkable_ocr_jobs(document_id, page, revision, status, attempts, last_error, queued_at, started_at, finished_at)
			VALUES(?, ?, ?, ?, 0, '', ?, 0, 0)
			ON CONFLICT(document_id, page) DO UPDATE SET
				revision=excluded.revision,
				status=excluded.status,
				attempts=0,
				last_error='',
				queued_at=excluded.queued_at,
				started_at=0,
				finished_at=0`,
			documentID, page, revision, ocrStatusPending, now)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO remarkable_ocr_jobs(document_id, page, revision, status, attempts, last_error, queued_at, started_at, finished_at)
		VALUES(?, ?, ?, ?, 0, '', ?, 0, 0)
		ON CONFLICT(document_id, page) DO UPDATE SET
			revision=excluded.revision,
			status=excluded.status,
			attempts=0,
			last_error='',
			queued_at=excluded.queued_at,
			started_at=0,
			finished_at=0
		WHERE remarkable_ocr_jobs.revision <> excluded.revision`,
		documentID, page, revision, ocrStatusPending, now)
	return err
}

func (s *store) claimNextOCRJob(ctx context.Context) (*ocrJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var job ocrJob
	err = tx.QueryRowContext(ctx, `
		SELECT id, document_id, page, revision
		FROM remarkable_ocr_jobs
		WHERE status = ?
		ORDER BY queued_at ASC, id ASC
		LIMIT 1`, ocrStatusPending).
		Scan(&job.ID, &job.DocumentID, &job.Page, &job.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
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
