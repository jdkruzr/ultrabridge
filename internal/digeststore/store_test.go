package digeststore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "digest.db")
	db, err := openTestDB(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return New(db)
}

func sampleItem() *Digest {
	return &Digest{
		UserID:           42,
		UniqueIdentifier: "uid-item-1",
		Content:          "the highlighted text",
		SourcePath:       "Note/example.note",
		SourceType:       2,
		MD5Hash:          "abc123",
		Metadata:         `{"note_page":"1"}`,
		CreationTime:     1000,
		LastModifiedTime: 1000,
		Author:           "Jane",
	}
}

func TestCreateAndGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	in := sampleItem()
	id, err := s.Create(ctx, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == 0 {
		t.Fatal("create returned id 0")
	}

	got, err := s.GetByID(ctx, 42, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.UniqueIdentifier != in.UniqueIdentifier || got.Content != in.Content ||
		got.SourceType != in.SourceType || got.MD5Hash != in.MD5Hash ||
		got.Metadata != in.Metadata || got.Author != in.Author ||
		got.CreationTime != in.CreationTime {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.IsGroup {
		t.Error("item should not be a group")
	}
	if got.CreatedAt == 0 || got.UpdatedAt == 0 {
		t.Error("created_at/updated_at should be stamped")
	}
}

func TestGetScopedByUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id, _ := s.Create(ctx, sampleItem())

	if _, err := s.GetByID(ctx, 999, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user get: want ErrNotFound, got %v", err)
	}
}

func TestGetByUniqueIdentifier(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, sampleItem()); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetByUniqueIdentifier(ctx, 42, "uid-item-1")
	if err != nil {
		t.Fatalf("get by uid: %v", err)
	}
	if got.Content != "the highlighted text" {
		t.Errorf("unexpected content %q", got.Content)
	}
	if _, err := s.GetByUniqueIdentifier(ctx, 42, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing uid: want ErrNotFound, got %v", err)
	}
}

func TestUpdate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id, _ := s.Create(ctx, sampleItem())

	upd := &Digest{ID: id, UserID: 42, Content: "edited", MD5Hash: "def456", LastModifiedTime: 2000}
	if err := s.Update(ctx, upd); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := s.GetByID(ctx, 42, id)
	if got.Content != "edited" || got.MD5Hash != "def456" || got.LastModifiedTime != 2000 {
		t.Fatalf("update not applied: %+v", got)
	}

	// Cross-user update must not touch the row.
	if err := s.Update(ctx, &Digest{ID: id, UserID: 7, Content: "hacked"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user update: want ErrNotFound, got %v", err)
	}
}

func TestSoftDeleteHidesRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id, _ := s.Create(ctx, sampleItem())

	if err := s.SoftDelete(ctx, 42, id); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := s.GetByID(ctx, 42, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted get: want ErrNotFound, got %v", err)
	}
	rows, total, err := s.List(ctx, 42, false, "", 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || len(rows) != 0 {
		t.Fatalf("deleted row still listed: total=%d len=%d", total, len(rows))
	}
}

func TestListItemsVsGroupsAndPagination(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// 3 items, 1 group.
	for i := 0; i < 3; i++ {
		it := sampleItem()
		it.UniqueIdentifier = ""
		if _, err := s.Create(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	grp := &Digest{UserID: 42, IsGroup: true, UniqueIdentifier: "grp-1", Name: "Library", MD5Hash: "g1"}
	if _, err := s.Create(ctx, grp); err != nil {
		t.Fatal(err)
	}

	items, total, err := s.List(ctx, 42, false, "", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("items total: want 3, got %d", total)
	}
	if len(items) != 2 {
		t.Errorf("page size: want 2, got %d", len(items))
	}

	groups, gtotal, err := s.List(ctx, 42, true, "", 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if gtotal != 1 || len(groups) != 1 || !groups[0].IsGroup {
		t.Errorf("groups: total=%d len=%d", gtotal, len(groups))
	}
}

func TestListByIDs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	var ids []int64
	for i := 0; i < 3; i++ {
		it := sampleItem()
		it.UniqueIdentifier = ""
		id, _ := s.Create(ctx, it)
		ids = append(ids, id)
	}
	got, err := s.ListByIDs(ctx, 42, ids[:2])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
	// Empty id list returns empty, not an error.
	empty, err := s.ListByIDs(ctx, 42, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Errorf("nil ids: want 0 rows, got %d", len(empty))
	}
}

func TestTagCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	id, err := s.CreateTag(ctx, 42, "work")
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	tags, err := s.ListTags(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0].Name != "work" {
		t.Fatalf("list tags: %+v", tags)
	}

	if err := s.UpdateTag(ctx, 42, id, "personal"); err != nil {
		t.Fatalf("update tag: %v", err)
	}
	tags, _ = s.ListTags(ctx, 42)
	if tags[0].Name != "personal" {
		t.Errorf("tag not renamed: %q", tags[0].Name)
	}

	if err := s.DeleteTag(ctx, 42, id); err != nil {
		t.Fatalf("delete tag: %v", err)
	}
	tags, _ = s.ListTags(ctx, 42)
	if len(tags) != 0 {
		t.Errorf("tag not deleted: %+v", tags)
	}
}

func TestTagsScopedByUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateTag(ctx, 42, "shared"); err != nil {
		t.Fatal(err)
	}
	tags, _ := s.ListTags(ctx, 7)
	if len(tags) != 0 {
		t.Errorf("user 7 should see no tags, got %+v", tags)
	}
}
