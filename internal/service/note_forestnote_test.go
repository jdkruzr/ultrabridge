package service

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"testing"

	"github.com/sysop/ultrabridge/internal/search"
	"github.com/sysop/ultrabridge/internal/syncbridge"
	"github.com/sysop/ultrabridge/internal/syncstore"
)

// fakeFNReader is a canned ForestNoteReader for testing the note service's
// ForestNote surfacing without a real syncstore.
type fakeFNReader struct {
	folders   []syncstore.FolderRow
	notebooks []syncstore.NotebookRow
	pages     map[string][]syncstore.PageRef
	strokes   map[string][]syncstore.StrokeData
	textBoxes map[string][]syncstore.TextBoxData
	meta      map[string]syncstore.NotebookRow
	live      map[string]bool // page id → live; absent ⇒ not live (missing/deleted)
	contents  map[string]struct {
		f []syncstore.FolderRow
		n []syncstore.NotebookRow
	} // folderID → direct children
	folderPaths map[string][]syncstore.FolderRow // folderID → ancestor chain
	deletePages map[string][]string              // notebookID → page IDs to return from SoftDeleteNotebook
	deleted     []string                         // notebookIDs passed to SoftDeleteNotebook
	textRefs    map[string][]syncstore.TextBoxRef
}

func (f *fakeFNReader) ListFolderContents(_ context.Context, id string) ([]syncstore.FolderRow, []syncstore.NotebookRow, error) {
	c := f.contents[id]
	return c.f, c.n, nil
}
func (f *fakeFNReader) FolderPath(_ context.Context, id string) ([]syncstore.FolderRow, error) {
	return f.folderPaths[id], nil
}
func (f *fakeFNReader) SoftDeleteNotebook(_ context.Context, nb string) ([]string, error) {
	f.deleted = append(f.deleted, nb)
	return f.deletePages[nb], nil
}
func (f *fakeFNReader) ListNotebookTextBoxes(_ context.Context, nb string) ([]syncstore.TextBoxRef, error) {
	return f.textRefs[nb], nil
}
func (f *fakeFNReader) LiveNotebookPageIDs(_ context.Context, nb string) ([]string, error) {
	var ids []string
	for _, p := range f.pages[nb] {
		ids = append(ids, p.ID)
	}
	return ids, nil
}

// fakeSearchIndex satisfies search.SearchIndex; only GetContentByPrefix and
// Delete carry behavior, the rest are no-ops.
type fakeSearchIndex struct {
	byPrefix map[string]search.NoteDocument // note_path → doc
	deleted  []string                       // paths passed to Delete
}

func (f *fakeSearchIndex) Index(context.Context, search.NoteDocument) error { return nil }
func (f *fakeSearchIndex) Search(context.Context, search.SearchQuery) ([]search.SearchResult, error) {
	return nil, nil
}
func (f *fakeSearchIndex) Delete(_ context.Context, path string) error {
	f.deleted = append(f.deleted, path)
	return nil
}
func (f *fakeSearchIndex) IndexPage(context.Context, string, int, string, string, string, string) error {
	return nil
}
func (f *fakeSearchIndex) GetContent(context.Context, string) ([]search.NoteDocument, error) {
	return nil, nil
}
func (f *fakeSearchIndex) GetContentByPrefix(_ context.Context, _ string) (map[string]search.NoteDocument, error) {
	return f.byPrefix, nil
}
func (f *fakeSearchIndex) ListFolders(context.Context) ([]string, error) { return nil, nil }

var _ search.SearchIndex = (*fakeSearchIndex)(nil)

func (f *fakeFNReader) ListFolders(context.Context) ([]syncstore.FolderRow, error) {
	return f.folders, nil
}
func (f *fakeFNReader) ListNotebooks(context.Context) ([]syncstore.NotebookRow, error) {
	return f.notebooks, nil
}
func (f *fakeFNReader) NotebookPages(_ context.Context, nb string) ([]syncstore.PageRef, error) {
	return f.pages[nb], nil
}
func (f *fakeFNReader) NotebookMeta(_ context.Context, nb string) (syncstore.NotebookRow, error) {
	return f.meta[nb], nil
}
func (f *fakeFNReader) LivePage(_ context.Context, pg string) (string, bool, error) {
	return "", f.live[pg], nil
}
func (f *fakeFNReader) LivePageStrokes(_ context.Context, pg string) ([]syncstore.StrokeData, error) {
	return f.strokes[pg], nil
}
func (f *fakeFNReader) LivePageTextBoxes(_ context.Context, pg string) ([]syncstore.TextBoxData, error) {
	return f.textBoxes[pg], nil
}

