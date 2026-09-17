package libraryrestore

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestParentCancelledWhileJoiningCannotPublish(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	joining, release := make(chan struct{}), make(chan struct{})
	var starts atomic.Int32
	w := NewWorkers(ctx, func(context.Context) func() {
		starts.Add(1)
		return func() {
			select {
			case <-joining:
			default:
				close(joining)
			}
			<-release
		}
	})
	defer w.Close()
	result := make(chan error, 1)
	go func() {
		result <- w.Exclusive(context.Background(), func() error { t.Error("publication after shutdown cancellation"); return nil })
	}()
	select {
	case <-joining:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("join not reached")
	}
	cancel()
	close(release)
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("join stuck")
	}
	if starts.Load() != 1 {
		t.Fatal("cancelled owner restarted")
	}
}

func TestClosedWorkersCannotRestartOrPublish(t *testing.T) {
	var starts atomic.Int32
	w := NewWorkers(context.Background(), func(context.Context) func() { starts.Add(1); return func() {} })
	w.Close()
	w.Close()
	if err := w.Exclusive(context.Background(), func() error { t.Fatal("closed publisher ran"); return nil }); !errors.Is(err, ErrWorkersClosed) {
		t.Fatal(err)
	}
	if starts.Load() != 1 {
		t.Fatal("workers resurrected")
	}
}
func TestParentCancellationDuringPublicationDoesNotRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var starts atomic.Int32
	w := NewWorkers(ctx, func(context.Context) func() { starts.Add(1); return func() {} })
	defer w.Close()
	if err := w.Exclusive(context.Background(), func() error { cancel(); return nil }); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 {
		t.Fatal("shutdown restarted workers")
	}
	if err := w.Exclusive(context.Background(), func() error { t.Fatal("cancelled publisher ran"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
