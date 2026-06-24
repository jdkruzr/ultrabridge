package search

import (
	"context"
	"testing"

	"github.com/sysop/ultrabridge/internal/notedb"
)

func openTestIndex(t *testing.T) *Store {
	t.Helper()
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db)
}

// AC6.1 + AC6.2: Indexed content is retrievable; result has path, page, snippet
func TestSearch_IndexAndQuery(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()

	if err := idx.Index(ctx, NoteDocument{Path: "/note1.note", Page: 0, BodyText: "hello world"}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	results, err := idx.Search(ctx, SearchQuery{Text: "hello"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	if results[0].Path != "/note1.note" {
		t.Errorf("path = %q, want /note1.note", results[0].Path)
	}
	if results[0].Snippet == "" {
		t.Error("expected non-empty snippet")
	}
}

// GetContentByPrefix returns only the rows matching the LIKE pattern, keyed by
// path, and excludes unrelated paths.
func TestGetContentByPrefix(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()
	nb := "forestnote://NB1/"
	docs := []NoteDocument{
		{Path: nb + "PG1", Page: 0, BodyText: "alpha", Source: "forestnote"},
		{Path: nb + "PG2", Page: 0, BodyText: "beta", Source: "forestnote"},
		{Path: "forestnote://NB2/PG9", Page: 0, BodyText: "other notebook"},
		{Path: "/supernote/foo.note", Page: 0, BodyText: "unrelated"},
	}
	for _, d := range docs {
		if err := idx.Index(ctx, d); err != nil {
			t.Fatalf("Index %s: %v", d.Path, err)
		}
	}
	got, err := idx.GetContentByPrefix(ctx, nb+"%")
	if err != nil {
		t.Fatalf("GetContentByPrefix: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 docs, got %d: %v", len(got), got)
	}
	if got[nb+"PG1"].BodyText != "alpha" || got[nb+"PG2"].BodyText != "beta" {
		t.Errorf("unexpected bodies: %+v", got)
	}
	if _, ok := got["forestnote://NB2/PG9"]; ok {
		t.Error("NB2 page leaked into NB1 prefix results")
	}
}

func TestIndexPageAndGetContentPageOrdering(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()

	if err := idx.IndexPage(ctx, "/ordered.note", 2, "api", "third page", "", ""); err != nil {
		t.Fatalf("IndexPage page 2: %v", err)
	}
	if err := idx.IndexPage(ctx, "/ordered.note", 0, "api", "first page", "Title", "tag"); err != nil {
		t.Fatalf("IndexPage page 0: %v", err)
	}
	if err := idx.IndexPage(ctx, "/ordered.note", 1, "api", "second page", "", ""); err != nil {
		t.Fatalf("IndexPage page 1: %v", err)
	}

	docs, err := idx.GetContent(ctx, "/ordered.note")
	if err != nil {
		t.Fatalf("GetContent: %v", err)
	}
	if len(docs) != 3 {
		t.Fatalf("GetContent len = %d, want 3", len(docs))
	}
	for i, doc := range docs {
		if doc.Page != i {
			t.Fatalf("docs[%d].Page = %d, want %d; docs=%+v", i, doc.Page, i, docs)
		}
	}
	if docs[0].TitleText != "Title" || docs[0].Keywords != "tag" || docs[0].Source != "api" {
		t.Fatalf("page 0 metadata not preserved: %+v", docs[0])
	}
}

func TestListFoldersAndFolderFilter(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()
	docs := []NoteDocument{
		{Path: "/notes/work/alpha.note", BodyText: "quarterly planning"},
		{Path: "/notes/personal/beta.note", BodyText: "quarterly home"},
		{Path: "/loose.note", BodyText: "quarterly loose"},
	}
	for _, d := range docs {
		if err := idx.Index(ctx, d); err != nil {
			t.Fatalf("Index %s: %v", d.Path, err)
		}
	}

	folders, err := idx.ListFolders(ctx)
	if err != nil {
		t.Fatalf("ListFolders: %v", err)
	}
	if len(folders) != 2 || folders[0] != "personal" || folders[1] != "work" {
		t.Fatalf("folders = %v, want [personal work]", folders)
	}

	results, err := idx.Search(ctx, SearchQuery{Text: "quarterly", Folder: "work"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Path != "/notes/work/alpha.note" {
		t.Fatalf("folder-filtered results = %+v, want only work note", results)
	}
}

func TestSearchQuotedInputIsEscaped(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()
	if err := idx.Index(ctx, NoteDocument{Path: "/quotes.note", BodyText: `he said "exact phrase" aloud`}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	results, err := idx.Search(ctx, SearchQuery{Text: `"exact phrase"`})
	if err != nil {
		t.Fatalf("Search quoted input should not produce FTS syntax error: %v", err)
	}
	if len(results) != 1 || results[0].Path != "/quotes.note" {
		t.Fatalf("quoted search results = %+v, want quotes.note", results)
	}
}

// Rename repoints FTS entries to the new path; the old path stops matching and
// the new path matches.
func TestSearch_Rename(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()
	if err := idx.Index(ctx, NoteDocument{Path: "/old.note", Page: 0, BodyText: "kumquat marmalade"}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if err := idx.Rename(ctx, "/old.note", "/new.note"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	res, _ := idx.Search(ctx, SearchQuery{Text: "kumquat"})
	if len(res) != 1 || res[0].Path != "/new.note" {
		t.Fatalf("after rename, results = %v, want one hit at /new.note", res)
	}
}

// Copy duplicates index entries so both src and dst are searchable.
func TestSearch_Copy(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()
	if err := idx.Index(ctx, NoteDocument{Path: "/src.note", Page: 0, BodyText: "rhubarb compote"}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if err := idx.Copy(ctx, "/src.note", "/dst.note"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	res, _ := idx.Search(ctx, SearchQuery{Text: "rhubarb"})
	paths := map[string]bool{}
	for _, r := range res {
		paths[r.Path] = true
	}
	if !paths["/src.note"] || !paths["/dst.note"] {
		t.Fatalf("after copy, want both /src.note and /dst.note searchable, got %v", paths)
	}
}

// AC6.3: Results ordered by relevance
func TestSearch_Ordering(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()

	idx.Index(ctx, NoteDocument{Path: "/low.note", Page: 0, BodyText: "hello once"})
	idx.Index(ctx, NoteDocument{Path: "/high.note", Page: 0, BodyText: "hello hello hello hello"})

	results, err := idx.Search(ctx, SearchQuery{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Path != "/high.note" {
		t.Errorf("expected high.note ranked first, got %s", results[0].Path)
	}
}

// AC6.4: Re-indexing same path+page replaces content
func TestSearch_Reindex(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()

	idx.Index(ctx, NoteDocument{Path: "/note.note", Page: 0, BodyText: "old text"})
	idx.Index(ctx, NoteDocument{Path: "/note.note", Page: 0, BodyText: "new text"})

	newResults, _ := idx.Search(ctx, SearchQuery{Text: "new"})
	if len(newResults) == 0 {
		t.Error("expected to find 'new text' after re-index")
	}
	oldResults, _ := idx.Search(ctx, SearchQuery{Text: "old"})
	if len(oldResults) != 0 {
		t.Error("expected 'old text' to be replaced and not findable")
	}
}

// AC6.5: Empty query returns empty results, not an error
func TestSearch_EmptyQuery(t *testing.T) {
	idx := openTestIndex(t)
	results, err := idx.Search(context.Background(), SearchQuery{Text: ""})
	if err != nil {
		t.Errorf("empty query returned error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("empty query returned %d results, want 0", len(results))
	}
}

func TestSearch_Delete(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()
	idx.Index(ctx, NoteDocument{Path: "/del.note", Page: 0, BodyText: "deleteme"})
	idx.Delete(ctx, "/del.note")
	results, _ := idx.Search(ctx, SearchQuery{Text: "deleteme"})
	if len(results) != 0 {
		t.Errorf("expected 0 results after delete, got %d", len(results))
	}
}

// boox-notes-pipeline.AC6.1: Search returns results from both Supernote and Boox notes
func TestSearch_ReturnsMultipleSources(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()

	// Index content from Supernote (no path prefix) with source="myScript"
	supernoteDoc := NoteDocument{
		Path:     "/notes/supernote.note",
		Page:     0,
		BodyText: "shared content here",
		Source:   "myScript",
	}
	if err := idx.Index(ctx, supernoteDoc); err != nil {
		t.Fatalf("Index supernote: %v", err)
	}

	// Index content from Boox (with /boox prefix) with source="api"
	booxDoc := NoteDocument{
		Path:     "/boox/notes/boox-note.note",
		Page:     0,
		BodyText: "shared content here",
		Source:   "api",
	}
	if err := idx.Index(ctx, booxDoc); err != nil {
		t.Fatalf("Index boox: %v", err)
	}

	// Search for term present in both
	results, err := idx.Search(ctx, SearchQuery{Text: "shared"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(results) < 2 {
		t.Errorf("expected at least 2 results from both sources, got %d", len(results))
	}

	// Verify both paths are present
	paths := make(map[string]bool)
	for _, r := range results {
		paths[r.Path] = true
	}
	if !paths["/notes/supernote.note"] {
		t.Error("supernote result not found")
	}
	if !paths["/boox/notes/boox-note.note"] {
		t.Error("boox result not found")
	}
}

// boox-notes-pipeline.AC6.3: BM25 scoring is unaffected by device source (ranking is consistent)
// Search returns results from both sources with BM25 scoring that doesn't favor one source over another
func TestSearch_BM25ConsistentAcrossSources(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()

	// Index pages from both sources with different relevance levels
	// Higher relevance: term appears more frequently
	booxHighRelevance := NoteDocument{
		Path:     "/boox/notes/boox-note.note",
		Page:     0,
		BodyText: "hello hello hello hello world world",
		Source:   "api",
	}
	if err := idx.Index(ctx, booxHighRelevance); err != nil {
		t.Fatalf("Index boox high: %v", err)
	}

	// Lower relevance: term appears once
	supernoteLowRelevance := NoteDocument{
		Path:     "/notes/supernote.note",
		Page:     0,
		BodyText: "hello world",
		Source:   "myScript",
	}
	if err := idx.Index(ctx, supernoteLowRelevance); err != nil {
		t.Fatalf("Index supernote low: %v", err)
	}

	// Search for the term
	results, err := idx.Search(ctx, SearchQuery{Text: "hello"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(results) < 2 {
		t.Errorf("expected at least 2 results, got %d", len(results))
		return
	}

	// Verify ranking: the Boox document with higher frequency should rank first
	// This proves BM25 scoring is path-agnostic and only considers content relevance
	if results[0].Path != "/boox/notes/boox-note.note" {
		t.Errorf("expected boox-note (higher relevance) ranked first, got %s", results[0].Path)
	}
	if results[1].Path != "/notes/supernote.note" {
		t.Errorf("expected supernote-note (lower relevance) ranked second, got %s", results[1].Path)
	}
}
