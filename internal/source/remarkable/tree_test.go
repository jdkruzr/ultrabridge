package remarkable

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// seedBlob writes a blob keyed by an arbitrary id (we control the id, so it
// stands in for the device's content-addressed hash) and returns that id.
func seedBlob(t *testing.T, st *store, id, content string) string {
	t.Helper()
	if _, err := st.putBlob(context.Background(), id, strings.NewReader(content), 0); err != nil {
		t.Fatalf("seed blob %s: %v", id, err)
	}
	return id
}

// indexFile builds a v3 hashtree index file from (hash,type,entryName,subfiles,size) lines.
func indexFile(lines ...string) string {
	return "3\n" + strings.Join(lines, "\n") + "\n"
}

func entryLine(hash, typ, name string, subfiles, size int) string {
	return fmt.Sprintf("%s:%s:%s:%d:%d", hash, typ, name, subfiles, size)
}

func newTestStore(t *testing.T) *store {
	t.Helper()
	db := testDB(t)
	if err := migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := newStore(db, t.TempDir())
	if err := st.ensurePaths(); err != nil {
		t.Fatalf("ensurePaths: %v", err)
	}
	return st
}

// TestListDocumentTree_HashTree walks a synthetic modern (sync v3) blob tree:
// root -> top index -> per-document sub-index -> .metadata/.content blobs.
// It must surface folders and documents with names, parent links, types, and
// page counts, and must drop documents whose metadata marks them deleted.
func TestListDocumentTree_HashTree(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// A folder.
	metaFolder := seedBlob(t, st, "h-meta-folder",
		`{"visibleName":"Notebooks","type":"CollectionType","parent":""}`)
	subFolder := seedBlob(t, st, "h-sub-folder",
		indexFile(entryLine(metaFolder, "0", "folder-1.metadata", 0, 50)))

	// A live document inside the folder, 5 pages.
	metaDoc := seedBlob(t, st, "h-meta-doc",
		`{"visibleName":"Project Plan","type":"DocumentType","parent":"folder-1"}`)
	contentDoc := seedBlob(t, st, "h-content-doc", `{"pageCount":5}`)
	subDoc := seedBlob(t, st, "h-sub-doc", indexFile(
		entryLine(metaDoc, "0", "doc-1.metadata", 0, 60),
		entryLine(contentDoc, "0", "doc-1.content", 0, 20),
	))

	// A deleted document (must be filtered out).
	metaDel := seedBlob(t, st, "h-meta-del",
		`{"visibleName":"Trash Me","type":"DocumentType","parent":"","deleted":true}`)
	subDel := seedBlob(t, st, "h-sub-del",
		indexFile(entryLine(metaDel, "0", "doc-2.metadata", 0, 40)))

	// Top index references each document's sub-index.
	top := seedBlob(t, st, "h-top", indexFile(
		entryLine(subFolder, "80000000", "folder-1", 1, 0),
		entryLine(subDoc, "80000000", "doc-1", 2, 0),
		entryLine(subDel, "80000000", "doc-2", 1, 0),
	))

	// Root blob content is the hash of the top index.
	seedBlob(t, st, rootBlobID, top)

	docs, err := st.listDocumentTree(ctx)
	if err != nil {
		t.Fatalf("listDocumentTree: %v", err)
	}

	byID := map[string]Document{}
	for _, d := range docs {
		byID[d.ID] = d
	}
	if _, gone := byID["doc-2"]; gone {
		t.Errorf("deleted document doc-2 should be filtered out, got %+v", byID["doc-2"])
	}
	if len(docs) != 2 {
		t.Fatalf("got %d documents, want 2: %+v", len(docs), docs)
	}

	folder := byID["folder-1"]
	if folder.Name != "Notebooks" || folder.Type != "folder" || folder.Parent != "" {
		t.Errorf("folder-1 = %+v, want Notebooks/folder/parent=''", folder)
	}
	doc := byID["doc-1"]
	if doc.Name != "Project Plan" || doc.Type != "document" || doc.Parent != "folder-1" {
		t.Errorf("doc-1 = %+v, want Project Plan/document/parent=folder-1", doc)
	}
	if doc.PageCount != 5 {
		t.Errorf("doc-1 page count = %d, want 5", doc.PageCount)
	}
}

// TestListDocumentTree_LegacyFallback: with no root blob, the walker falls back
// to the remarkable_documents table populated by the legacy document-storage v2
// path (handleUpdateStatus -> upsertMetadata).
func TestListDocumentTree_LegacyFallback(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.upsertMetadata(ctx, documentMeta{
		ID:          "legacy-1",
		Version:     2,
		Type:        "DocumentType",
		VisibleName: "Old Notebook",
		Parent:      "",
	}); err != nil {
		t.Fatalf("upsertMetadata: %v", err)
	}

	docs, err := st.listDocumentTree(ctx)
	if err != nil {
		t.Fatalf("listDocumentTree: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d documents, want 1: %+v", len(docs), docs)
	}
	if docs[0].ID != "legacy-1" || docs[0].Name != "Old Notebook" || docs[0].Type != "document" {
		t.Errorf("legacy doc = %+v, want legacy-1/Old Notebook/document", docs[0])
	}
}
