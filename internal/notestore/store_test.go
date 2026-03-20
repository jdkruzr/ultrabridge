package notestore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/notedb"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db, t.TempDir())
}

func TestClassifyFileType(t *testing.T) {
	cases := []struct {
		ext  string
		want FileType
	}{
		{".note", FileTypeNote},
		{".NOTE", FileTypeNote},
		{".pdf", FileTypePDF},
		{".PDF", FileTypePDF},
		{".epub", FileTypeEPUB},
		{".mark", FileTypeOther},
		{".mobi", FileTypeOther},
		{"", FileTypeOther},
	}
	for _, c := range cases {
		if got := ClassifyFileType(c.ext); got != c.want {
			t.Errorf("ClassifyFileType(%q) = %q, want %q", c.ext, got, c.want)
		}
	}
}

func TestScan_NewFileDiscovered(t *testing.T) {
	s := openTestStore(t)
	noteFile := filepath.Join(s.notesPath, "test.note")
	if err := os.WriteFile(noteFile, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	changed, err := s.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(changed) != 1 || changed[0] != noteFile {
		t.Errorf("Scan changed = %v, want [%s]", changed, noteFile)
	}

	nf, err := s.Get(context.Background(), noteFile)
	if err != nil {
		t.Fatalf("Get after scan: %v", err)
	}
	if nf.FileType != FileTypeNote {
		t.Errorf("FileType = %q, want note", nf.FileType)
	}
}

func TestScan_ChangedFileDetectedByMtime(t *testing.T) {
	s := openTestStore(t)
	noteFile := filepath.Join(s.notesPath, "test.note")
	if err := os.WriteFile(noteFile, []byte("v1"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(noteFile, future, future); err != nil {
		t.Fatal(err)
	}

	changed, err := s.Scan(context.Background())
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if len(changed) != 1 {
		t.Errorf("expected 1 changed file, got %d: %v", len(changed), changed)
	}
}

func TestScan_UnchangedFileNotReported(t *testing.T) {
	s := openTestStore(t)
	noteFile := filepath.Join(s.notesPath, "test.note")
	if err := os.WriteFile(noteFile, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	changed, err := s.Scan(context.Background())
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("expected 0 changed, got %d: %v", len(changed), changed)
	}
}

func TestList_ReturnsDirectChildren(t *testing.T) {
	s := openTestStore(t)
	root := filepath.Join(s.notesPath, "root.note")
	subdir := filepath.Join(s.notesPath, "sub")
	subfile := filepath.Join(subdir, "child.note")
	os.MkdirAll(subdir, 0755)
	os.WriteFile(root, []byte("r"), 0644)
	os.WriteFile(subfile, []byte("c"), 0644)

	if _, err := s.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	files, err := s.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// List("") returns: subdirectories first (from os.ReadDir), then files from DB.
	// Expects: [sub/ (dir), root.note (file)]
	if len(files) != 2 {
		t.Fatalf("List(\"\") returned %d items, want 2 (sub dir + root.note)", len(files))
	}
	if files[0].Name != "sub" || !files[0].IsDir {
		t.Errorf("files[0] = %+v, want sub dir", files[0])
	}
	if files[1].Name != "root.note" {
		t.Errorf("files[1] = %+v, want root.note", files[1])
	}

	subFiles, err := s.List(context.Background(), "sub")
	if err != nil {
		t.Fatalf("List sub: %v", err)
	}
	if len(subFiles) != 1 || subFiles[0].Name != "child.note" {
		t.Errorf("List(\"sub\") = %v, want [child.note]", subFiles)
	}
}

func TestGet_NotFound(t *testing.T) {
	s := openTestStore(t)
	_, err := s.Get(context.Background(), "/nonexistent.note")
	if !IsNotFound(err) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestUpsertFile_InsertPath(t *testing.T) {
	s := openTestStore(t)
	noteFile := filepath.Join(s.notesPath, "new.note")
	if err := os.WriteFile(noteFile, []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}

	// UpsertFile should insert the file
	if err := s.UpsertFile(context.Background(), noteFile); err != nil {
		t.Fatalf("UpsertFile insert: %v", err)
	}

	// Verify the file was inserted
	nf, err := s.Get(context.Background(), noteFile)
	if err != nil {
		t.Fatalf("Get after upsert: %v", err)
	}
	if nf.FileType != FileTypeNote {
		t.Errorf("FileType = %q, want note", nf.FileType)
	}
}

func TestUpsertFile_ConflictUpdatePath(t *testing.T) {
	s := openTestStore(t)
	noteFile := filepath.Join(s.notesPath, "existing.note")
	if err := os.WriteFile(noteFile, []byte("v1"), 0644); err != nil {
		t.Fatal(err)
	}

	// First upsert (insert)
	if err := s.UpsertFile(context.Background(), noteFile); err != nil {
		t.Fatalf("UpsertFile first: %v", err)
	}
	nf1, _ := s.Get(context.Background(), noteFile)
	size1 := nf1.SizeBytes

	// Update the file
	if err := os.WriteFile(noteFile, []byte("v1_updated_content"), 0644); err != nil {
		t.Fatal(err)
	}

	// Second upsert (should update on conflict)
	if err := s.UpsertFile(context.Background(), noteFile); err != nil {
		t.Fatalf("UpsertFile second: %v", err)
	}

	nf2, _ := s.Get(context.Background(), noteFile)
	if nf2.SizeBytes <= size1 {
		t.Errorf("size after conflict update = %d, want > %d (file was updated)", nf2.SizeBytes, size1)
	}
}

func TestComputeSHA256(t *testing.T) {
	// Create a temporary file with known content
	tmpFile, err := os.CreateTemp("", "test-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	content := []byte("test content for sha256")
	if _, err := tmpFile.Write(content); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	// Compute SHA256
	digest, err := ComputeSHA256(tmpFile.Name())
	if err != nil {
		t.Fatalf("ComputeSHA256: %v", err)
	}

	// Verify the digest is a valid hex string (64 chars for SHA256)
	if len(digest) != 64 {
		t.Errorf("digest length = %d, want 64 (SHA256 hex)", len(digest))
	}

	// Verify the digest matches expected value (precomputed)
	// echo -n "test content for sha256" | sha256sum
	// -> 47914c8afb6da51b436bca58d0fd288d7cd3ea252f778b57617b86f12306c20f
	expectedDigest := "47914c8afb6da51b436bca58d0fd288d7cd3ea252f778b57617b86f12306c20f"
	if digest != expectedDigest {
		t.Errorf("digest = %s, want %s", digest, expectedDigest)
	}
}

// TestScan_PrunesOrphans verifies that Scan removes DB entries for files that
// have been moved or deleted since the last scan (e.g. after a device re-org).
func TestScan_PrunesOrphans(t *testing.T) {
	s := openTestStore(t)
	notePath := filepath.Join(s.notesPath, "test.note")

	// First scan: file exists, gets inserted
	if err := os.WriteFile(notePath, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(context.Background()); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if _, err := s.Get(context.Background(), notePath); err != nil {
		t.Fatalf("Get before delete: %v", err)
	}

	// File is moved/deleted
	if err := os.Remove(notePath); err != nil {
		t.Fatal(err)
	}

	// Second scan: file gone, DB entry should be pruned
	if _, err := s.Scan(context.Background()); err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	_, err := s.Get(context.Background(), notePath)
	if !IsNotFound(err) {
		t.Errorf("expected ErrNotFound after prune, got %v", err)
	}
}

// TestSetHash verifies that SetHash writes the sha256 column for the given path.
func TestSetHash(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().Unix()

	// Seed a notes row.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notes (path, rel_path, file_type, size_bytes, mtime, created_at, updated_at)
		VALUES (?, ?, 'note', 0, ?, ?, ?)`, "/a.note", "a.note", now, now, now)
	if err != nil {
		t.Fatalf("seed notes: %v", err)
	}

	if err := s.SetHash(ctx, "/a.note", "abc123"); err != nil {
		t.Fatalf("SetHash: %v", err)
	}

	var got string
	if err := s.db.QueryRowContext(ctx, "SELECT sha256 FROM notes WHERE path=?", "/a.note").Scan(&got); err != nil {
		t.Fatalf("read sha256: %v", err)
	}
	if got != "abc123" {
		t.Errorf("sha256 = %q, want abc123", got)
	}
}

// TestLookupByHash_Found verifies LookupByHash returns the path when a matching
// sha256 exists with a done job.
func TestLookupByHash_Found(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().Unix()

	// Seed notes row + done job + hash.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notes (path, rel_path, file_type, size_bytes, mtime, created_at, updated_at)
		VALUES (?, ?, 'note', 0, ?, ?, ?)`, "/a.note", "a.note", now, now, now)
	if err != nil {
		t.Fatalf("seed notes: %v", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO jobs (note_path, status, queued_at) VALUES (?, 'done', ?)`, "/a.note", now)
	if err != nil {
		t.Fatalf("seed jobs: %v", err)
	}
	if err := s.SetHash(ctx, "/a.note", "deadbeef"); err != nil {
		t.Fatalf("SetHash: %v", err)
	}

	path, found, err := s.LookupByHash(ctx, "deadbeef")
	if err != nil {
		t.Fatalf("LookupByHash: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if path != "/a.note" {
		t.Errorf("path = %q, want /a.note", path)
	}
}

// TestLookupByHash_NotFound verifies LookupByHash returns found=false for an unknown hash.
func TestLookupByHash_NotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, found, err := s.LookupByHash(ctx, "notfound")
	if err != nil {
		t.Fatalf("LookupByHash: %v", err)
	}
	if found {
		t.Fatal("expected found=false for unknown hash")
	}
}

// TestLookupByHash_NoJob verifies LookupByHash returns found=false when a note has
// a matching sha256 but no associated job record.
func TestLookupByHash_NoJob(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().Unix()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notes (path, rel_path, file_type, size_bytes, mtime, created_at, updated_at)
		VALUES (?, ?, 'note', 0, ?, ?, ?)`, "/a.note", "a.note", now, now, now)
	if err != nil {
		t.Fatalf("seed notes: %v", err)
	}
	if err := s.SetHash(ctx, "/a.note", "deadbeef"); err != nil {
		t.Fatalf("SetHash: %v", err)
	}

	// No jobs row — LookupByHash should return found=false.
	_, found, err := s.LookupByHash(ctx, "deadbeef")
	if err != nil {
		t.Fatalf("LookupByHash: %v", err)
	}
	if found {
		t.Fatal("expected found=false when no job exists")
	}
}

// TestLookupByHash_PendingJob verifies LookupByHash returns found=false when the job
// exists but is not yet done (pending/in_progress/failed).
// This ensures in-flight jobs are not misidentified as completed moves.
func TestLookupByHash_PendingJob(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().Unix()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notes (path, rel_path, file_type, size_bytes, mtime, created_at, updated_at)
		VALUES (?, ?, 'note', 0, ?, ?, ?)`, "/a.note", "a.note", now, now, now)
	if err != nil {
		t.Fatalf("seed notes: %v", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO jobs (note_path, status, queued_at) VALUES (?, 'pending', ?)`, "/a.note", now)
	if err != nil {
		t.Fatalf("seed jobs: %v", err)
	}
	if err := s.SetHash(ctx, "/a.note", "deadbeef"); err != nil {
		t.Fatalf("SetHash: %v", err)
	}

	// Pending job — LookupByHash should return found=false (only done jobs count).
	_, found, err := s.LookupByHash(ctx, "deadbeef")
	if err != nil {
		t.Fatalf("LookupByHash: %v", err)
	}
	if found {
		t.Fatal("expected found=false for pending job (only done jobs should match)")
	}
}

// TestTransferJob verifies that TransferJob moves the job record from oldPath to newPath,
// verifying AC4.1 (job retains fields) and AC4.2 (old path job is gone).
func TestTransferJob(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().Unix()

	// Seed two notes rows.
	for _, path := range []string{"/old.note", "/new.note"} {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO notes (path, rel_path, file_type, size_bytes, mtime, created_at, updated_at)
			VALUES (?, ?, 'note', 0, ?, ?, ?)`, path, filepath.Base(path), now, now, now)
		if err != nil {
			t.Fatalf("seed notes %s: %v", path, err)
		}
	}

	// Seed a done job for /old.note with ocr_source and api_model set.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO jobs (note_path, status, ocr_source, api_model, queued_at, finished_at)
		VALUES (?, 'done', 'api', 'test-model', ?, ?)`, "/old.note", now, now)
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}

	// Transfer job to /new.note.
	if err := s.TransferJob(ctx, "/old.note", "/new.note"); err != nil {
		t.Fatalf("TransferJob: %v", err)
	}

	// AC4.2: old path has no job.
	var count int
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE note_path=?", "/old.note").Scan(&count)
	if count != 0 {
		t.Errorf("old path still has %d job(s), want 0", count)
	}

	// AC4.1: new path has the job with original fields intact.
	var status, ocrSource, apiModel string
	s.db.QueryRowContext(ctx,
		"SELECT status, COALESCE(ocr_source,''), COALESCE(api_model,'') FROM jobs WHERE note_path=?",
		"/new.note").Scan(&status, &ocrSource, &apiModel)
	if status != "done" {
		t.Errorf("status = %q, want done", status)
	}
	if ocrSource != "api" {
		t.Errorf("ocr_source = %q, want api", ocrSource)
	}
	if apiModel != "test-model" {
		t.Errorf("api_model = %q, want test-model", apiModel)
	}
}

// TestTransferJob_NoJob verifies TransferJob returns an error when no job exists
// for the given old path.
func TestTransferJob_NoJob(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	err := s.TransferJob(ctx, "/nonexistent.note", "/new.note")
	if err == nil {
		t.Error("expected error when no job exists for old path")
	}
}