func twoPointStroke() syncstore.StrokeData {
	buf := make([]byte, 0, 40)
	for _, v := range []int32{10, 10, 1000, 0, 0, 80, 120, 1000, 0, 5} {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(v))
		buf = append(buf, b[:]...)
	}
	return syncstore.StrokeData{Color: -16777216, PenWidthMin: 2, PenWidthMax: 6, Points: buf, Z: 0}
}

func TestRenderForestNotePage_ReturnsJPEG(t *testing.T) {
	r := &fakeFNReader{
		strokes: map[string][]syncstore.StrokeData{
			"00000000000000000000000PGA": {twoPointStroke()},
		},
		live: map[string]bool{"00000000000000000000000PGA": true},
	}
	s := &noteService{fnReader: r}
	rc, ct, err := s.RenderPage(context.Background(), "forestnote://00000000000000000000000NBA/00000000000000000000000PGA", 0)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	defer rc.Close()
	if ct != "image/jpeg" {
		t.Errorf("content-type = %q, want image/jpeg", ct)
	}
	body, _ := io.ReadAll(rc)
	if len(body) == 0 {
		t.Error("empty image body")
	}
}

// A live page with zero strokes still renders (a blank page), so liveness — not
// stroke count — is what distinguishes "blank" from "gone".
func TestRenderForestNotePage_LiveBlankPageRenders(t *testing.T) {
	r := &fakeFNReader{live: map[string]bool{"pgBlank": true}}
	s := &noteService{fnReader: r}
	rc, ct, err := s.RenderPage(context.Background(), "forestnote://nb/pgBlank", 0)
	if err != nil {
		t.Fatalf("render blank: %v", err)
	}
	defer rc.Close()
	if ct != "image/jpeg" {
		t.Errorf("content-type = %q, want image/jpeg", ct)
	}
}

// A missing/soft-deleted page must error (→ 404 at the handler) rather than
// serve a blank 200 that a stale tab or search deep-link would cache.
func TestRenderForestNotePage_MissingPageErrors(t *testing.T) {
	r := &fakeFNReader{} // no live pages
	s := &noteService{fnReader: r}
	if _, _, err := s.RenderPage(context.Background(), "forestnote://nb/gonePage", 0); err == nil {
		t.Error("want error for missing/deleted page, got nil")
	}
}

func TestRenderForestNotePage_NilReader(t *testing.T) {
	s := &noteService{}
	if _, _, err := s.RenderPage(context.Background(), "forestnote://nb/pg", 0); err == nil {
		t.Error("want error when forestnote reader not wired, got nil")
	}
}

func TestListForestNotePages_BuildsPaths(t *testing.T) {
	r := &fakeFNReader{
		meta:  map[string]syncstore.NotebookRow{"nbA": {ID: "nbA", Name: "Journal"}},
		pages: map[string][]syncstore.PageRef{"nbA": {{ID: "pgA"}, {ID: "pgB"}}},
	}
	s := &noteService{fnReader: r}
	name, pages, err := s.ListForestNotePages(context.Background(), "nbA")
	if err != nil {
		t.Fatalf("list pages: %v", err)
	}
	if name != "Journal" {
		t.Errorf("name = %q, want Journal", name)
	}
	if len(pages) != 2 || pages[0].Path != "forestnote://nbA/pgA" || pages[1].Ordinal != 1 {
		t.Errorf("pages = %+v", pages)
	}
}

type fakeReprocessor struct {
	called []string
	err    error
	status syncbridge.Status // returned by Status(); zero value is fine when caller doesn't care
}

func (f *fakeReprocessor) ReprocessNotebook(_ context.Context, nb string) error {
	f.called = append(f.called, nb)
	return f.err
}

func (f *fakeReprocessor) EditTextBox(_ context.Context, boxID, _ string) error {
	f.called = append(f.called, "edit:"+boxID)
	return f.err
}

func (f *fakeReprocessor) Status() syncbridge.Status { return f.status }

