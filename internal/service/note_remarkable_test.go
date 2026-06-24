package service

import (
	"context"
	"database/sql"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/search"
	rmsource "github.com/sysop/ultrabridge/internal/source/remarkable"
)

type fakeRMReader struct {
	docs      []rmsource.Document
	renderDoc rmsource.RenderDocument
	err       error
	renderErr error
}

type fakeRMReprocessor struct {
	reprocessed []string
	status      rmsource.OCRQueueStatus
}

func (f *fakeRMReader) ListDocuments(context.Context) ([]rmsource.Document, error) {
	return f.docs, f.err
}

func (f *fakeRMReader) RenderDocument(context.Context, string) (rmsource.RenderDocument, error) {
	return f.renderDoc, f.renderErr
}

func (f *fakeRMReprocessor) ReprocessDocument(_ context.Context, documentID string) error {
	f.reprocessed = append(f.reprocessed, documentID)
	return nil
}

func (f *fakeRMReprocessor) OCRStatus(context.Context) (rmsource.OCRQueueStatus, error) {
	return f.status, nil
}

func TestNoteService_RemarkablePresenceAndFolderListing(t *testing.T) {
	s := &noteService{}
	if s.HasRemarkableSource() {
		t.Fatal("HasRemarkableSource true before reader is wired")
	}
	crumbs, entries, err := s.ListRemarkableFolder(context.Background(), "", "", "")
	if err != nil {
		t.Fatalf("nil reader list should be empty, got err: %v", err)
	}
	if len(crumbs) != 0 || len(entries) != 0 {
		t.Fatalf("nil reader returned crumbs=%v entries=%v, want empty", crumbs, entries)
	}

	s.SetRemarkableReader(&fakeRMReader{docs: []rmsource.Document{
		{ID: "doc-b", Name: "Beta", Type: "document", Parent: "folder-1", PageCount: 8},
		{ID: "folder-1", Name: "Projects", Type: "folder", Parent: ""},
		{ID: "doc-a", Name: "Alpha", Type: "document", Parent: "", PageCount: 3},
		{ID: "folder-2", Name: "Archive", Type: "folder", Parent: ""},
	}})
	if !s.HasRemarkableSource() {
		t.Fatal("HasRemarkableSource false after reader is wired")
	}

	crumbs, entries, err = s.ListRemarkableFolder(context.Background(), "", "", "")
	if err != nil {
		t.Fatalf("ListRemarkableFolder root: %v", err)
	}
	if len(crumbs) != 1 || crumbs[0].FolderID != "" || crumbs[0].Name != "Home" {
		t.Fatalf("root crumbs = %+v", crumbs)
	}
	if got := entryNames(entries); len(got) != 3 || got[0] != "Archive" || got[1] != "Projects" || got[2] != "Alpha" {
		t.Fatalf("root entries sorted folders-first = %v; entries=%+v", got, entries)
	}
	if entries[2].Path != "remarkable://doc-a" || entries[2].PageCount != 3 {
		t.Fatalf("document entry shape = %+v", entries[2])
	}

	crumbs, entries, err = s.ListRemarkableFolder(context.Background(), "folder-1", "", "")
	if err != nil {
		t.Fatalf("ListRemarkableFolder child: %v", err)
	}
	if len(crumbs) != 2 || crumbs[1].FolderID != "folder-1" || crumbs[1].Name != "Projects" {
		t.Fatalf("child crumbs = %+v", crumbs)
	}
	if got := entryNames(entries); len(got) != 1 || got[0] != "Beta" {
		t.Fatalf("child entries = %v; entries=%+v", got, entries)
	}
}

