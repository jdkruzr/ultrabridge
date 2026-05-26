package service

import (
	"context"
	"errors"
	"testing"

	"github.com/sysop/ultrabridge/internal/digeststore"
)

// fakeDigestStore implements DigestStore for the delete tests. Only the methods
// DeleteDigest touches are meaningful; the list methods return empty.
type fakeDigestStore struct {
	item        *digeststore.Digest
	getErr      error
	softDeleted []struct {
		userID, id int64
	}
	softDeleteErr error
}

func (f *fakeDigestStore) ListItems(context.Context, string, string, int, int) ([]digeststore.Digest, int64, error) {
	return nil, 0, nil
}
func (f *fakeDigestStore) ListGroups(context.Context) ([]digeststore.Digest, error) { return nil, nil }
func (f *fakeDigestStore) GetItem(_ context.Context, id int64) (*digeststore.Digest, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.item, nil
}
func (f *fakeDigestStore) SoftDelete(_ context.Context, userID, id int64) error {
	if f.softDeleteErr != nil {
		return f.softDeleteErr
	}
	f.softDeleted = append(f.softDeleted, struct{ userID, id int64 }{userID, id})
	return nil
}

type recordingDeindexer struct{ uids []string }

func (r *recordingDeindexer) Deindex(uid string) { r.uids = append(r.uids, uid) }

type recordingTombstone struct {
	ids       []int64
	dataTypes []string
	err       error
}

func (r *recordingTombstone) NotifyDigestDelete(_ context.Context, id int64, dataType string) error {
	r.ids = append(r.ids, id)
	r.dataTypes = append(r.dataTypes, dataType)
	return r.err
}

func newDeleteSvc(store DigestStore, di DigestDeindexer) (*digestService, *recordingTombstone) {
	s := NewDigestService(store, di).(*digestService)
	tomb := &recordingTombstone{}
	s.SetTombstoneNotifier(tomb)
	return s, tomb
}

func TestDeleteDigest_SoftDeletesDeindexesAndPushes(t *testing.T) {
	store := &fakeDigestStore{item: &digeststore.Digest{ID: 7, UserID: 3, UniqueIdentifier: "abc", SourceType: 2}}
	di := &recordingDeindexer{}
	svc, tomb := newDeleteSvc(store, di)

	if err := svc.DeleteDigest(context.Background(), 7); err != nil {
		t.Fatalf("DeleteDigest: %v", err)
	}
	if len(store.softDeleted) != 1 || store.softDeleted[0].userID != 3 || store.softDeleted[0].id != 7 {
		t.Fatalf("SoftDelete not called with (userID=3,id=7): %+v", store.softDeleted)
	}
	if len(di.uids) != 1 || di.uids[0] != "abc" {
		t.Errorf("Deindex(uid) not called with %q: %v", "abc", di.uids)
	}
	if len(tomb.ids) != 1 || tomb.ids[0] != 7 || tomb.dataTypes[0] != "2" {
		t.Errorf("tombstone push wrong: ids=%v dataTypes=%v", tomb.ids, tomb.dataTypes)
	}
}

func TestDeleteDigest_NotFound(t *testing.T) {
	store := &fakeDigestStore{getErr: digeststore.ErrNotFound}
	di := &recordingDeindexer{}
	svc, tomb := newDeleteSvc(store, di)

	if err := svc.DeleteDigest(context.Background(), 99); !errors.Is(err, digeststore.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if len(store.softDeleted) != 0 || len(di.uids) != 0 || len(tomb.ids) != 0 {
		t.Errorf("nothing should mutate on not-found: del=%v di=%v push=%v", store.softDeleted, di.uids, tomb.ids)
	}
}

func TestDeleteDigest_PushErrorDoesNotFail(t *testing.T) {
	store := &fakeDigestStore{item: &digeststore.Digest{ID: 1, UserID: 1, UniqueIdentifier: "x", SourceType: 1}}
	svc, tomb := newDeleteSvc(store, &recordingDeindexer{})
	tomb.err = errors.New("socket down")

	if err := svc.DeleteDigest(context.Background(), 1); err != nil {
		t.Fatalf("push error must not fail the delete, got %v", err)
	}
	if len(store.softDeleted) != 1 {
		t.Errorf("soft-delete should still happen, got %+v", store.softDeleted)
	}
}

func TestDeleteDigest_SoftDeleteErrorPropagates(t *testing.T) {
	store := &fakeDigestStore{
		item:          &digeststore.Digest{ID: 1, UserID: 1, UniqueIdentifier: "x", SourceType: 2},
		softDeleteErr: errors.New("db locked"),
	}
	svc, tomb := newDeleteSvc(store, &recordingDeindexer{})

	if err := svc.DeleteDigest(context.Background(), 1); err == nil {
		t.Fatal("expected soft-delete error to propagate")
	}
	if len(tomb.ids) != 0 {
		t.Errorf("must not push tombstone when soft-delete failed: %v", tomb.ids)
	}
}

func TestDeleteDigest_NilDepsNoPanic(t *testing.T) {
	store := &fakeDigestStore{item: &digeststore.Digest{ID: 5, UserID: 2, UniqueIdentifier: "u", SourceType: 2}}
	svc := NewDigestService(store, nil).(*digestService) // nil deindexer, no tombstone set

	if err := svc.DeleteDigest(context.Background(), 5); err != nil {
		t.Fatalf("DeleteDigest with nil deps: %v", err)
	}
	if len(store.softDeleted) != 1 {
		t.Errorf("soft-delete should happen with nil deps, got %+v", store.softDeleted)
	}
}
