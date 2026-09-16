package forestnote

import (
	"context"
	"sync"
)

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

func (w *backgroundWork) launch(job func(context.Context)) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.ctx.Err() != nil {
		return false
	}
	w.wg.Add(1)
	go func() { defer w.wg.Done(); job(w.ctx) }()
	return true
}

func (w *backgroundWork) close() {
	w.mu.Lock()
	w.closed = true
	w.cancel()
	w.mu.Unlock()
	w.wg.Wait()
}
