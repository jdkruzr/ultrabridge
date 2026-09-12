package readerstore

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

type ProcessChange func(context.Context, Change) error
type WorkerOptions struct {
	PageSize                            int
	PollInterval, PageDelay, RetryDelay time.Duration
	// OnError must be quick and must not log book/ink payloads or credentials.
	OnError func(error)
}

func DefaultWorkerOptions() WorkerOptions {
	return WorkerOptions{PageSize: 32, PollInterval: time.Second, PageDelay: 10 * time.Millisecond, RetryDelay: time.Second}
}

// Worker has one serial materializer and an independent serial downstream
// delivery loop. Neither is a sync request handler. One owner per library;
// duplicate Run calls on this owner fail, not spawn overlapping consumers.
type Worker struct {
	store        *Store
	process      ProcessChange
	options      WorkerOptions
	wake         chan struct{}
	deliveryWake chan struct{}
	running      atomic.Bool
}

func NewWorker(s *Store, process ProcessChange, options WorkerOptions) (*Worker, error) {
	if s == nil || options.PageSize < 1 || options.PageSize > 128 || options.PollInterval <= 0 || options.PageDelay <= 0 || options.RetryDelay <= 0 {
		return nil, fmt.Errorf("invalid reader worker options")
	}
	return &Worker{store: s, process: process, options: options, wake: make(chan struct{}, 1), deliveryWake: make(chan struct{}, 1)}, nil
}
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Wake is a coalesced post-commit hint, never a source of durability. Startup
// scans and periodic scalar probes recover missing hints (including process loss).
func (w *Worker) Wake() { signal(w.wake) }
func pause(ctx context.Context, delay time.Duration, wake <-chan struct{}) bool {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	case <-wake:
		return true
	}
}
func (w *Worker) report(err error) {
	if w.options.OnError != nil {
		w.options.OnError(err)
	}
}

func (w *Worker) Run(ctx context.Context) error {
	if !w.running.CompareAndSwap(false, true) {
		return fmt.Errorf("reader worker already running")
	}
	defer w.running.Store(false)
	done := make(chan struct{})
	if w.process != nil {
		go func() { defer close(done); w.deliver(ctx) }()
	} else {
		close(done)
	}
	defer func() { <-done }()
	var after, head int64
	active, progress := true, false // Always recover old pending rows on startup.
	for ctx.Err() == nil {
		if after == 0 {
			current, err := w.store.incomingHead(ctx)
			if err != nil {
				w.report(err)
				if !pause(ctx, w.options.RetryDelay, nil) {
					break
				}
				continue
			}
			if !active && current == head {
				if !pause(ctx, w.options.PollInterval, w.wake) {
					break
				}
				continue
			}
			head = current
			active = true
		}
		page, err := w.store.Drain(ctx, after, w.options.PageSize)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			w.report(err)
			if !pause(ctx, w.options.RetryDelay, nil) {
				break
			}
			continue
		}
		if len(page.Changed) > 0 {
			signal(w.deliveryWake)
		}
		for _, r := range page.Records {
			if r.State != "pending" {
				progress = true
			}
		}
		after = page.Next
		if after == 0 {
			active = progress
			progress = false
		}
		// A real scheduling break after every bounded page; notifications cannot
		// bypass it or the error backoff and monopolize the writer.
		if !pause(ctx, w.options.PageDelay, nil) {
			break
		}
	}
	return ctx.Err()
}

func (w *Worker) deliver(ctx context.Context) {
	for ctx.Err() == nil {
		changes, err := w.store.Changes(ctx, w.options.PageSize)
		if err == nil {
			for _, c := range changes {
				if ctx.Err() != nil {
					return
				}
				if err = w.process(ctx, c); err != nil {
					break
				}
				if err = w.store.CompleteChange(ctx, c.Seq); err != nil {
					break
				}
			}
		}
		delay := w.options.PageDelay
		var wake <-chan struct{}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.report(err)
			delay = w.options.RetryDelay
		} else if len(changes) == 0 {
			delay = w.options.PollInterval
			wake = w.deliveryWake
		}
		if !pause(ctx, delay, wake) {
			return
		}
	}
}
