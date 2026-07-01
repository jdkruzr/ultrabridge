package syncstore

import (
	"context"
	"testing"
)

// Test ULIDs (26-char uppercase Crockford; no I/L/O/U).
const (
	fdrA = "0000000000000000000000FDRA"
	fdrB = "0000000000000000000000FDRB"
	nbA  = "00000000000000000000000NBA"
	nbB  = "00000000000000000000000NBB"
	pgA  = "00000000000000000000000PGA"
	pgB  = "00000000000000000000000PGC"
)

func folderOp(seq, wall int64, pk, name, parent string, deletedAt any) Op {
	var p any
	if parent != "" {
		p = parent
	}
	return Op{
		Table: "folder", PK: pk, SiteID: siteA, OpSeq: seq, WallTS: wall,
		Cols: map[string]any{
			"name": name, "sort_order": float64(0), "created_at": float64(1000),
			"deleted_at": deletedAt, "parent_folder_id": p,
		},
	}
}

func nbInFolder(seq, wall int64, pk, name, folder string, deletedAt any) Op {
	var f any
	if folder != "" {
		f = folder
	}
	return Op{
		Table: "notebook", PK: pk, SiteID: siteA, OpSeq: seq, WallTS: wall,
		Cols: map[string]any{
			"name": name, "sort_order": float64(0), "created_at": float64(1000),
			"deleted_at": deletedAt, "folder_id": f, "aspect_long_axis": nil,
		},
	}
}

func pageOp(seq, wall int64, pk, notebookID string, sortOrder int, deletedAt any) Op {
	return Op{
		Table: "page", PK: pk, SiteID: siteA, OpSeq: seq, WallTS: wall,
		Cols: map[string]any{
			"notebook_id": notebookID, "sort_order": float64(sortOrder), "created_at": float64(1000),
			"deleted_at": deletedAt, "template": nil, "template_pitch_mm": nil,
		},
	}
}

