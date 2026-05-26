package service

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"testing"

	"github.com/sysop/ultrabridge/internal/booxpipeline"
	"github.com/sysop/ultrabridge/internal/notestore"
	"github.com/sysop/ultrabridge/internal/processor"
)

type recordingEmbedDeleter struct {
	paths []string
	err   error
}

func (r *recordingEmbedDeleter) Delete(_ context.Context, path string) error {
	r.paths = append(r.paths, path)
	return r.err
}

// Deleting a Boox note also drops its RAG embeddings (booxStore.DeleteNote
// already clears FTS content). The embed deleter is called with the note path.
func TestNoteService_DeleteNote_BooxDeindexesEmbeddings(t *testing.T) {
	embed := &recordingEmbedDeleter{}
	s := &noteService{
		booxStore:     &mockBooxStore{},
		booxNotesPath: "/boox",
		embedDeleter:  embed,
		logger:        slog.Default(),
	}
	const path = "/boox/notebook.note"
	if err := s.DeleteNote(context.Background(), path); err != nil {
		t.Fatalf("DeleteNote: %v", err)
	}
	if len(embed.paths) != 1 || embed.paths[0] != path {
		t.Fatalf("embed deleter paths = %v, want [%q]", embed.paths, path)
	}
}

// De-index is best-effort: an embed-delete error is logged, not propagated.
func TestNoteService_DeleteNote_BooxEmbedErrorIsBestEffort(t *testing.T) {
	s := &noteService{
		booxStore:     &mockBooxStore{},
		booxNotesPath: "/boox",
		embedDeleter:  &recordingEmbedDeleter{err: errors.New("boom")},
		logger:        slog.Default(),
	}
	if err := s.DeleteNote(context.Background(), "/boox/notebook.note"); err != nil {
		t.Fatalf("embed-delete error must not fail the delete: %v", err)
	}
}

type mockNoteStore struct {
	files []notestore.NoteFile
}

func (m *mockNoteStore) List(ctx context.Context, path string) ([]notestore.NoteFile, error) {
	return m.files, nil
}
func (m *mockNoteStore) Scan(ctx context.Context) ([]string, error)      { return nil, nil }
func (m *mockNoteStore) Get(ctx context.Context, path string) (*notestore.NoteFile, error) {
	for i := range m.files {
		if m.files[i].Path == path {
			return &m.files[i], nil
		}
	}
	return nil, nil
}
func (m *mockNoteStore) UpsertFile(ctx context.Context, path string) error { return nil }
func (m *mockNoteStore) SetHash(ctx context.Context, path, hash string) error { return nil }
func (m *mockNoteStore) GetHash(ctx context.Context, path string) (string, error) { return "", nil }
func (m *mockNoteStore) LookupByHash(ctx context.Context, hash string) (string, bool, error) {
	return "", false, nil
}
func (m *mockNoteStore) TransferJob(ctx context.Context, oldPath, newPath string) error { return nil }

type mockProcessor struct {
	enqueued []string
	status   processor.ProcessorStatus
}

func (m *mockProcessor) Start(ctx context.Context) error { return nil }
func (m *mockProcessor) Stop() error                    { return nil }
func (m *mockProcessor) Status() processor.ProcessorStatus { return m.status }
func (m *mockProcessor) Enqueue(ctx context.Context, path string, opts ...processor.EnqueueOption) error {
	m.enqueued = append(m.enqueued, path)
	return nil
}
func (m *mockProcessor) Skip(ctx context.Context, path, reason string) error { return nil }
func (m *mockProcessor) Unskip(ctx context.Context, path string) error      { return nil }
func (m *mockProcessor) GetJob(ctx context.Context, path string) (*processor.Job, error) {
	return nil, nil
}

type mockBooxStore struct {
	notes    []booxpipeline.BooxNoteEntry
	enqueued []string
}

