package libraryrestore

import (
	"context"
	"sync"
)

// Workers owns cancellation AND joining. Start creates new worker instances so
// old in-memory cursor/head values never survive a whole-library replacement.
type Workers struct {
	mu     sync.Mutex
	parent context.Context
	start  func(context.Context) func()
	cancel context.CancelFunc
	join   func()
}

func NewWorkers(parent context.Context, start func(context.Context) func()) *Workers {
	w := &Workers{parent: parent, start: start}
	w.resume()
	return w
}
func (w *Workers) resume() {
	ctx, cancel := context.WithCancel(w.parent)
	w.cancel = cancel
	w.join = w.start(ctx)
}
func (w *Workers) Close() { w.mu.Lock(); defer w.mu.Unlock(); w.cancel(); w.join() }
func (w *Workers) Exclusive(ctx context.Context, publish func() error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cancel()
	w.join()
	defer w.resume()
	if err := ctx.Err(); err != nil {
		return err
	}
	return publish()
}
