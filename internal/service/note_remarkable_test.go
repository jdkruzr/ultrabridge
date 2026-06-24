package service

import (
	"context"
	"database/sql"
	"testing"

	rmsource "github.com/sysop/ultrabridge/internal/source/remarkable"
)

type fakeRMReader struct {
	docs []rmsource.Document
	err  error
}

func (f *fakeRMReader) ListDocuments(context.Context) ([]rmsource.Document, error) {
	return f.docs, f.err
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
	s := &noteService{}
	s.SetRemarkableReader(&fakeRMReader{docs: []rmsource.Document{
		{ID: "folder-1", Name: "Projects", Type: "folder", Parent: ""},
		{ID: "doc-b", Name: "Beta", Type: "document", Parent: "folder-1", PageCount: 8},
	}})

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
	if detail.RenderAvailable || detail.OCRAvailable {
		t.Fatalf("first structural chunk should not advertise render/OCR: %+v", detail)
	}

	if _, err := s.GetRemarkableDocumentDetail(context.Background(), "missing"); err != sql.ErrNoRows {
		t.Fatalf("missing detail err = %v, want sql.ErrNoRows", err)
	}
}

func entryNames(entries []RemarkableEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}
