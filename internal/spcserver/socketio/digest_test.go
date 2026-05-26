package socketio

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sysop/ultrabridge/internal/spcserver/auth"
)

type fakeDigestQueue struct {
	drain   string
	drainOK bool
	acked   chan struct{}
}

func (f *fakeDigestQueue) DrainDigest(_ context.Context, _ string) (string, bool) {
	return f.drain, f.drainOK
}
func (f *fakeDigestQueue) AckDigest(_ context.Context, _ string) {
	if f.acked != nil {
		select {
		case f.acked <- struct{}{}:
		default:
		}
	}
}

// TestRattaPingDrainsAndAcks: a ratta_ping emits the pending tombstones as a
// "digest" event and then clears them (AckDigest) — the device gives no
// app-level ack, so UB acks on its own successful emit.
func TestRattaPingDrainsAndAcks(t *testing.T) {
	drainPayload := `{"msgType":"DIGEST-SYN","data":[]}`
	fake := &fakeDigestQueue{drain: drainPayload, drainOK: true, acked: make(chan struct{}, 1)}
	h := NewHandler(wsSecret, NewRegistry(), nil)
	h.SetDigestQueue(fake)
	srv := httptest.NewServer(h)
	defer srv.Close()

	c, _, err := dialWS(t, srv, auth.Mint("u1", wsSecret))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	readFrame(t, c) // open
	readFrame(t, c) // proactive 40

	c.WriteMessage(websocket.TextMessage, []byte(`42["ratta_ping"]`))
	if got := readFrame(t, c); got != `42["ratta_ping","Received"]` {
		t.Fatalf("ratta_ping ack: got %q", got)
	}
	// The payload rides as a STRING arg (device gson-parses args[0]), so the
	// frame is the escaped-string form — built via EncodeEvent for fidelity.
	want := string(EncodeEvent("digest", drainPayload))
	if got := readFrame(t, c); got != want {
		t.Errorf("digest drain frame:\n got %q\nwant %q", got, want)
	}
	// The tombstone is cleared right after the successful emit.
	select {
	case <-fake.acked:
	case <-time.After(2 * time.Second):
		t.Error("AckDigest not called after a successful digest emit")
	}
}

// TestRattaPingNoDrainWhenEmpty: a ratta_ping with nothing pending emits no
// digest frame and does not ack.
func TestRattaPingNoDrainWhenEmpty(t *testing.T) {
	fake := &fakeDigestQueue{drainOK: false, acked: make(chan struct{}, 1)}
	h := NewHandler(wsSecret, NewRegistry(), nil)
	h.SetDigestQueue(fake)
	srv := httptest.NewServer(h)
	defer srv.Close()

	c, _, err := dialWS(t, srv, auth.Mint("u1", wsSecret))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	readFrame(t, c) // open
	readFrame(t, c) // proactive 40

	c.WriteMessage(websocket.TextMessage, []byte(`42["ratta_ping"]`))
	if got := readFrame(t, c); got != `42["ratta_ping","Received"]` {
		t.Fatalf("ratta_ping ack: got %q", got)
	}
	// No digest frame and no ack should follow: a native ping round-trips cleanly.
	c.WriteMessage(websocket.TextMessage, []byte("2"))
	if got := readFrame(t, c); got != "3" {
		t.Errorf("expected pong (no digest frame), got %q", got)
	}
	select {
	case <-fake.acked:
		t.Error("AckDigest must not be called when nothing was drained")
	case <-time.After(200 * time.Millisecond):
	}
}
