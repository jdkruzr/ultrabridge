package remarkable

import (
	"context"
	"testing"
)

func TestOCRQueue_EnqueueClaimStatusAndRevisionStaleness(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := newStore(db, t.TempDir())

	if err := st.enqueueOCRPage(ctx, "doc-1", 0, "rev-a", false); err != nil {
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

	if err := st.enqueueOCRPage(ctx, "doc-1", 0, "rev-a", false); err != nil {
		t.Fatalf("same revision enqueue: %v", err)
	}
	status, err = st.ocrQueueStatus(ctx)
	if err != nil {
		t.Fatalf("status same revision: %v", err)
	}
	if status.Failed != 1 || status.Pending != 0 {
		t.Fatalf("same revision should not requeue failed job: %+v", status)
	}

	if err := st.enqueueOCRPage(ctx, "doc-1", 0, "rev-b", false); err != nil {
		t.Fatalf("new revision enqueue: %v", err)
	}
	status, err = st.ocrQueueStatus(ctx)
	if err != nil {
		t.Fatalf("status new revision: %v", err)
	}
	if status.Pending != 1 || status.Failed != 0 {
		t.Fatalf("new revision status = %+v", status)
	}

	if err := st.enqueueOCRPage(ctx, "doc-1", 0, "rev-b", true); err != nil {
		t.Fatalf("force enqueue: %v", err)
	}
	var attempts int
	if err := db.QueryRowContext(ctx, `SELECT attempts FROM remarkable_ocr_jobs WHERE document_id='doc-1' AND page=0`).Scan(&attempts); err != nil {
		t.Fatalf("query attempts: %v", err)
	}
	if attempts != 0 {
		t.Fatalf("force attempts = %d, want 0", attempts)
	}
}