func (m *mockBooxStore) ListNotes(ctx context.Context) ([]booxpipeline.BooxNoteEntry, error) {
	return m.notes, nil
}
func (m *mockBooxStore) GetVersions(ctx context.Context, path string) ([]booxpipeline.BooxVersion, error) {
	return nil, nil
}
func (m *mockBooxStore) GetNoteID(ctx context.Context, path string) (string, error) { return "", nil }
func (m *mockBooxStore) EnqueueJob(ctx context.Context, path string) error {
	m.enqueued = append(m.enqueued, path)
	return nil
}
func (m *mockBooxStore) GetLatestJob(ctx context.Context, path string) (*booxpipeline.BooxJob, error) {
	return nil, nil
}
func (m *mockBooxStore) RetryAllFailed(ctx context.Context) (int64, error) { return 0, nil }
func (m *mockBooxStore) DeleteNote(ctx context.Context, path string) error    { return nil }
func (m *mockBooxStore) SkipNote(ctx context.Context, path, reason string) error { return nil }
func (m *mockBooxStore) UnskipNote(ctx context.Context, path string) error       { return nil }
func (m *mockBooxStore) GetQueueStatus(ctx context.Context) (booxpipeline.QueueStatus, error) { return booxpipeline.QueueStatus{}, nil }
func (m *mockBooxStore) ReconcileCreatedAtFromFilename(ctx context.Context) (int64, error) { return 0, nil }
func (m *mockBooxStore) ListAutoNamedNotebooks(ctx context.Context) ([]booxpipeline.BooxNote, error) { return nil, nil }
func (m *mockBooxStore) MoveNote(ctx context.Context, oldPath, newPath, newFolder string) error { return nil }
func (m *mockBooxStore) ListFolders(ctx context.Context) ([]booxpipeline.FolderCount, error) {
	counts := map[string]int{}
	for _, bn := range m.notes {
		counts[bn.Folder]++
	}
	var out []booxpipeline.FolderCount
	for f, c := range counts {
		out = append(out, booxpipeline.FolderCount{Folder: f, Count: c})
	}
	return out, nil
}
func (m *mockBooxStore) ListDevices(ctx context.Context) ([]booxpipeline.DeviceCount, error) {
	counts := map[string]int{}
	for _, bn := range m.notes {
		if bn.DeviceModel == ".." {
			continue
		}
		counts[bn.DeviceModel]++
	}
	var out []booxpipeline.DeviceCount
	for d, c := range counts {
		out = append(out, booxpipeline.DeviceCount{DeviceModel: d, Count: c})
	}
	return out, nil
}

type mockFileScanner struct {
	scanned int
}

func (m *mockFileScanner) ScanNow(ctx context.Context) {
	m.scanned++
}

func TestNoteService_ListFiles(t *testing.T) {
	ns := &mockNoteStore{
		files: []notestore.NoteFile{
			{Name: "SN Note 1", Path: "/notes/sn1", FileType: notestore.FileTypeNote},
			{Name: "SN Note 2", Path: "/notes/sn2", FileType: notestore.FileTypeNote},
		},
	}
	bs := &mockBooxStore{
		notes: []booxpipeline.BooxNoteEntry{
			{Title: "Boox Note 1", Path: "/boox/bn1"},
		},
	}
	svc := NewNoteService(ns, nil, bs, nil, nil, nil, nil, nil, "", "", nil)

	files, total, err := svc.ListFiles(context.Background(), "", "name", "asc", 1, 10)
	if err != nil {
		t.Fatalf("ListFiles failed: %v", err)
	}

	if total != 3 {
		t.Errorf("expected 3 total files, got %d", total)
	}
	if len(files) != 3 {
		t.Errorf("expected 3 files in page, got %d", len(files))
	}

	// Verify merging and mapping
	names := map[string]bool{}
	for _, f := range files {
		names[f.Name] = true
	}
	if !names["SN Note 1"] || !names["SN Note 2"] || !names["Boox Note 1"] {
		t.Errorf("missing files in merged list: %v", names)
	}
}

func TestNoteService_ListSupernoteFiles(t *testing.T) {
	ns := &mockNoteStore{
		files: []notestore.NoteFile{
			{Name: "SN Note 1", Path: "/notes/sn1", FileType: notestore.FileTypeNote},
			{Name: "SN Note 2", Path: "/notes/sn2", FileType: notestore.FileTypeNote},
		},
	}
	bs := &mockBooxStore{
		notes: []booxpipeline.BooxNoteEntry{
			{Title: "Boox Note", Path: "/boox/bn1"},
		},
	}
	svc := NewNoteService(ns, nil, bs, nil, nil, nil, nil, nil, "", "", nil)

	files, total, err := svc.ListSupernoteFiles(context.Background(), "", "name", "asc", 1, 10)
	if err != nil {
		t.Fatalf("ListSupernoteFiles failed: %v", err)
	}
	if total != 2 {
		t.Errorf("expected 2 Supernote files, got %d (list must exclude Boox)", total)
	}
	for _, f := range files {
		if f.Source != "supernote" {
			t.Errorf("unexpected non-supernote file in supernote list: %+v", f)
		}
	}

	t.Run("no_store_returns_empty", func(t *testing.T) {
		noneSvc := NewNoteService(nil, nil, bs, nil, nil, nil, nil, nil, "", "", nil)
		got, n, err := noneSvc.ListSupernoteFiles(context.Background(), "", "", "", 1, 10)
		if err != nil || n != 0 || len(got) != 0 {
			t.Errorf("expected empty result when noteStore is nil, got (%v, %d, %v)", got, n, err)
		}
	})
}

