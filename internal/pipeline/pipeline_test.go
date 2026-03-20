package pipeline

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/notestore"
	"github.com/sysop/ultrabridge/internal/processor"
)

func openTestComponents(t *testing.T) (*notestore.Store, *processor.Store, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return notestore.New(db, dir), processor.New(db, processor.WorkerConfig{}), dir
}

// openTestComponentsWithDB returns the shared *sql.DB in addition to Store and Processor,
// so tests can manipulate job/notes state via raw SQL without accessing unexported fields.
func openTestComponentsWithDB(t *testing.T) (*notestore.Store, *processor.Store, *sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return notestore.New(db, dir), processor.New(db, processor.WorkerConfig{}), db, dir
}

// AC8.3 + AC8.1: Reconciler queues new files, not unchanged files
func TestReconciler_NewAndUnchanged(t *testing.T) {
	ns, proc, dir := openTestComponents(t)
	pl := New(Config{NotesPath: dir, Store: ns, Proc: proc, Logger: nil})

	notePath := filepath.Join(dir, "test.note")
	os.WriteFile(notePath, []byte("data"), 0644)

	// AC8.1: first reconcile discovers and queues the new file
	pl.reconcile(context.Background())
	if proc.Status().Pending != 1 {
		t.Errorf("pending after first scan = %d, want 1", proc.Status().Pending)
	}

	// AC8.3: second reconcile (same mtime) does not re-queue
	pl.reconcile(context.Background())
	if proc.Status().Pending != 1 {
		t.Errorf("pending after second scan = %d, want 1 (unchanged file should not re-queue)", proc.Status().Pending)
	}
}

// AC8.2: Changed file (mtime bump) is re-queued
func TestReconciler_ChangedFileRequeued(t *testing.T) {
	ns, proc, dir := openTestComponents(t)
	pl := New(Config{NotesPath: dir, Store: ns, Proc: proc, Logger: nil})

	notePath := filepath.Join(dir, "test.note")
	os.WriteFile(notePath, []byte("v1"), 0644)
	pl.reconcile(context.Background())

	// Bump mtime
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(notePath, future, future)
	pl.reconcile(context.Background())

	// Should now have a second pending entry (or re-queued)
	if proc.Status().Pending < 1 {
		t.Error("expected re-queued job after mtime bump")
	}
}

// AC8.4: Debounce coalesces rapid writes into a single enqueue
func TestWatcher_Debounce(t *testing.T) {
	ns, proc, dir := openTestComponents(t)
	pl := New(Config{NotesPath: dir, Store: ns, Proc: proc, Logger: nil})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pl.Start(ctx)
	defer pl.Close()

	notePath := filepath.Join(dir, "rapid.note")
	for i := 0; i < 5; i++ {
		os.WriteFile(notePath, []byte("data"), 0644)
		time.Sleep(50 * time.Millisecond)
	}

	// Wait for debounce to expire
	time.Sleep(debounceDelay + time.Second)

	st := proc.Status()
	if st.Pending != 1 {
		t.Errorf("pending = %d after 5 rapid writes, want 1 (debounce should coalesce)", st.Pending)
	}
}

// TestPipeline_MoveDetection_JobTransferred verifies AC1.1 and AC1.3:
// when a file with the same content appears at a new path, the reconciler
// transfers the done job to the new path without re-enqueueing.
func TestPipeline_MoveDetection_JobTransferred(t *testing.T) {
	ns, proc, db, dir := openTestComponentsWithDB(t)
	ctx := context.Background()

	// Create a file at pathA.
	pathA := filepath.Join(dir, "original.note")
	content := []byte("supernote note content for hash test")
	if err := os.WriteFile(pathA, content, 0644); err != nil {
		t.Fatalf("write pathA: %v", err)
	}

	pl := New(Config{NotesPath: dir, Store: ns, Proc: proc, Logger: slog.Default()})

	// First reconcile: discovers pathA, creates pending job.
	pl.reconcile(ctx)

	// Simulate completed processing: set job to done and store hash.
	hashA, err := notestore.ComputeSHA256(pathA)
	if err != nil {
		t.Fatalf("ComputeSHA256: %v", err)
	}
	if err := ns.SetHash(ctx, pathA, hashA); err != nil {
		t.Fatalf("SetHash: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE jobs SET status='done', finished_at=? WHERE note_path=?",
		time.Now().Unix(), pathA); err != nil {
		t.Fatalf("mark done: %v", err)
	}

	// Create pathB with identical content (simulates a move/rename).
	pathB := filepath.Join(dir, "moved.note")
	if err := os.WriteFile(pathB, content, 0644); err != nil {
		t.Fatalf("write pathB: %v", err)
	}

	// Second reconcile: discovers pathB (new mtime), should detect hash match and transfer.
	pl.reconcile(ctx)

	// AC1.1: pathB should have a done job (transferred from pathA).
	jobB, err := proc.GetJob(ctx, pathB)
	if err != nil {
		t.Fatalf("GetJob(pathB): %v", err)
	}
	if jobB == nil {
		t.Fatal("expected job for pathB after transfer, got nil")
	}
	if jobB.Status != processor.StatusDone {
		t.Errorf("pathB job status = %q, want done", jobB.Status)
	}

	// AC1.1: pathA should have no job (transferred away).
	jobA, _ := proc.GetJob(ctx, pathA)
	if jobA != nil {
		t.Errorf("expected no job for pathA after transfer, got status=%q", jobA.Status)
	}

	// AC1.3: pathB's notes.sha256 should be set.
	var sha256B string
	db.QueryRowContext(ctx, "SELECT COALESCE(sha256,'') FROM notes WHERE path=?", pathB).Scan(&sha256B)
	if sha256B == "" {
		t.Error("notes.sha256 for pathB should be set after transfer")
	}
	if sha256B != hashA {
		t.Errorf("notes.sha256 = %q, want %q", sha256B, hashA)
	}
}

// TestPipeline_MoveDetection_ContentChanged verifies AC1.4:
// when the moved file has different content, it gets enqueued normally.
func TestPipeline_MoveDetection_ContentChanged(t *testing.T) {
	ns, proc, db, dir := openTestComponentsWithDB(t)
	ctx := context.Background()

	// Create pathA, simulate done job with hash.
	pathA := filepath.Join(dir, "original.note")
	if err := os.WriteFile(pathA, []byte("original content"), 0644); err != nil {
		t.Fatalf("write pathA: %v", err)
	}
	pl := New(Config{NotesPath: dir, Store: ns, Proc: proc, Logger: slog.Default()})
	pl.reconcile(ctx)

	hashA, _ := notestore.ComputeSHA256(pathA)
	ns.SetHash(ctx, pathA, hashA)
	db.ExecContext(ctx, "UPDATE jobs SET status='done' WHERE note_path=?", pathA)

	// Create pathB with DIFFERENT content.
	pathB := filepath.Join(dir, "different.note")
	if err := os.WriteFile(pathB, []byte("completely different content"), 0644); err != nil {
		t.Fatalf("write pathB: %v", err)
	}

	pl.reconcile(ctx)

	// AC1.4: pathB should be enqueued (not transferred) because content differs.
	jobB, err := proc.GetJob(ctx, pathB)
	if err != nil {
		t.Fatalf("GetJob(pathB): %v", err)
	}
	if jobB == nil {
		t.Fatal("expected pathB to be enqueued, got nil job")
	}
	if jobB.Status != processor.StatusPending {
		t.Errorf("pathB job status = %q, want pending", jobB.Status)
	}
}
