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
