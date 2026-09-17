package forestnote

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/source"
)

func TestManualWorkCancelledJoinedAndRefusedAfterStop(t *testing.T) {
	w := newBackgroundWork(context.Background())
	entered, cancelled, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- w.run(context.Background(), func(ctx context.Context) error {
			close(entered)
			<-ctx.Done()
			close(cancelled)
			<-release
			return ctx.Err()
		})
	}()
	<-entered
	go func() { w.close(); close(closed) }()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("manual request not cancelled")
	}
	select {
	case <-closed:
		t.Fatal("manual request not joined")
	default:
	}
	if err := w.run(context.Background(), func(context.Context) error { t.Error("late request ran"); return nil }); !errors.Is(err, errSourceStopping) {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown stuck")
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestStoppedSourceRefusesEveryManualMutation(t *testing.T) {
	s, err := NewSource(testDB(t), source.SourceRow{Name: "fixture"}, source.SharedDeps{}, ForestNoteDeps{Indexer: nopIndexer{}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Stop()
	checks := []error{s.EditTextBox(context.Background(), "box", "late"), s.ReprocessNotebook(context.Background(), "notebook")}
	_, err = s.PruneDevice(context.Background(), "site")
	checks = append(checks, err)
	_, err = s.SetDeviceLabel(context.Background(), "site", "late")
	checks = append(checks, err)
	_, err = s.CompactNow(context.Background())
	checks = append(checks, err)
	for _, err := range checks {
		if !errors.Is(err, errSourceStopping) {
			t.Fatal("stopped mutation was admitted", err)
		}
	}
}

func TestBackgroundCloseCancelsJoinsAndRejectsLateWork(t *testing.T) {
	w := newBackgroundWork(context.Background())
	started, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	if !w.launch(func(ctx context.Context) { close(started); <-ctx.Done(); <-release }) {
		t.Fatal("not admitted")
	}
	<-started
	go func() { w.close(); close(returned) }()
	<-w.ctx.Done()
	if w.launch(func(context.Context) { t.Error("late job ran") }) {
		t.Fatal("late job admitted")
	}
	select {
	case <-returned:
		t.Fatal("close did not join")
	default:
	}
	close(release)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not finish")
	}
	w.close()
}

func TestConcurrentBackgroundAdmissionAndClose(t *testing.T) {
	w := newBackgroundWork(context.Background())
	var admitted, finished atomic.Int64
	var callers sync.WaitGroup
	for n := 0; n < 100; n++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			if w.launch(func(ctx context.Context) { <-ctx.Done(); finished.Add(1) }) {
				admitted.Add(1)
			}
		}()
	}
	w.close()
	callers.Wait()
	if admitted.Load() != finished.Load() {
		t.Fatal("unjoined work", admitted.Load(), finished.Load())
	}
}

func TestSourceStopOwnsAuxiliaryJobs(t *testing.T) {
	s, err := NewSource(testDB(t), source.SourceRow{Name: "fixture"}, source.SharedDeps{}, ForestNoteDeps{Indexer: nopIndexer{}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	if !s.background.launch(func(ctx context.Context) { <-ctx.Done(); close(finished) }) {
		t.Fatal("job refused")
	}
	s.Stop()
	select {
	case <-finished:
	default:
		t.Fatal("source returned before auxiliary job stopped")
	}
	if s.background.launch(func(context.Context) { t.Error("stopped source accepted work") }) {
		t.Fatal("late admission")
	}
	s.Stop()
}
