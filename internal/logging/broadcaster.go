package logging

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

const ringBufferSize = 100

// LogBroadcaster manages log entry broadcast to multiple WebSocket clients.
// It maintains a ring buffer of recent entries and sends new entries to all
// active subscribers.
type LogBroadcaster struct {
	mu          sync.RWMutex
	subscribers map[int]chan string
	nextID      int
	ringBuffer  [ringBufferSize]string
	ringIndex   int
	ringCount   int
}

// NewLogBroadcaster creates a new broadcaster.
func NewLogBroadcaster() *LogBroadcaster {
	return &LogBroadcaster{
		subscribers: make(map[int]chan string),
	}
}

// Subscribe registers a new subscriber and returns a channel and subscriber ID.
// The channel is pre-populated with recent entries from the ring buffer.
// The subscriber ID must be used to unsubscribe later.
func (lb *LogBroadcaster) Subscribe() (int, <-chan string) {
	lb.mu.Lock()

	ch := make(chan string, ringBufferSize+10) // Increase buffer to hold backfill
	id := lb.nextID
	lb.nextID++

	lb.subscribers[id] = ch

	// Send recent entries to the new subscriber as backfill (synchronously)
	// Send entries from the ring buffer in chronological order
	if lb.ringCount < ringBufferSize {
		// Ring buffer not full yet - send from index 0 to ringIndex
		for i := 0; i < lb.ringIndex; i++ {
			if lb.ringBuffer[i] != "" {
				ch <- lb.ringBuffer[i]
			}
		}
	} else {
		// Ring buffer is full - send from ringIndex to end, then start to ringIndex
		for i := lb.ringIndex; i < ringBufferSize; i++ {
			if lb.ringBuffer[i] != "" {
				ch <- lb.ringBuffer[i]
			}
		}
		for i := 0; i < lb.ringIndex; i++ {
			if lb.ringBuffer[i] != "" {
				ch <- lb.ringBuffer[i]
			}
		}
	}

	lb.mu.Unlock()
	return id, ch
}

// Unsubscribe removes a subscriber. Note: we need the ID which is not directly
// available, so we use a different approach in practice - the returned channel
// can be used with a separate cleanup mechanism or we maintain a map.
// For simplicity, callers should close the channel they receive.
func (lb *LogBroadcaster) Unsubscribe(id int) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	if ch, exists := lb.subscribers[id]; exists {
		close(ch)
		delete(lb.subscribers, id)
	}
}

// Broadcast sends a new log entry to all active subscribers and adds it to
// the ring buffer.
func (lb *LogBroadcaster) Broadcast(entry string) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	// Add to ring buffer
	lb.ringBuffer[lb.ringIndex] = entry
	lb.ringIndex = (lb.ringIndex + 1) % ringBufferSize
	if lb.ringCount < ringBufferSize {
		lb.ringCount++
	}

	// Send to all subscribers
	for _, ch := range lb.subscribers {
		select {
		case ch <- entry:
		default:
			// Channel full, skip (non-blocking)
		}
	}
}

// BroadcastingHandler wraps an slog.Handler to broadcast entries to the
// broadcaster in addition to the original handler.
type BroadcastingHandler struct {
	handler     slog.Handler
	broadcaster *LogBroadcaster
	levelFilter slog.Level
}

// NewBroadcastingHandler creates a handler that broadcasts entries to the
// broadcaster.
func NewBroadcastingHandler(handler slog.Handler, broadcaster *LogBroadcaster) *BroadcastingHandler {
	return &BroadcastingHandler{
		handler:     handler,
		broadcaster: broadcaster,
		levelFilter: slog.LevelDebug,
	}
}

// Handle implements slog.Handler. It writes to the underlying handler and
// broadcasts the entry.
func (bh *BroadcastingHandler) Handle(ctx context.Context, record slog.Record) error {
	// Delegate to the original handler
	if err := bh.handler.Handle(ctx, record); err != nil {
		return err
	}

	// Broadcast the entry as text
	bh.broadcaster.Broadcast(formatLogEntry(record))
	return nil
}

// WithAttrs implements slog.Handler.
func (bh *BroadcastingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &BroadcastingHandler{
		handler:     bh.handler.WithAttrs(attrs),
		broadcaster: bh.broadcaster,
		levelFilter: bh.levelFilter,
	}
}

// WithGroup implements slog.Handler.
func (bh *BroadcastingHandler) WithGroup(name string) slog.Handler {
	return &BroadcastingHandler{
		handler:     bh.handler.WithGroup(name),
		broadcaster: bh.broadcaster,
		levelFilter: bh.levelFilter,
	}
}

// Enabled implements slog.Handler.
func (bh *BroadcastingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return bh.handler.Enabled(ctx, level)
}

// formatLogEntry formats a log record with all attributes for broadcasting.
func formatLogEntry(record slog.Record) string {
	var b strings.Builder
	b.WriteString(record.Time.UTC().Format("15:04:05"))
	b.WriteString(" [")
	b.WriteString(record.Level.String())
	b.WriteString("] ")
	b.WriteString(record.Message)

	record.Attrs(func(a slog.Attr) bool {
		b.WriteString("  ")
		b.WriteString(a.Key)
		b.WriteString("=")
		b.WriteString(fmt.Sprintf("%v", a.Value.Any()))
		return true
	})

	return b.String()
}