func TestNoteService_RemarkableDetail(t *testing.T) {
	s := &noteService{searchIndex: &fakeSearchIndex{byPath: map[string][]search.NoteDocument{
		"remarkable://doc-b": {
			{Path: "remarkable://doc-b", Page: -1, BodyText: "metadata should not become a page"},
			{Path: "remarkable://doc-b", Page: 1, BodyText: "second page text", Source: "api"},
		},
	}}}
	s.SetRemarkableReader(&fakeRMReader{
		docs: []rmsource.Document{
			{ID: "folder-1", Name: "Projects", Type: "folder", Parent: ""},
			{ID: "doc-b", Name: "Beta", Type: "document", Parent: "folder-1", PageCount: 8},
		},
		renderDoc: rmsource.RenderDocument{ID: "doc-b", Renderable: true},
	})

	detail, err := s.GetRemarkableDocumentDetail(context.Background(), "doc-b")
	if err != nil {
		t.Fatalf("GetRemarkableDocumentDetail: %v", err)
	}
	if detail.ID != "doc-b" || detail.Name != "Beta" || detail.Path != "remarkable://doc-b" || detail.PageCount != 8 {
		t.Fatalf("detail = %+v", detail)
	}
	if len(detail.FolderPath) != 1 || detail.FolderPath[0] != "Projects" {
		t.Fatalf("folder path = %+v", detail.FolderPath)
	}
	if !detail.RenderAvailable || detail.OCRAvailable {
		t.Fatalf("detail should advertise render only: %+v", detail)
	}
	if len(detail.Pages) != 8 || detail.Pages[1].BodyText != "second page text" || detail.Pages[1].Source != "api" {
		t.Fatalf("detail pages = %+v", detail.Pages)
	}

	if _, err := s.GetRemarkableDocumentDetail(context.Background(), "missing"); err != sql.ErrNoRows {
		t.Fatalf("missing detail err = %v, want sql.ErrNoRows", err)
	}
}

func TestNoteService_RemarkableReprocessAndStatus(t *testing.T) {
	reproc := &fakeRMReprocessor{status: rmsource.OCRQueueStatus{Pending: 2, InProgress: 1, Done: 3, Failed: 4}}
	s := &noteService{}
	s.SetRemarkableReprocessor(reproc)

	if err := s.ReprocessRemarkableDocument(context.Background(), "doc-1"); err != nil {
		t.Fatalf("ReprocessRemarkableDocument: %v", err)
	}
	if len(reproc.reprocessed) != 1 || reproc.reprocessed[0] != "doc-1" {
		t.Fatalf("reprocessed = %v", reproc.reprocessed)
	}
	status, err := s.GetProcessorStatus(context.Background())
	if err != nil {
		t.Fatalf("GetProcessorStatus: %v", err)
	}
	if status.Remarkable == nil || status.Remarkable.Pending != 2 || status.Remarkable.InProgress != 1 || status.Remarkable.Done != 3 || status.Remarkable.Failed != 4 {
		t.Fatalf("remarkable status = %+v", status.Remarkable)
	}
}

func TestNoteService_RenderRemarkablePage(t *testing.T) {
	rmPath := writeMinimalRM(t)
	s := &noteService{}
	s.SetRemarkableReader(&fakeRMReader{
		docs: []rmsource.Document{{ID: "doc-1", Name: "Sketch", Type: "document", PageCount: 1}},
		renderDoc: rmsource.RenderDocument{
			ID:         "doc-1",
			PageCount:  1,
			PageOrder:  []string{"page-1"},
			PageRM:     map[string]rmsource.RenderBlob{"page-1": {Hash: "h-page", Path: rmPath}},
			Renderable: true,
		},
	})

	rc, ct, err := s.RenderPage(context.Background(), "remarkable://doc-1", 0)
	if err != nil {
		t.Fatalf("RenderPage: %v", err)
	}
	defer rc.Close()
	if ct != "image/jpeg" {
		t.Fatalf("content type = %q, want image/jpeg", ct)
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read render: %v", err)
	}
	if len(data) < 2 || !strings.HasPrefix(string(data[:2]), "\xff\xd8") {
		t.Fatalf("render did not produce a JPEG, len=%d", len(data))
	}
}

func writeMinimalRM(t *testing.T) string {
	t.Helper()
	header := []byte("reMarkable .lines file, version=6")
	for len(header) < 43 {
		header = append(header, ' ')
	}
	path := t.TempDir() + "/page.rm"
	if err := os.WriteFile(path, header, 0o644); err != nil {
		t.Fatalf("write rm: %v", err)
	}
	return path
}

func entryNames(entries []RemarkableEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}