func TestListForestNoteFolder_EntriesAndStatus(t *testing.T) {
	r := &fakeFNReader{
		contents: map[string]struct {
			f []syncstore.FolderRow
			n []syncstore.NotebookRow
		}{
			"": {
				f: []syncstore.FolderRow{{ID: "f1", Name: "Sub", CreatedAt: 100, ModifiedAt: 200}},
				n: []syncstore.NotebookRow{
					{ID: "nbFull", Name: "Full", PageCount: 2, CreatedAt: 10, ModifiedAt: 50},
					{ID: "nbPartial", Name: "Partial", PageCount: 2},
					{ID: "nbBlank", Name: "Blank", PageCount: 0},
				},
			},
		},
		folderPaths: map[string][]syncstore.FolderRow{},
	}
	si := &fakeSearchIndex{byPrefix: map[string]search.NoteDocument{
		"forestnote://nbFull/p1":    {Path: "forestnote://nbFull/p1", BodyText: "a"},
		"forestnote://nbFull/p2":    {Path: "forestnote://nbFull/p2", BodyText: "b"},
		"forestnote://nbPartial/p1": {Path: "forestnote://nbPartial/p1", BodyText: "only one"},
		"forestnote://nbPartial/p2": {Path: "forestnote://nbPartial/p2", BodyText: ""}, // empty → not counted
	}}
	s := &noteService{fnReader: r, searchIndex: si, logger: slog.Default()}

	crumbs, entries, err := s.ListForestNoteFolder(context.Background(), "", "name", "asc")
	if err != nil {
		t.Fatalf("list folder: %v", err)
	}
	if len(crumbs) != 0 {
		t.Errorf("root crumbs = %+v, want none", crumbs)
	}
	// Folder first, then notebooks by name asc: Sub(folder), Blank, Full, Partial.
	if len(entries) != 4 || !entries[0].IsFolder || entries[0].ID != "f1" {
		t.Fatalf("entries = %+v, want folder f1 first", entries)
	}
	byID := map[string]ForestNoteEntry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	if byID["nbFull"].Status != "indexed" {
		t.Errorf("nbFull status = %q, want indexed", byID["nbFull"].Status)
	}
	if byID["nbPartial"].Status != "partial" {
		t.Errorf("nbPartial status = %q, want partial", byID["nbPartial"].Status)
	}
	if byID["nbBlank"].Status != "blank" {
		t.Errorf("nbBlank status = %q, want blank", byID["nbBlank"].Status)
	}
	if byID["nbFull"].Path != "forestnote://nbFull" {
		t.Errorf("nbFull path = %q", byID["nbFull"].Path)
	}
}

func TestGetForestNoteNotebookDetail_JoinsOCRAndFolderPath(t *testing.T) {
	r := &fakeFNReader{
		meta:  map[string]syncstore.NotebookRow{"nbA": {ID: "nbA", Name: "Journal", FolderID: "f2", CreatedAt: 10, ModifiedAt: 99}},
		pages: map[string][]syncstore.PageRef{"nbA": {{ID: "pgA"}, {ID: "pgB"}}},
		folderPaths: map[string][]syncstore.FolderRow{
			"f2": {{ID: "f1", Name: "Parent"}, {ID: "f2", Name: "Child"}},
		},
	}
	si := &fakeSearchIndex{byPrefix: map[string]search.NoteDocument{
		"forestnote://nbA/pgA": {Path: "forestnote://nbA/pgA", BodyText: "hello", Source: "forestnote"},
	}}
	s := &noteService{fnReader: r, searchIndex: si, logger: slog.Default()}

	d, err := s.GetForestNoteNotebookDetail(context.Background(), "nbA")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if d.Name != "Journal" || d.CreatedAt != 10 || d.ModifiedAt != 99 || d.PageCount != 2 {
		t.Errorf("header = %+v", d)
	}
	if len(d.FolderPath) != 2 || d.FolderPath[0] != "Parent" || d.FolderPath[1] != "Child" {
		t.Errorf("folder path = %+v, want [Parent Child]", d.FolderPath)
	}
	if d.Pages[0].BodyText != "hello" || d.Pages[0].Source != "forestnote" {
		t.Errorf("page 0 OCR not joined: %+v", d.Pages[0])
	}
	if d.Pages[1].BodyText != "" {
		t.Errorf("page 1 should have no OCR text, got %q", d.Pages[1].BodyText)
	}
}

