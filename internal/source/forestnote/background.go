package forestnote

import (
	"context"
	"errors"
	"sync"
)

var errSourceStopping = errors.New("forestnote source is not running")

// backgroundWork owns the source's auxiliary jobs, not just its OCR bridge.
// Closing admission before Wait prevents a concurrent reprocess request from
// starting an untracked job after shutdown/library-replacement quiescence.
type backgroundWork struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func newBackgroundWork(parent context.Context) *backgroundWork {
	ctx, cancel := context.WithCancel(parent)
	return &backgroundWork{ctx: ctx, cancel: cancel}
}

func (w *backgroundWork) admit() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.ctx.Err() != nil {
		return false
	}
	w.wg.Add(1)
	return true
}

func (w *backgroundWork) launch(job func(context.Context)) bool {
	if !w.admit() {
		return false
	}
	go func() { defer w.wg.Done(); job(w.ctx) }()
	return true
}

// run includes synchronous manual mutations in the SAME admission/join boundary.
// Cancellation belongs to both the request and source; neither can outlive Stop.
func (w *backgroundWork) run(ctx context.Context, job func(context.Context) error) error {
	if w == nil || !w.admit() {
		return errSourceStopping
	}
	defer w.wg.Done()
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(w.ctx, cancel)
	defer stop()
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if err := run.Err(); err != nil {
		return err
	}
	return job(run)
}

func (w *backgroundWork) close() {
	w.mu.Lock()
	w.closed = true
	w.cancel()
	w.mu.Unlock()
	w.wg.Wait()
}