func TestNoteService_ListBooxNotes(t *testing.T) {
	ns := &mockNoteStore{
		files: []notestore.NoteFile{
			{Name: "SN Note", Path: "/notes/sn1", FileType: notestore.FileTypeNote},
		},
	}
	bs := &mockBooxStore{
		notes: []booxpipeline.BooxNoteEntry{
			{
				Title:       "Project Notes",
				Path:        "/boox/bn1.note",
				NoteID:      "n1",
				DeviceModel: "NoteAir5C",
				NoteType:    "Notebooks",
				Folder:      "Personal",
				PageCount:   12,
				JobStatus:   "done",
			},
			{
				Title:     "Reading",
				Path:      "/boox/bn2.note",
				NoteID:    "n2",
				Folder:    "Books",
				PageCount: 3,
			},
		},
	}
	svc := NewNoteService(ns, nil, bs, nil, nil, nil, nil, nil, "", "", nil)

	rows, total, err := svc.ListBooxNotes(context.Background(), "", "", "title", "asc", 1, 10)
	if err != nil {
		t.Fatalf("ListBooxNotes failed: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("expected 2 Boox rows, got total=%d page=%d", total, len(rows))
	}
	// asc by title → "Project Notes" before "Reading"
	if rows[0].Title != "Project Notes" || rows[1].Title != "Reading" {
		t.Errorf("unexpected title order: %+v", []string{rows[0].Title, rows[1].Title})
	}
	// Boox-specific fields populated
	if rows[0].DeviceModel != "NoteAir5C" || rows[0].Folder != "Personal" ||
		rows[0].NoteType != "Notebooks" || rows[0].PageCount != 12 || rows[0].NoteID != "n1" ||
		rows[0].JobStatus != "done" {
		t.Errorf("unexpected Boox summary: %+v", rows[0])
	}
	if rows[0].Filename != "bn1.note" {
		t.Errorf("filename should be basename of path, got %q", rows[0].Filename)
	}

	t.Run("sort_by_folder_desc", func(t *testing.T) {
		rows, _, err := svc.ListBooxNotes(context.Background(), "", "", "folder", "desc", 1, 10)
		if err != nil {
			t.Fatalf("ListBooxNotes failed: %v", err)
		}
		// desc by folder → "Personal" before "Books"
		if rows[0].Folder != "Personal" || rows[1].Folder != "Books" {
			t.Errorf("unexpected folder order: %+v", []string{rows[0].Folder, rows[1].Folder})
		}
	})

	t.Run("no_store_returns_empty", func(t *testing.T) {
		noneSvc := NewNoteService(ns, nil, nil, nil, nil, nil, nil, nil, "", "", nil)
		got, n, err := noneSvc.ListBooxNotes(context.Background(), "", "", "", "", 1, 10)
		if err != nil || n != 0 || len(got) != 0 {
			t.Errorf("expected empty result when booxStore is nil, got (%v, %d, %v)", got, n, err)
		}
	})

	t.Run("compute_move_dest_path", func(t *testing.T) {
		root := "/data/boox"
		cases := []struct {
			old, folder, want string
			wantErr           bool
		}{
			// onyx-prefixed, move to existing folder
			{"/data/boox/onyx/Palma/Notebooks/Personal/foo.note", "Moffitt", "/data/boox/onyx/Palma/Notebooks/Moffitt/foo.note", false},
			// onyx-prefixed, move to unfiled
			{"/data/boox/onyx/Palma/Notebooks/Personal/foo.note", "", "/data/boox/onyx/Palma/Notebooks/foo.note", false},
			// legacy (no onyx), move to folder
			{"/data/boox/NoteMax/Notebooks/foo.pdf", "Moffitt", "/data/boox/NoteMax/Notebooks/Moffitt/foo.pdf", false},
			// already unfiled, moving to same → still computes to unfiled (no-op caller's responsibility)
			{"/data/boox/NoteMax/Notebooks/foo.pdf", "", "/data/boox/NoteMax/Notebooks/foo.pdf", false},
			// path outside root → error
			{"/elsewhere/foo.note", "Moffitt", "", true},
			// too short to re-parent
			{"/data/boox/onyx/foo.note", "Moffitt", "", true},
		}
		for _, c := range cases {
			got, err := computeBooxMoveDestPath(root, c.old, c.folder)
			if (err != nil) != c.wantErr {
				t.Errorf("%q→%q: err=%v wantErr=%v", c.old, c.folder, err, c.wantErr)
			}
			if got != c.want {
				t.Errorf("%q→%q: got %q want %q", c.old, c.folder, got, c.want)
			}
		}
	})

	t.Run("unfiled_sentinel_filters_to_empty_folder", func(t *testing.T) {
		bs := &mockBooxStore{
			notes: []booxpipeline.BooxNoteEntry{
				{Title: "Has folder", Path: "/boox/a.note", Folder: "Moffitt"},
				{Title: "No folder 1", Path: "/boox/b.note", Folder: ""},
				{Title: "No folder 2", Path: "/boox/c.note", Folder: ""},
			},
		}
		svc := NewNoteService(ns, nil, bs, nil, nil, nil, nil, nil, "", "", nil)

		// All (folder="") returns everything.
		_, total, _ := svc.ListBooxNotes(context.Background(), "", "", "title", "asc", 1, 10)
		if total != 3 {
			t.Errorf("All filter expected 3, got %d", total)
		}

		// Unfiled sentinel returns only the 2 empty-folder notes.
		rows, total, _ := svc.ListBooxNotes(context.Background(), "", FolderFilterUnfiled, "title", "asc", 1, 10)
		if total != 2 {
			t.Errorf("Unfiled filter expected 2, got %d", total)
		}
		for _, r := range rows {
			if r.Folder != "" {
				t.Errorf("Unfiled filter returned row with folder=%q", r.Folder)
			}
		}

		// Specific folder returns only matching rows.
		_, total, _ = svc.ListBooxNotes(context.Background(), "", "Moffitt", "title", "asc", 1, 10)
		if total != 1 {
			t.Errorf("Moffitt filter expected 1, got %d", total)
		}
	})
}

func TestNoteService_GetFile(t *testing.T) {
	ns := &mockNoteStore{
		files: []notestore.NoteFile{
			{Name: "sn1.note", Path: "/notes/sn1.note", FileType: notestore.FileTypeNote, JobStatus: "done"},
		},
	}
	bs := &mockBooxStore{
		notes: []booxpipeline.BooxNoteEntry{
			{Title: "bn1", Path: "/boox/bn1.note", DeviceModel: "Go103"},
		},
	}
	// booxNotesPath="/boox" routes paths under /boox to the Boox branch.
	svc := NewNoteService(ns, nil, bs, nil, nil, nil, nil, nil, "", "/boox", nil)

	t.Run("supernote", func(t *testing.T) {
		f, err := svc.GetFile(context.Background(), "/notes/sn1.note")
		if err != nil {
			t.Fatalf("GetFile(sn1) failed: %v", err)
		}
		if f.Name != "sn1.note" || f.Source != "supernote" || f.JobStatus != "done" {
			t.Errorf("unexpected Supernote file: %+v", f)
		}
	})

	t.Run("boox", func(t *testing.T) {
		f, err := svc.GetFile(context.Background(), "/boox/bn1.note")
		if err != nil {
			t.Fatalf("GetFile(bn1) failed: %v", err)
		}
		if f.Name != "bn1" || f.Source != "boox" {
			t.Errorf("unexpected Boox file: %+v", f)
		}
	})

	t.Run("not_found_supernote", func(t *testing.T) {
		_, err := svc.GetFile(context.Background(), "/notes/missing.note")
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("GetFile(missing) err=%v, want sql.ErrNoRows", err)
		}
	})

	t.Run("not_found_boox", func(t *testing.T) {
		_, err := svc.GetFile(context.Background(), "/boox/missing.note")
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("GetFile(missing boox) err=%v, want sql.ErrNoRows", err)
		}
	})
}

func TestNoteService_Enqueue(t *testing.T) {
	p := &mockProcessor{}
	bs := &mockBooxStore{}
	svc := NewNoteService(nil, p, bs, nil, nil, nil, nil, nil, "", "", nil)

	// Supernote path
	err := svc.Enqueue(context.Background(), "/notes/sn1", false)
	if err != nil {
		t.Fatalf("Enqueue SN failed: %v", err)
	}
	if len(p.enqueued) != 1 || p.enqueued[0] != "/notes/sn1" {
		t.Errorf("expected /notes/sn1 in SN processor, got %v", p.enqueued)
	}

	// Boox path
	err = svc.Enqueue(context.Background(), "/boox/bn1.note", false)
	if err != nil {
		t.Fatalf("Enqueue Boox failed: %v", err)
	}
	if len(bs.enqueued) != 1 || bs.enqueued[0] != "/boox/bn1.note" {
		t.Errorf("expected /boox/bn1.note in Boox store, got %v", bs.enqueued)
	}
}
