package remarkable

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestOCRQueue_EnqueueClaimStatusAndRevisionStaleness(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := newStore(db, t.TempDir())

	if err := st.enqueueOCRPage(ctx, "doc-1", 0, "rev-a", false, false); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	status, err := st.ocrQueueStatus(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Pending != 1 {
		t.Fatalf("pending = %d, want 1", status.Pending)
	}

	job, err := st.claimNextOCRJob(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if job == nil || job.DocumentID != "doc-1" || job.Page != 0 || job.Revision != "rev-a" {
		t.Fatalf("job = %+v", job)
	}
	if err := st.failOCRJob(ctx, job.ID, "boom"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	status, err = st.ocrQueueStatus(ctx)
	if err != nil {
		t.Fatalf("status after fail: %v", err)
	}
	if status.Failed != 1 || status.Pending != 0 {
		t.Fatalf("status after fail = %+v", status)
	}

	if err := st.enqueueOCRPage(ctx, "doc-1", 0, "rev-a", false, false); err != nil {
		t.Fatalf("same revision enqueue: %v", err)
	}
	status, err = st.ocrQueueStatus(ctx)
	if err != nil {
		t.Fatalf("status same revision: %v", err)
	}
	if status.Failed != 1 || status.Pending != 0 {
		t.Fatalf("same revision should not requeue failed job: %+v", status)
	}

	if err := st.enqueueOCRPage(ctx, "doc-1", 0, "rev-b", false, false); err != nil {
		t.Fatalf("new revision enqueue: %v", err)
	}
	status, err = st.ocrQueueStatus(ctx)
	if err != nil {
		t.Fatalf("status new revision: %v", err)
	}
	if status.Pending != 1 || status.Failed != 0 {
		t.Fatalf("new revision status = %+v", status)
	}

	if err := st.enqueueOCRPage(ctx, "doc-1", 0, "rev-b", true, true); err != nil {
		t.Fatalf("force enqueue: %v", err)
	}
	var attempts int
	if err := db.QueryRowContext(ctx, `SELECT attempts FROM remarkable_ocr_jobs WHERE document_id='doc-1' AND page=0`).Scan(&attempts); err != nil {
		t.Fatalf("query attempts: %v", err)
	}
	if attempts != 0 {
		t.Fatalf("force attempts = %d, want 0", attempts)
	}
	var manual int
	if err := db.QueryRowContext(ctx, `SELECT manual FROM remarkable_ocr_jobs WHERE document_id='doc-1' AND page=0`).Scan(&manual); err != nil {
		t.Fatalf("query manual: %v", err)
	}
	if manual != 1 {
		t.Fatalf("force manual = %d, want 1", manual)
	}
}

func TestShouldAutoOCRDocumentSkipsPDFAndEPUB(t *testing.T) {
	tests := []struct {
		name string
		doc  RenderDocument
		want bool
	}{
		{
			name: "notebook with rm pages",
			doc: RenderDocument{
				FileType: "notebook",
				PageRM:   map[string]RenderBlob{"page-1": {Hash: "h"}},
			},
			want: true,
		},
		{
			name: "pdf file type",
			doc: RenderDocument{
				FileType: "pdf",
				PageRM:   map[string]RenderBlob{"page-1": {Hash: "h"}},
			},
			want: false,
		},
		{
			name: "epub file type",
			doc: RenderDocument{
				FileType: "epub",
				PageRM:   map[string]RenderBlob{"page-1": {Hash: "h"}},
			},
			want: false,
		},
		{
			name: "pdf backing file",
			doc: RenderDocument{
				FileType: "notebook",
				PDFPath:  "/tmp/source.pdf",
				PageRM:   map[string]RenderBlob{"page-1": {Hash: "h"}},
			},
			want: false,
		},
		{
			name: "empty file type with rm pages",
			doc: RenderDocument{
				PageRM: map[string]RenderBlob{"page-1": {Hash: "h"}},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldAutoOCRDocument(tt.doc); got != tt.want {
				t.Fatalf("shouldAutoOCRDocument = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDeleteAutomaticOCRJobsPreservesManual(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := newStore(db, t.TempDir())
	if err := st.enqueueOCRPage(ctx, "doc-1", 0, "rev", true, false); err != nil {
		t.Fatalf("enqueue auto: %v", err)
	}
	if err := st.enqueueOCRPage(ctx, "doc-1", 1, "rev", true, true); err != nil {
		t.Fatalf("enqueue manual: %v", err)
	}
	if err := st.deleteAutomaticOCRJobs(ctx, "doc-1"); err != nil {
		t.Fatalf("delete automatic: %v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM remarkable_ocr_jobs WHERE document_id='doc-1'`).Scan(&count); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if count != 1 {
		t.Fatalf("remaining jobs = %d, want 1", count)
	}
	var page int
	if err := db.QueryRowContext(ctx, `SELECT page FROM remarkable_ocr_jobs WHERE document_id='doc-1'`).Scan(&page); err != nil {
		t.Fatalf("remaining page: %v", err)
	}
	if page != 1 {
		t.Fatalf("remaining page = %d, want manual page 1", page)
	}
}

// TestReclaimStuckOCRJobs_StartupReclaimsOrphans reproduces the state found in
// production on 2026-07-26: two jobs claimed on 2026-06-24 and left
// in_progress for a month by a process that died mid-job. Nothing could rescue
// them — the non-forced re-enqueue is revision-gated, so every restart skipped
// right past them and the pipeline status bar read "2 in progress" forever.
//
// An unbounded reclaim (what Start does) must requeue them regardless of age.
func TestReclaimStuckOCRJobs_StartupReclaimsOrphans(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := newStore(db, t.TempDir())

	if err := st.enqueueOCRPage(ctx, "doc-1", 0, "rev-a", false, false); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	job, err := st.claimNextOCRJob(ctx)
	if err != nil || job == nil {
		t.Fatalf("claim: job=%v err=%v", job, err)
	}

	// The worker dies here — the row stays in_progress.
	status, err := st.ocrQueueStatus(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.InProgress != 1 {
		t.Fatalf("InProgress = %d, want 1", status.InProgress)
	}

	// A re-enqueue at the same revision must NOT rescue it — this is the
	// gate that made the wedge permanent. Pinning it here so the reclaim
	// path can't be quietly deleted as redundant later.
	if err := st.enqueueOCRPage(ctx, "doc-1", 0, "rev-a", false, false); err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	if status, _ = st.ocrQueueStatus(ctx); status.InProgress != 1 {
		t.Fatalf("revision-gated re-enqueue changed status: %+v", status)
	}

	requeued, failed, err := st.reclaimStuckOCRJobs(ctx, 0)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if requeued != 1 || failed != 0 {
		t.Fatalf("reclaim = (%d requeued, %d failed), want (1, 0)", requeued, failed)
	}
	status, err = st.ocrQueueStatus(ctx)
	if err != nil {
		t.Fatalf("status after reclaim: %v", err)
	}
	if status.InProgress != 0 || status.Pending != 1 {
		t.Fatalf("after reclaim: %+v, want InProgress=0 Pending=1", status)
	}

	// And the requeued job is claimable again.
	if job, err = st.claimNextOCRJob(ctx); err != nil || job == nil {
		t.Fatalf("re-claim: job=%v err=%v", job, err)
	}
}

// TestReclaimStuckOCRJobs_AgeBounded covers the watchdog's bounded sweep: a
// job claimed moments ago is still legitimately in flight and must be left
// alone, while one past the threshold is reclaimed.
func TestReclaimStuckOCRJobs_AgeBounded(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := newStore(db, t.TempDir())

	for i, doc := range []string{"fresh", "stale"} {
		if err := st.enqueueOCRPage(ctx, doc, i, "rev", false, false); err != nil {
			t.Fatalf("enqueue %s: %v", doc, err)
		}
	}
	for range 2 {
		if _, err := st.claimNextOCRJob(ctx); err != nil {
			t.Fatalf("claim: %v", err)
		}
	}
	// Backdate only the "stale" job well past the threshold.
	old := time.Now().Add(-2 * ocrStuckAfter).UnixMilli()
	if _, err := db.ExecContext(ctx,
		`UPDATE remarkable_ocr_jobs SET started_at = ? WHERE document_id = 'stale'`, old); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	requeued, failed, err := st.reclaimStuckOCRJobs(ctx, ocrStuckAfter)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if requeued != 1 || failed != 0 {
		t.Fatalf("reclaim = (%d requeued, %d failed), want (1, 0)", requeued, failed)
	}
	status, err := st.ocrQueueStatus(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.InProgress != 1 || status.Pending != 1 {
		t.Fatalf("after bounded reclaim: %+v, want InProgress=1 Pending=1", status)
	}
	var stuckDoc string
	if err := db.QueryRowContext(ctx,
		`SELECT document_id FROM remarkable_ocr_jobs WHERE status = 'in_progress'`).Scan(&stuckDoc); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if stuckDoc != "fresh" {
		t.Errorf("watchdog reclaimed the in-flight job %q, want it to spare 'fresh'", stuckDoc)
	}
}

// TestReclaimStuckOCRJobs_AttemptCap stops the reclaim loop from becoming its
// own infinite cycle: a page that reliably kills the worker must eventually
// land in `failed` rather than churning pending -> in_progress -> reclaimed.
func TestReclaimStuckOCRJobs_AttemptCap(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := newStore(db, t.TempDir())

	if err := st.enqueueOCRPage(ctx, "poison", 0, "rev", false, false); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Each cycle is one claim (attempts++) followed by a crash + reclaim.
	for i := 1; i < ocrMaxAttempts; i++ {
		if _, err := st.claimNextOCRJob(ctx); err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		requeued, failed, err := st.reclaimStuckOCRJobs(ctx, 0)
		if err != nil {
			t.Fatalf("reclaim %d: %v", i, err)
		}
		if requeued != 1 || failed != 0 {
			t.Fatalf("cycle %d: reclaim = (%d, %d), want (1, 0)", i, requeued, failed)
		}
	}

	// The claim that reaches the cap gets failed, not requeued.
	if _, err := st.claimNextOCRJob(ctx); err != nil {
		t.Fatalf("final claim: %v", err)
	}
	requeued, failed, err := st.reclaimStuckOCRJobs(ctx, 0)
	if err != nil {
		t.Fatalf("final reclaim: %v", err)
	}
	if requeued != 0 || failed != 1 {
		t.Fatalf("final reclaim = (%d requeued, %d failed), want (0, 1)", requeued, failed)
	}
	status, err := st.ocrQueueStatus(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Failed != 1 || status.Pending != 0 || status.InProgress != 0 {
		t.Fatalf("after cap: %+v, want Failed=1 only", status)
	}
	var lastErr string
	if err := db.QueryRowContext(ctx,
		`SELECT last_error FROM remarkable_ocr_jobs WHERE document_id = 'poison'`).Scan(&lastErr); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !strings.Contains(lastErr, "abandoned in_progress") {
		t.Errorf("last_error = %q, want it to explain the abandonment", lastErr)
	}
}
