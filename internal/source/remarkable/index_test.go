package remarkable

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/search"
)

type recordingMetadataIndex struct {
	calls    []indexCall
	deleted  []string
	existing map[string]search.NoteDocument
	err      error
}

type indexCall struct {
	path      string
	page      int
	source    string
	bodyText  string
	titleText string
	keywords  string
}

func (r *recordingMetadataIndex) IndexPage(_ context.Context, path string, page int, source, bodyText, titleText, keywords string) error {
	if r.err != nil {
		return r.err
	}
	r.calls = append(r.calls, indexCall{
		path:      path,
		page:      page,
		source:    source,
		bodyText:  bodyText,
		titleText: titleText,
		keywords:  keywords,
	})
	return nil
}

func (r *recordingMetadataIndex) Delete(_ context.Context, path string) error {
	r.deleted = append(r.deleted, path)
	return nil
}

func (r *recordingMetadataIndex) GetContentByPrefix(_ context.Context, _ string) (map[string]search.NoteDocument, error) {
	return r.existing, nil
}

func TestMetadataIndexer_IndexesDocumentNamesAndFolderContext(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := newStore(db, t.TempDir())
	if err := st.ensurePaths(); err != nil {
		t.Fatalf("ensurePaths: %v", err)
	}
	if err := st.upsertMetadata(ctx, documentMeta{ID: "folder-1", Type: "CollectionType", VisibleName: "Projects"}); err != nil {
		t.Fatalf("upsert folder: %v", err)
	}
	if err := st.upsertMetadata(ctx, documentMeta{ID: "doc-1", Type: "DocumentType", VisibleName: "Alpha Plan", Parent: "folder-1"}); err != nil {
		t.Fatalf("upsert doc: %v", err)
	}
	if err := st.upsertMetadata(ctx, documentMeta{ID: "folder-2", Type: "CollectionType", VisibleName: "Archive"}); err != nil {
		t.Fatalf("upsert second folder: %v", err)
	}

	idx := &recordingMetadataIndex{}
	mi := newMetadataIndexer(st, idx, nil)
	if err := mi.indexAll(ctx); err != nil {
		t.Fatalf("indexAll: %v", err)
	}

	if len(idx.calls) != 1 {
		t.Fatalf("index calls = %d, want 1 document-only call: %+v", len(idx.calls), idx.calls)
	}
	got := idx.calls[0]
	if got.path != "remarkable://doc-1" || got.page != metadataPage || got.source != "remarkable" {
		t.Fatalf("indexed identity = %+v", got)
	}
	if got.titleText != "Alpha Plan" {
		t.Fatalf("titleText = %q, want Alpha Plan", got.titleText)
	}
	for _, want := range []string{"Alpha Plan", "Folder: Projects"} {
		if !strings.Contains(got.bodyText, want) {
			t.Fatalf("bodyText = %q, want %q", got.bodyText, want)
		}
	}
}

func TestMetadataIndexer_PrunesStaleRemarkablePaths(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := newStore(db, t.TempDir())
	if err := st.ensurePaths(); err != nil {
		t.Fatalf("ensurePaths: %v", err)
	}
	if err := st.upsertMetadata(ctx, documentMeta{ID: "doc-live", Type: "DocumentType", VisibleName: "Live"}); err != nil {
		t.Fatalf("upsert doc: %v", err)
	}

	idx := &recordingMetadataIndex{existing: map[string]search.NoteDocument{
		"remarkable://doc-live":  {Path: "remarkable://doc-live"},
		"remarkable://doc-stale": {Path: "remarkable://doc-stale"},
	}}
	mi := newMetadataIndexer(st, idx, nil)
	if err := mi.indexAll(ctx); err != nil {
		t.Fatalf("indexAll: %v", err)
	}
	if len(idx.deleted) != 1 || idx.deleted[0] != "remarkable://doc-stale" {
		t.Fatalf("deleted = %+v, want stale remarkable path only", idx.deleted)
	}
}

func TestMetadataIndexer_DeleteDocumentUsesRemarkablePath(t *testing.T) {
	idx := &recordingMetadataIndex{}
	mi := newMetadataIndexer(&store{}, idx, nil)
	if err := mi.deleteDocument(context.Background(), "doc-1"); err != nil {
		t.Fatalf("deleteDocument: %v", err)
	}
	if len(idx.deleted) != 1 || idx.deleted[0] != "remarkable://doc-1" {
		t.Fatalf("deleted = %+v", idx.deleted)
	}
}

func TestMetadataIndexer_PropagatesIndexErrors(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := newStore(db, t.TempDir())
	if err := st.ensurePaths(); err != nil {
		t.Fatalf("ensurePaths: %v", err)
	}
	if err := st.upsertMetadata(ctx, documentMeta{ID: "doc-1", Type: "DocumentType", VisibleName: "Alpha"}); err != nil {
		t.Fatalf("upsert doc: %v", err)
	}
	want := errors.New("index failed")
	idx := &recordingMetadataIndex{err: want}
	if err := newMetadataIndexer(st, idx, nil).indexAll(ctx); !errors.Is(err, want) {
		t.Fatalf("indexAll error = %v, want %v", err, want)
	}
}