func TestDeleteForestNoteNotebook_SoftDeleteThenDeindex(t *testing.T) {
	r := &fakeFNReader{deletePages: map[string][]string{"nbA": {"pgA", "pgB"}}}
	si := &fakeSearchIndex{}
	emb := &fakeEmbedIndex{}
	s := &noteService{fnReader: r, searchIndex: si, embedIndex: emb, logger: slog.Default()}

	if err := s.DeleteForestNoteNotebook(context.Background(), "nbA"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(r.deleted) != 1 || r.deleted[0] != "nbA" {
		t.Errorf("soft-delete calls = %+v", r.deleted)
	}
	want := []string{"forestnote://nbA/pgA", "forestnote://nbA/pgB"}
	if len(si.deleted) != 2 || si.deleted[0] != want[0] || si.deleted[1] != want[1] {
		t.Errorf("search de-index = %+v, want %+v", si.deleted, want)
	}
	if len(emb.deleted) != 2 {
		t.Errorf("embedding de-index = %+v, want 2", emb.deleted)
	}
}

func TestReprocessForestNoteNotebook_DelegatesAndNilSafe(t *testing.T) {
	// Nil reprocessor → error, not panic.
	s := &noteService{logger: slog.Default()}
	if err := s.ReprocessForestNoteNotebook(context.Background(), "nbA"); err == nil {
		t.Error("want error when reprocessor not wired")
	}
	// Wired → delegates.
	rp := &fakeReprocessor{}
	s.SetForestNoteReprocessor(rp)
	if err := s.ReprocessForestNoteNotebook(context.Background(), "nbA"); err != nil {
		t.Fatalf("reprocess: %v", err)
	}
	if len(rp.called) != 1 || rp.called[0] != "nbA" {
		t.Errorf("reprocessor calls = %+v", rp.called)
	}
}

func TestForestNoteTextBoxServiceDelegates(t *testing.T) {
	t.Run("list requires reader and returns refs", func(t *testing.T) {
		s := &noteService{logger: slog.Default()}
		if _, err := s.ListForestNoteTextBoxes(context.Background(), "nbA"); err == nil {
			t.Fatal("want error when reader is not wired")
		}
		r := &fakeFNReader{textRefs: map[string][]syncstore.TextBoxRef{
			"nbA": {{ID: "box1", PageID: "pgA", Text: "hello"}},
		}}
		s.SetForestNoteReader(r)
		got, err := s.ListForestNoteTextBoxes(context.Background(), "nbA")
		if err != nil {
			t.Fatalf("ListForestNoteTextBoxes: %v", err)
		}
		if len(got) != 1 || got[0].ID != "box1" || got[0].Text != "hello" {
			t.Fatalf("text boxes = %+v", got)
		}
	})

	t.Run("edit requires reprocessor and delegates", func(t *testing.T) {
		s := &noteService{logger: slog.Default()}
		if err := s.EditForestNoteTextBox(context.Background(), "box1", "new"); err == nil {
			t.Fatal("want error when reprocessor is not wired")
		}
		rp := &fakeReprocessor{}
		s.SetForestNoteReprocessor(rp)
		if err := s.EditForestNoteTextBox(context.Background(), "box1", "new"); err != nil {
			t.Fatalf("EditForestNoteTextBox: %v", err)
		}
		if len(rp.called) != 1 || rp.called[0] != "edit:box1" {
			t.Fatalf("edit calls = %+v", rp.called)
		}
	})
}

func TestExportForestNoteNotebookPDF(t *testing.T) {
	t.Run("renders live pages into a PDF with safe filename", func(t *testing.T) {
		r := &fakeFNReader{
			meta: map[string]syncstore.NotebookRow{
				"nbA": {ID: "nbA", Name: "Journal: Week/One"},
			},
			pages: map[string][]syncstore.PageRef{
				"nbA": {{ID: "pgA"}, {ID: "pgB"}},
			},
			live: map[string]bool{"pgA": true, "pgB": true},
		}
		s := &noteService{fnReader: r, logger: slog.Default()}
		rc, filename, err := s.ExportForestNoteNotebookPDF(context.Background(), "nbA")
		if err != nil {
			t.Fatalf("ExportForestNoteNotebookPDF: %v", err)
		}
		defer rc.Close()
		body, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read PDF: %v", err)
		}
		if filename != "Journal_ Week_One.pdf" {
			t.Fatalf("filename = %q", filename)
		}
		if len(body) < 5 || string(body[:5]) != "%PDF-" {
			t.Fatalf("export did not produce a PDF, first bytes=%q len=%d", body[:min(len(body), 8)], len(body))
		}
	})

	t.Run("empty notebook errors", func(t *testing.T) {
		r := &fakeFNReader{
			meta:  map[string]syncstore.NotebookRow{"nbA": {ID: "nbA", Name: "Empty"}},
			pages: map[string][]syncstore.PageRef{"nbA": nil},
		}
		s := &noteService{fnReader: r, logger: slog.Default()}
		if _, _, err := s.ExportForestNoteNotebookPDF(context.Background(), "nbA"); err == nil {
			t.Fatal("want error for empty notebook")
		}
	})
}

