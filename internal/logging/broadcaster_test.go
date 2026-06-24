package logging

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestSubscribeReturnsChannel verifies that Subscribe returns a channel that receives entries
func TestSubscribeReturnsChannel(t *testing.T) {
	broadcaster := NewLogBroadcaster()

	id, ch := broadcaster.Subscribe()
	if id < 0 {
		t.Errorf("Subscribe returned invalid ID: %d", id)
	}
	if ch == nil {
		t.Errorf("Subscribe returned nil channel")
	}
	broadcaster.Unsubscribe(id)
}

// TestBroadcastSingleSubscriber verifies entry broadcast to a single subscriber
func TestBroadcastSingleSubscriber(t *testing.T) {
	broadcaster := NewLogBroadcaster()
	id, ch := broadcaster.Subscribe()
	defer broadcaster.Unsubscribe(id)

	// Broadcast an entry
	broadcaster.Broadcast("[INFO] test message")

	// Verify subscriber receives it
	select {
	case msg := <-ch:
		if msg != "[INFO] test message" {
			t.Errorf("Got message %q, want %q", msg, "[INFO] test message")
		}
	case <-time.After(1 * time.Second):
		t.Errorf("Did not receive message within timeout")
	}
}

// TestBroadcastMultipleSubscribers verifies that all subscribers receive the same entry
func TestBroadcastMultipleSubscribers(t *testing.T) {
	broadcaster := NewLogBroadcaster()

	// Create 3 subscribers
	id1, ch1 := broadcaster.Subscribe()
	id2, ch2 := broadcaster.Subscribe()
	id3, ch3 := broadcaster.Subscribe()
	defer broadcaster.Unsubscribe(id1)
	defer broadcaster.Unsubscribe(id2)
	defer broadcaster.Unsubscribe(id3)

	// Broadcast an entry
	testMessage := "[WARN] multiple subscribers"
	broadcaster.Broadcast(testMessage)

	// Verify all subscribers receive it
	timeout := 1 * time.Second
	for i, ch := range []<-chan string{ch1, ch2, ch3} {
		select {
		case msg := <-ch:
			if msg != testMessage {
				t.Errorf("Subscriber %d got message %q, want %q", i+1, msg, testMessage)
			}
		case <-time.After(timeout):
			t.Errorf("Subscriber %d did not receive message within timeout", i+1)
		}
	}
}

// TestUnsubscribe verifies that Unsubscribe stops delivery to that subscriber
func TestUnsubscribe(t *testing.T) {
	broadcaster := NewLogBroadcaster()

	// Subscribe and get IDs
	id1, ch1 := broadcaster.Subscribe()
	id2, ch2 := broadcaster.Subscribe()

	// Broadcast first message
	broadcaster.Broadcast("[INFO] first message")

	// Consume from both
	timeout := 1 * time.Second
	select {
	case msg := <-ch1:
		if msg != "[INFO] first message" {
			t.Errorf("ch1 got unexpected message: %q", msg)
		}
	case <-time.After(timeout):
		t.Errorf("ch1 should receive first message")
	}

	select {
	case msg := <-ch2:
		if msg != "[INFO] first message" {
			t.Errorf("ch2 got unexpected message: %q", msg)
		}
	case <-time.After(timeout):
		t.Errorf("ch2 should receive first message")
	}

	// Unsubscribe ch1
	broadcaster.Unsubscribe(id1)

	// Broadcast second message
	broadcaster.Broadcast("[INFO] second message")

	// Verify ch2 receives it but ch1 does not
	timeout = 100 * time.Millisecond
	select {
	case msg := <-ch2:
		if msg != "[INFO] second message" {
			t.Errorf("ch2 got unexpected message: %q", msg)
		}
	case <-time.After(timeout):
		t.Errorf("ch2 should receive second message")
	}

	// ch1 should be closed now
	select {
	case msg, ok := <-ch1:
		if ok {
			t.Errorf("ch1 should be closed after unsubscribe, got message: %q", msg)
		}
		// Channel is closed, which is expected
	case <-time.After(timeout):
		// Expected: ch1 is closed and won't receive
	}

	broadcaster.Unsubscribe(id2)
}

