package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	rmsource "github.com/sysop/ultrabridge/internal/source/remarkable"
)

// fakeRemarkableFileManager records calls and echoes back configurable errors.
type fakeRemarkableFileManager struct {
	err      error
	uploaded []string
	renamed  []string
}

func (f *fakeRemarkableFileManager) UploadDocument(ctx context.Context, filename, parentID string, payload io.Reader) (rmsource.Document, error) {
	if f.err != nil {
		return rmsource.Document{}, f.err
	}
	f.uploaded = append(f.uploaded, filename+"->"+parentID)
	return rmsource.Document{ID: "doc-1", Name: "Doc", Type: "document", Parent: parentID, FileType: "pdf"}, nil
}
func (f *fakeRemarkableFileManager) DownloadDocument(ctx context.Context, documentID string) (io.ReadCloser, string, string, error) {
	if f.err != nil {
		return nil, "", "", f.err
	}
	return io.NopCloser(strings.NewReader("bytes")), `Bad"Name` + "\n" + `.pdf`, "application/pdf", nil
}
func (f *fakeRemarkableFileManager) DeleteDocument(ctx context.Context, documentID string) error {
	return f.err
}
func (f *fakeRemarkableFileManager) CreateFolder(ctx context.Context, name, parentID string) (rmsource.Document, error) {
	if f.err != nil {
		return rmsource.Document{}, f.err
	}
	return rmsource.Document{ID: "folder-1", Name: name, Type: "folder", Parent: parentID}, nil
}
func (f *fakeRemarkableFileManager) MoveNode(ctx context.Context, documentID, newParentID string) error {
	return f.err
}
func (f *fakeRemarkableFileManager) RenameNode(ctx context.Context, documentID, newName string) error {
	if f.err != nil {
		return f.err
	}
	f.renamed = append(f.renamed, documentID+"->"+newName)
	return nil
}

func newRemarkableFilesService(mgr RemarkableFileManager) *noteService {
	s := &noteService{}
	s.SetRemarkableFileManager(mgr)
	return s
}

func TestRemarkableFileManager_NilGating(t *testing.T) {
	s := &noteService{}
	if s.HasRemarkableFileManager() {
		t.Fatal("HasRemarkableFileManager = true with nil seam")
	}
	if _, err := s.UploadRemarkableDocument(context.Background(), "a.pdf", "", strings.NewReader("x")); err == nil {
		t.Fatal("upload with nil seam succeeded")
	}
	if err := s.DeleteRemarkableDocument(context.Background(), "doc-1"); err == nil {
		t.Fatal("delete with nil seam succeeded")
	}
	if _, err := s.CreateRemarkableFolder(context.Background(), "F", ""); err == nil {
		t.Fatal("create folder with nil seam succeeded")
	}
}

func TestRemarkableFileManager_PassThroughAndValidation(t *testing.T) {
	fake := &fakeRemarkableFileManager{}
	s := newRemarkableFilesService(fake)
	ctx := context.Background()

	if !s.HasRemarkableFileManager() {
		t.Fatal("HasRemarkableFileManager = false")
	}
	doc, err := s.UploadRemarkableDocument(ctx, "  book.pdf  ", "folder-1", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if doc.FileType != "pdf" {
		t.Fatalf("doc = %+v", doc)
	}
	if len(fake.uploaded) != 1 || fake.uploaded[0] != "book.pdf->folder-1" {
		t.Fatalf("uploaded = %v (filename must be trimmed)", fake.uploaded)
	}
	if _, err := s.UploadRemarkableDocument(ctx, "   ", "", strings.NewReader("x")); !errors.Is(err, ErrRemarkableUnsupportedFile) {
		t.Fatalf("blank filename = %v, want ErrRemarkableUnsupportedFile", err)
	}

	// Download filename is sanitized for the Content-Disposition header.
	rc, name, contentType, err := s.DownloadRemarkableDocument(ctx, "doc-1")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	rc.Close()
	if strings.ContainsAny(name, "\"\n\\") {
		t.Fatalf("download name %q not sanitized", name)
	}
	if contentType != "application/pdf" {
		t.Fatalf("contentType = %q", contentType)
	}

	if err := s.RenameRemarkableNode(ctx, "doc-1", "  New Name  "); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if len(fake.renamed) != 1 || fake.renamed[0] != "doc-1->New Name" {
		t.Fatalf("renamed = %v (name must be trimmed)", fake.renamed)
	}
	if err := s.RenameRemarkableNode(ctx, "doc-1", "   "); err == nil {
		t.Fatal("blank rename succeeded")
	}
	if _, err := s.CreateRemarkableFolder(ctx, "   ", ""); err == nil {
		t.Fatal("blank folder name succeeded")
	}

	// Source sentinels pass through errors.Is-able.
	fake.err = rmsource.ErrTreeConflict
	if err := s.DeleteRemarkableDocument(ctx, "doc-1"); !errors.Is(err, ErrRemarkableTreeConflict) {
		t.Fatalf("delete = %v, want ErrRemarkableTreeConflict", err)
	}
	if err := s.MoveRemarkableNode(ctx, "doc-1", "f"); !errors.Is(err, rmsource.ErrTreeConflict) {
		t.Fatalf("move = %v, want source sentinel", err)
	}
}
