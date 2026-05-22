package notify

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeEmitter records the last Emit call and returns a configurable delivered count.
type fakeEmitter struct {
	userID, event string
	payload       any
	calls         int
	delivered     int
}

func (f *fakeEmitter) Emit(userID, event string, payload any) int {
	f.calls++
	f.userID, f.event, f.payload = userID, event, payload
	return f.delivered
}

func fixedUser(id string) UserIDFunc {
	return func(context.Context) (string, error) { return id, nil }
}

// TestNotifyEmitsStartSync verifies a well-formed FILE-SYN STARTSYNC
// ServerMessage is emitted to the resolved user. Verifies: spc-phase-1.AC4.5
func TestNotifyEmitsStartSync(t *testing.T) {
	em := &fakeEmitter{delivered: 1}
	n := NewSocketNotifier(em, fixedUser("u1"), nil)

	if err := n.Notify(context.Background()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if em.calls != 1 || em.userID != "u1" || em.event != "ServerMessage" {
		t.Fatalf("emit: calls=%d user=%q event=%q", em.calls, em.userID, em.event)
	}
	payload, _ := em.payload.(string)
	for _, want := range []string{"STARTSYNC", "FILE-SYN", `"code":"200"`} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload missing %q: %s", want, payload)
		}
	}
}

// TestNotifyNoUserIsNoOp verifies no emit and no error when no device userId.
func TestNotifyNoUserIsNoOp(t *testing.T) {
	em := &fakeEmitter{}
	n := NewSocketNotifier(em, fixedUser(""), nil)
	if err := n.Notify(context.Background()); err != nil {
		t.Errorf("Notify err: %v", err)
	}
	if em.calls != 0 {
		t.Errorf("expected no emit when userId empty, got %d", em.calls)
	}
}

// TestNotifyNoConnectionIsNoError verifies delivered=0 (no live conn) is not an error.
func TestNotifyNoConnectionIsNoError(t *testing.T) {
	em := &fakeEmitter{delivered: 0}
	n := NewSocketNotifier(em, fixedUser("u1"), nil)
	if err := n.Notify(context.Background()); err != nil {
		t.Errorf("no-connection should be nil error, got %v", err)
	}
}

// TestNotifyResolveErrorIsNoError verifies a userId resolve error degrades gracefully.
func TestNotifyResolveErrorIsNoError(t *testing.T) {
	em := &fakeEmitter{}
	n := NewSocketNotifier(em, func(context.Context) (string, error) {
		return "", errors.New("db down")
	}, nil)
	if err := n.Notify(context.Background()); err != nil {
		t.Errorf("resolve error should be nil error, got %v", err)
	}
	if em.calls != 0 {
		t.Errorf("expected no emit on resolve error, got %d", em.calls)
	}
}