// fakeEmbedIndex tracks Delete calls.
type fakeEmbedIndex struct{ deleted []string }

func (f *fakeEmbedIndex) Delete(_ context.Context, path string) error {
	f.deleted = append(f.deleted, path)
	return nil
}
func (f *fakeEmbedIndex) Rename(context.Context, string, string) error { return nil }

func TestBuildForestNoteTree_NestingAndUnfiled(t *testing.T) {
	folders := []syncstore.FolderRow{
		{ID: "f1", Name: "Parent"},
		{ID: "f2", Name: "Child", ParentFolderID: "f1"},
	}
	notebooks := []syncstore.NotebookRow{
		{ID: "n1", Name: "InChild", FolderID: "f2", PageCount: 3},
		{ID: "n2", Name: "Loose"},                    // unfiled (no folder)
		{ID: "n3", Name: "Orphan", FolderID: "gone"}, // folder missing → unfiled
	}
	roots, unfiled := buildForestNoteTree(folders, notebooks)
	if len(roots) != 1 || roots[0].FolderID != "f1" {
		t.Fatalf("roots = %+v, want single f1 root", roots)
	}
	if len(roots[0].Children) != 1 || roots[0].Children[0].FolderID != "f2" {
		t.Fatalf("f1 children = %+v, want [f2]", roots[0].Children)
	}
	if len(roots[0].Children[0].Notebooks) != 1 || roots[0].Children[0].Notebooks[0].NotebookID != "n1" {
		t.Errorf("f2 notebooks = %+v, want [n1]", roots[0].Children[0].Notebooks)
	}
	if len(unfiled) != 2 {
		t.Errorf("unfiled = %+v, want 2 (loose + orphan)", unfiled)
	}
}

// TestGetProcessorStatus_PopulatesForestNoteWhenWired verifies that when a
// ForestNoteReprocessor is wired and its bridge has non-zero state (or even
// just a non-zero capacity), GetProcessorStatus surfaces a ForestNote block
// on the response. Mirrors the Boox pattern. Tested separately from the
// "no source wired" case because the JSON omitempty hides the field
// entirely when the source isn't live.
func TestGetProcessorStatus_PopulatesForestNoteWhenWired(t *testing.T) {
	t.Run("populated when bridge reports activity", func(t *testing.T) {
		s := &noteService{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		s.SetForestNoteReprocessor(&fakeReprocessor{
			status: syncbridge.Status{
				Pending:   7,
				InFlight:  1,
				Processed: 42,
				Dropped:   0,
				Capacity:  256,
			},
		})
		got, err := s.GetProcessorStatus(context.Background())
		if err != nil {
			t.Fatalf("GetProcessorStatus: %v", err)
		}
		if got.ForestNote == nil {
			t.Fatal("ForestNote block should be populated when bridge has activity")
		}
		if got.ForestNote.Pending != 7 || got.ForestNote.InFlight != 1 ||
			got.ForestNote.Processed != 42 || got.ForestNote.Capacity != 256 {
			t.Errorf("ForestNote: got %+v", got.ForestNote)
		}
	})

	t.Run("populated when bridge is fresh (Capacity>0, counters zero)", func(t *testing.T) {
		// A just-started bridge has Capacity=256 but every counter at zero —
		// the UI should still see the FN block so the operator knows the
		// source is wired (even if there's no work yet).
		s := &noteService{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		s.SetForestNoteReprocessor(&fakeReprocessor{
			status: syncbridge.Status{Capacity: 256},
		})
		got, _ := s.GetProcessorStatus(context.Background())
		if got.ForestNote == nil {
			t.Errorf("ForestNote block should be present when Capacity>0; got nil")
		}
	})

	t.Run("omitted when no reprocessor wired", func(t *testing.T) {
		s := &noteService{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		got, _ := s.GetProcessorStatus(context.Background())
		if got.ForestNote != nil {
			t.Errorf("ForestNote block should be nil when no source wired; got %+v", got.ForestNote)
		}
	})

	t.Run("omitted when bridge zero value (source not started)", func(t *testing.T) {
		// fakeReprocessor.status is the zero value here — mimics what happens
		// before the source's Start runs (bridge nil → Status() returns zero).
		s := &noteService{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		s.SetForestNoteReprocessor(&fakeReprocessor{})
		got, _ := s.GetProcessorStatus(context.Background())
		if got.ForestNote != nil {
			t.Errorf("ForestNote block should be nil when bridge is at zero value; got %+v", got.ForestNote)
		}
	})
}