// TestBroadcasterMultipleMessages verifies broadcasting several messages in sequence
func TestBroadcasterMultipleMessages(t *testing.T) {
	broadcaster := NewLogBroadcaster()
	id, ch := broadcaster.Subscribe()
	defer broadcaster.Unsubscribe(id)

	// Broadcast multiple messages quickly
	messages := []string{"[INFO] msg1", "[INFO] msg2", "[INFO] msg3"}
	for _, msg := range messages {
		broadcaster.Broadcast(msg)
	}

	// Collect all messages
	received := 0
	timeout := 1 * time.Second
	for {
		select {
		case msg := <-ch:
			if msg != "" {
				received++
			}
			if received >= len(messages) {
				return
			}
		case <-time.After(timeout):
			t.Errorf("Expected %d messages, received %d", len(messages), received)
			return
		}
	}
}

// TestRingBufferBackfillBasic verifies that new subscribers receive recent entries via backfill
// This tests the basic backfill case where the ring buffer is not yet full
func TestRingBufferBackfillBasic(t *testing.T) {
	broadcaster := NewLogBroadcaster()

	// Broadcast N entries before subscribing
	numEntries := 10
	expectedMessages := make([]string, numEntries)
	for i := 0; i < numEntries; i++ {
		msg := "[INFO] message " + string(rune('0'+i))
		expectedMessages[i] = msg
		broadcaster.Broadcast(msg)
	}

	// Subscribe a new client - should receive backfill
	_, ch := broadcaster.Subscribe()

	// Verify channel receives backfill entries in chronological order
	timeout := 1 * time.Second
	receivedMessages := make([]string, 0, numEntries)

	for i := 0; i < numEntries; i++ {
		select {
		case msg := <-ch:
			receivedMessages = append(receivedMessages, msg)
		case <-time.After(timeout):
			t.Fatalf("Timeout waiting for backfill entry %d", i)
		}
	}

	// Verify all expected messages were received in order
	if len(receivedMessages) != numEntries {
		t.Errorf("Expected %d backfill messages, got %d", numEntries, len(receivedMessages))
	}

	for i, msg := range receivedMessages {
		if msg != expectedMessages[i] {
			t.Errorf("Backfill message %d: got %q, want %q", i, msg, expectedMessages[i])
		}
	}
}

// TestRingBufferBackfillFullWrap verifies backfill with ring buffer wraparound
// This tests that when the ring buffer is full and has wrapped around,
// new subscribers get the last 100 entries in chronological order
func TestRingBufferBackfillFullWrap(t *testing.T) {
	broadcaster := NewLogBroadcaster()

	// Broadcast more than ringBufferSize (100) entries to trigger wraparound
	numEntries := 150
	for i := 0; i < numEntries; i++ {
		msg := "[INFO] message " + formatIndex(i)
		broadcaster.Broadcast(msg)
	}

	// Subscribe a new client - should receive the last 100 entries (50-149)
	_, ch := broadcaster.Subscribe()

	// Collect backfilled entries
	timeout := 1 * time.Second
	receivedMessages := make([]string, 0, 100)

	for i := 0; i < 100; i++ {
		select {
		case msg := <-ch:
			receivedMessages = append(receivedMessages, msg)
		case <-time.After(timeout):
			t.Fatalf("Timeout waiting for backfill entry %d", i)
		}
	}

	// Verify exactly 100 entries were received
	if len(receivedMessages) != 100 {
		t.Errorf("Expected 100 backfill messages, got %d", len(receivedMessages))
	}

	// Verify they are the last 100 entries (indices 50-149) in order
	for i, msg := range receivedMessages {
		expectedIndex := 50 + i
		expectedMsg := "[INFO] message " + formatIndex(expectedIndex)
		if msg != expectedMsg {
			t.Errorf("Backfill message %d: got %q, want %q", i, msg, expectedMsg)
		}
	}

	// Verify no extra messages are in the channel (should time out trying to read more)
	select {
	case msg := <-ch:
		t.Errorf("Unexpected message in channel after backfill: %q", msg)
	case <-time.After(100 * time.Millisecond):
		// Expected: no more backfill messages
	}
}

