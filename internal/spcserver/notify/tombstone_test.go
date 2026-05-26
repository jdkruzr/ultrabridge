package notify

import (
	"context"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/spcserver/digesttomb"
)

type fakeTombStore struct {
	pending []digesttomb.Tombstone
	acked   []int64
}

func (f *fakeTombStore) Pending(_ context.Context, userID int64) ([]digesttomb.Tombstone, error) {
	return f.pending, nil
}
func (f *fakeTombStore) Ack(_ context.Context, userID int64) error {
	f.acked = append(f.acked, userID)
	return nil
}

func TestDrainDigestBuildsPayload(t *testing.T) {
	store := &fakeTombStore{pending: []digesttomb.Tombstone{
		{UserID: 42, DigestID: 7, DataType: "2"},
		{UserID: 42, DigestID: 8, DataType: "1"},
	}}
	q := NewTombstoneQueue(store, nil)

	payload, ok := q.DrainDigest(context.Background(), "42")
	if !ok {
		t.Fatal("DrainDigest ok=false, want true (pending exist)")
	}
	for _, want := range []string{`"msgType":"DIGEST-SYN"`, "DELETE_DIGEST", `"id":7`, `"id":8`, `"dataType":"2"`, `"dataType":"1"`} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload missing %q: %s", want, payload)
		}
	}
}

func TestDrainDigestEmpty(t *testing.T) {
	q := NewTombstoneQueue(&fakeTombStore{}, nil)
	payload, ok := q.DrainDigest(context.Background(), "42")
	if ok || payload != "" {
		t.Errorf("empty queue should drain nothing, got ok=%v payload=%q", ok, payload)
	}
}

func TestDrainDigestBadUserID(t *testing.T) {
	q := NewTombstoneQueue(&fakeTombStore{pending: []digesttomb.Tombstone{{DigestID: 1}}}, nil)
	if _, ok := q.DrainDigest(context.Background(), "not-a-number"); ok {
		t.Error("non-numeric userID should drain nothing, not panic")
	}
}

func TestAckDigest(t *testing.T) {
	store := &fakeTombStore{}
	q := NewTombstoneQueue(store, nil)
	q.AckDigest(context.Background(), "42")
	if len(store.acked) != 1 || store.acked[0] != 42 {
		t.Errorf("AckDigest acked = %v, want [42]", store.acked)
	}
}