func TestListFolders_TreeShapeAndSoftDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.ApplyBatch(ctx, siteA, []Op{
		folderOp(1, 1000, fdrA, "Parent", "", nil),
		folderOp(2, 1010, fdrB, "Child", fdrA, nil),
		folderOp(3, 1020, "0000000000000000000000FDRD", "Gone", "", float64(1020)), // soft-deleted
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	folders, err := s.ListFolders(ctx)
	if err != nil {
		t.Fatalf("list folders: %v", err)
	}
	if len(folders) != 2 {
		t.Fatalf("want 2 live folders, got %d", len(folders))
	}
	byID := map[string]FolderRow{}
	for _, f := range folders {
		byID[f.ID] = f
	}
	if byID[fdrB].ParentFolderID != fdrA {
		t.Errorf("child parent = %q, want %q", byID[fdrB].ParentFolderID, fdrA)
	}
	if byID[fdrA].ParentFolderID != "" {
		t.Errorf("root parent = %q, want empty", byID[fdrA].ParentFolderID)
	}
}

func TestListNotebooks_PageCountAndUnfiled(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.ApplyBatch(ctx, siteA, []Op{
		folderOp(1, 1000, fdrA, "Folder", "", nil),
		nbInFolder(2, 1010, nbA, "Filed", fdrA, nil),
		nbInFolder(3, 1020, nbB, "Unfiled", "", nil),
		pageOp(4, 1030, pgA, nbA, 0, nil),
		pageOp(5, 1040, pgB, nbA, 1, nil),
		pageOp(6, 1050, "00000000000000000000000PGD", nbA, 2, float64(1050)), // soft-deleted page
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	nbs, err := s.ListNotebooks(ctx)
	if err != nil {
		t.Fatalf("list notebooks: %v", err)
	}
	byID := map[string]NotebookRow{}
	for _, n := range nbs {
		byID[n.ID] = n
	}
	if byID[nbA].FolderID != fdrA {
		t.Errorf("filed notebook folder = %q, want %q", byID[nbA].FolderID, fdrA)
	}
	if byID[nbA].PageCount != 2 {
		t.Errorf("page count = %d, want 2 (soft-deleted excluded)", byID[nbA].PageCount)
	}
	if byID[nbB].FolderID != "" {
		t.Errorf("unfiled notebook folder = %q, want empty", byID[nbB].FolderID)
	}
}

func TestNotebookPages_OrderAndSoftDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.ApplyBatch(ctx, siteA, []Op{
		nbInFolder(1, 1000, nbA, "NB", "", nil),
		pageOp(2, 1010, pgB, nbA, 1, nil),                                    // sort_order 1
		pageOp(3, 1020, pgA, nbA, 0, nil),                                    // sort_order 0
		pageOp(4, 1030, "00000000000000000000000PGD", nbA, 2, float64(1030)), // deleted
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	pages, err := s.NotebookPages(ctx, nbA)
	if err != nil {
		t.Fatalf("notebook pages: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("want 2 live pages, got %d", len(pages))
	}
	if pages[0].ID != pgA || pages[1].ID != pgB {
		t.Errorf("page order = [%s, %s], want [%s, %s] (by sort_order)", pages[0].ID, pages[1].ID, pgA, pgB)
	}
}

// strokeOnPage builds a stroke op (valid base64 points) on the given page.
func strokeOnPage(seq, wall int64, pk, pageID string, deletedAt any) Op {
	return Op{
		Table: "stroke", PK: pk, SiteID: siteA, OpSeq: seq, WallTS: wall,
		Cols: map[string]any{
			"page_id": pageID, "color": float64(4278190080), "pen_width_min": float64(2),
			"pen_width_max": float64(6), "points": "MgAAADwAAADIAAAAAAAAAAEAAAA=",
			"z": float64(0), "created_at": float64(1000), "deleted_at": deletedAt,
		},
	}
}

func TestListNotebooks_CreatedAndDerivedModified(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// Notebook authored at wall 1000; a page at 2000; a stroke on it at 5000.
	// A second page that was later soft-deleted at 9000 must NOT raise Modified
	// (only LIVE pages/strokes feed the rollup).
	if _, err := s.ApplyBatch(ctx, siteA, []Op{
		nbInFolder(1, 1000, nbA, "NB", "", nil),
		pageOp(2, 2000, pgA, nbA, 0, nil),
		strokeOnPage(3, 5000, "00000000000000000000000ST1", pgA, nil),
		pageOp(4, 9000, pgB, nbA, 1, float64(9000)), // soft-deleted page (deleted op wall 9000)
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	nbs, err := s.ListNotebooks(ctx)
	if err != nil {
		t.Fatalf("list notebooks: %v", err)
	}
	if len(nbs) != 1 {
		t.Fatalf("want 1 notebook, got %d", len(nbs))
	}
	n := nbs[0]
	if n.CreatedAt != 1000 {
		t.Errorf("CreatedAt = %d, want 1000", n.CreatedAt)
	}
	// Modified = MAX(notebook 1000, live page 2000, live stroke 5000) = 5000.
	// The soft-deleted page's op (wall 9000) is excluded.
	if n.ModifiedAt != 5000 {
		t.Errorf("ModifiedAt = %d, want 5000 (deleted page excluded)", n.ModifiedAt)
	}
	if n.PageCount != 1 {
		t.Errorf("PageCount = %d, want 1", n.PageCount)
	}
}

func TestListFolderContents_DirectChildrenOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	gc := "0000000000000000000000FDGC" // grandchild folder under fdrB
	if _, err := s.ApplyBatch(ctx, siteA, []Op{
		folderOp(1, 1000, fdrA, "Parent", "", nil),
		folderOp(2, 1010, fdrB, "Child", fdrA, nil),
		folderOp(3, 1020, gc, "Grandchild", fdrB, nil),
		nbInFolder(4, 1030, nbA, "InParent", fdrA, nil),
		nbInFolder(5, 1040, nbB, "AtRoot", "", nil),
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Root: one folder (fdrA), one notebook (nbB).
	folders, nbs, err := s.ListFolderContents(ctx, "")
	if err != nil {
		t.Fatalf("root contents: %v", err)
	}
	if len(folders) != 1 || folders[0].ID != fdrA {
		t.Errorf("root folders = %+v, want [fdrA]", folders)
	}
	if len(nbs) != 1 || nbs[0].ID != nbB {
		t.Errorf("root notebooks = %+v, want [nbB]", nbs)
	}
	// Under fdrA: one subfolder (fdrB), one notebook (nbA). The grandchild folder
	// must NOT appear (it's under fdrB, not fdrA).
	folders, nbs, err = s.ListFolderContents(ctx, fdrA)
	if err != nil {
		t.Fatalf("fdrA contents: %v", err)
	}
	if len(folders) != 1 || folders[0].ID != fdrB {
		t.Errorf("fdrA folders = %+v, want [fdrB]", folders)
	}
	if len(nbs) != 1 || nbs[0].ID != nbA {
		t.Errorf("fdrA notebooks = %+v, want [nbA]", nbs)
	}
}

func TestFolderPath_RootToLeaf(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	gc := "0000000000000000000000FDGC"
	if _, err := s.ApplyBatch(ctx, siteA, []Op{
		folderOp(1, 1000, fdrA, "Parent", "", nil),
		folderOp(2, 1010, fdrB, "Child", fdrA, nil),
		folderOp(3, 1020, gc, "Grandchild", fdrB, nil),
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	chain, err := s.FolderPath(ctx, gc)
	if err != nil {
		t.Fatalf("folder path: %v", err)
	}
	if len(chain) != 3 || chain[0].ID != fdrA || chain[1].ID != fdrB || chain[2].ID != gc {
		t.Errorf("chain = %+v, want [fdrA, fdrB, gc]", chain)
	}
	// Root path is empty.
	if chain, err := s.FolderPath(ctx, ""); err != nil || chain != nil {
		t.Errorf("root path = %+v, %v; want nil, nil", chain, err)
	}
}

func TestSoftDeleteNotebook_DeindexSetAndLiveness(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.ApplyBatch(ctx, siteA, []Op{
		nbInFolder(1, 1000, nbA, "NB", "", nil),
		pageOp(2, 2000, pgA, nbA, 0, nil),
		pageOp(3, 2010, pgB, nbA, 1, nil),
		strokeOnPage(4, 2020, "00000000000000000000000ST1", pgA, nil),
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	pageIDs, err := s.SoftDeleteNotebook(ctx, nbA)
	if err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
	if len(pageIDs) != 2 {
		t.Fatalf("want 2 affected page IDs, got %d: %v", len(pageIDs), pageIDs)
	}
	// Notebook no longer listed; pages no longer live.
	nbs, err := s.ListNotebooks(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nbs) != 0 {
		t.Errorf("want 0 live notebooks after delete, got %d", len(nbs))
	}
	for _, pid := range []string{pgA, pgB} {
		if _, live, err := s.LivePage(ctx, pid); err != nil || live {
			t.Errorf("page %s live=%v err=%v, want live=false", pid, live, err)
		}
	}
	if strokes, err := s.LivePageStrokes(ctx, pgA); err != nil || len(strokes) != 0 {
		t.Errorf("page %s strokes = %d (err %v), want 0", pgA, len(strokes), err)
	}
}

func TestLiveNotebookPageIDs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.ApplyBatch(ctx, siteA, []Op{
		nbInFolder(1, 1000, nbA, "NB", "", nil),
		pageOp(2, 2000, pgA, nbA, 0, nil),
		pageOp(3, 2010, pgB, nbA, 1, nil),
		pageOp(4, 2020, "00000000000000000000000PGD", nbA, 2, float64(2020)), // deleted
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	ids, err := s.LiveNotebookPageIDs(ctx, nbA)
	if err != nil {
		t.Fatalf("live page ids: %v", err)
	}
	if len(ids) != 2 || ids[0] != pgA || ids[1] != pgB {
		t.Errorf("ids = %v, want [%s %s]", ids, pgA, pgB)
	}
}

func TestNotebookMeta_LiveOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.ApplyBatch(ctx, siteA, []Op{nbInFolder(1, 1000, nbA, "MyNotebook", fdrA, nil)}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	meta, err := s.NotebookMeta(ctx, nbA)
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	if meta.Name != "MyNotebook" || meta.FolderID != fdrA {
		t.Errorf("meta = %+v, want name=MyNotebook folder=%s", meta, fdrA)
	}
}
