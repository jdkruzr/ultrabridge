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
			"deleted_at": deletedAt, "folder_id": f,
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