// TestRingBufferBackfillMultipleSubscribers verifies that multiple subscribers
// each receive independent backfill from the ring buffer
func TestRingBufferBackfillMultipleSubscribers(t *testing.T) {
	broadcaster := NewLogBroadcaster()

	// Broadcast some initial entries
	numInitial := 5
	for i := 0; i < numInitial; i++ {
		broadcaster.Broadcast("[INFO] initial " + formatIndex(i))
	}

	// Subscribe first client - gets backfill
	id1, ch1 := broadcaster.Subscribe()
	defer broadcaster.Unsubscribe(id1)

	// Broadcast more entries
	numBetween := 3
	for i := 0; i < numBetween; i++ {
		broadcaster.Broadcast("[INFO] between " + formatIndex(i))
	}

	// Subscribe second client - gets backfill with all entries so far (5 + 3 = 8)
	id2, ch2 := broadcaster.Subscribe()
	defer broadcaster.Unsubscribe(id2)

	timeout := 1 * time.Second

	// Verify ch1 received initial backfill
	for i := 0; i < numInitial; i++ {
		select {
		case msg := <-ch1:
			if msg != "[INFO] initial "+formatIndex(i) {
				t.Errorf("ch1 backfill %d: got %q", i, msg)
			}
		case <-time.After(timeout):
			t.Errorf("ch1 timeout on backfill message %d", i)
		}
	}

	// Verify ch1 received "between" entries as live messages
	for i := 0; i < numBetween; i++ {
		select {
		case msg := <-ch1:
			if msg != "[INFO] between "+formatIndex(i) {
				t.Errorf("ch1 live %d: got %q", i, msg)
			}
		case <-time.After(timeout):
			t.Errorf("ch1 timeout on live message %d", i)
		}
	}

	// Verify ch2 received all entries as backfill (5 initial + 3 between = 8)
	// First 5 should be initial entries
	for i := 0; i < numInitial; i++ {
		select {
		case msg := <-ch2:
			if msg != "[INFO] initial "+formatIndex(i) {
				t.Errorf("ch2 backfill initial %d: got %q", i, msg)
			}
		case <-time.After(timeout):
			t.Errorf("ch2 timeout on backfill initial message %d", i)
		}
	}

	// Next 3 should be between entries
	for i := 0; i < numBetween; i++ {
		select {
		case msg := <-ch2:
			if msg != "[INFO] between "+formatIndex(i) {
				t.Errorf("ch2 backfill between %d: got %q", i, msg)
			}
		case <-time.After(timeout):
			t.Errorf("ch2 timeout on backfill between message %d", i)
		}
	}

	// Broadcast an entry after ch2 subscribes
	broadcaster.Broadcast("[INFO] after second subscribe")

	// Both should receive this new message
	select {
	case msg := <-ch1:
		if msg != "[INFO] after second subscribe" {
			t.Errorf("ch1 new message: got %q", msg)
		}
	case <-time.After(timeout):
		t.Errorf("ch1 timeout on new message")
	}

	select {
	case msg := <-ch2:
		if msg != "[INFO] after second subscribe" {
			t.Errorf("ch2 new message: got %q", msg)
		}
	case <-time.After(timeout):
		t.Errorf("ch2 timeout on new message")
	}
}

func TestBroadcastingHandlerIncludesAttrsAndGroups(t *testing.T) {
	broadcaster := NewLogBroadcaster()
	id, ch := broadcaster.Subscribe()
	defer broadcaster.Unsubscribe(id)

	var sink bytes.Buffer
	handler := NewBroadcastingHandler(slog.NewTextHandler(&sink, nil), broadcaster).
		WithAttrs([]slog.Attr{slog.String("component", "sync")}).
		WithGroup("request").
		WithAttrs([]slog.Attr{slog.String("id", "abc")})

	record := slog.NewRecord(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC), slog.LevelInfo, "started", 0)
	record.AddAttrs(slog.Int("count", 3))
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	select {
	case msg := <-ch:
		for _, want := range []string{"[INFO] started", "component=sync", "request.id=abc", "request.count=3"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("broadcast %q missing %q", msg, want)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for broadcast")
	}
}

// Helper function to format index with zero-padding for consistent sorting
func formatIndex(i int) string {
	if i < 10 {
		return "00" + string(rune('0'+i))
	} else if i < 100 {
		return "0" + string(rune('0'+i/10)) + string(rune('0'+i%10))
	}
	return string(rune('0'+i/100)) + string(rune('0'+(i/10)%10)) + string(rune('0'+i%10))
}
