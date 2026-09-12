package readerstore

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func workerOptions() WorkerOptions {
	o := DefaultWorkerOptions()
	o.PageSize = 1
	o.PageDelay = time.Millisecond
	o.PollInterval = 10 * time.Millisecond
	o.RetryDelay = 25 * time.Millisecond
	return o
}
func eventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("worker condition not reached")
}
func startWorker(t *testing.T, w *Worker) func() {
	t.Helper()
	c, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- w.Run(c) }()
	var stopped atomic.Bool
	stop := func() {
		if stopped.Swap(true) {
			return
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("worker exit: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("worker did not join")
		}
	}
	t.Cleanup(stop)
	return stop
}
func newWorker(t *testing.T, s *Store, process ProcessChange, o WorkerOptions) *Worker {
	t.Helper()
	w, err := NewWorker(s, process, o)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestWorkerStartupLostWakeDependenciesAndIdle(t *testing.T) {
	db, s := installed(t)
	db.SetMaxOpenConns(1)
	exec(t, db, "CREATE TABLE attempts(n INTEGER NOT NULL)")
	exec(t, db, "INSERT INTO attempts VALUES(0)")
	exec(t, db, "CREATE TRIGGER attempts AFTER UPDATE ON reader_store_incoming BEGIN UPDATE attempts SET n=n+1; END")
	ops := fixture()
	stage(t, s, ops[3]) // durable orphan before startup, no wake
	w := newWorker(t, s, nil, workerOptions())
	stop := startWorker(t, w)
	reads := func() int {
		var n int
		if err := db.QueryRow("SELECT n FROM attempts").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	eventually(t, func() bool { return reads() == 1 })
	time.Sleep(60 * time.Millisecond)
	if reads() != 1 {
		t.Fatal("idle orphan was repeatedly drained")
	}
	stage(t, s, ops[4], ops[2], ops[1], ops[0]) // lost post-commit hint recovered by scalar poll
	eventually(t, func() bool { return count(t, db, "fn_reader_stroke") == 1 })
	eventually(t, func() bool {
		var n int
		db.QueryRow("SELECT count(*) FROM reader_store_incoming WHERE state='pending'").Scan(&n)
		return n == 0
	})
	stop()
	p, err := s.Projection(ctx, "n", DefaultLimits())
	if err != nil || p == nil || len(p.Strokes) != 1 {
		t.Fatalf("projection: %+v %v", p, err)
	}
	changes, err := s.Changes(ctx, 128)
	if err != nil || len(changes) == 0 {
		t.Fatalf("downstream work lost without consumer: %v", err)
	}
	before := reads()
	stop = startWorker(t, newWorker(t, s, nil, workerOptions()))
	time.Sleep(30 * time.Millisecond)
	stop()
	if reads() != before {
		t.Fatal("restart reprocessed settled receipts")
	}
}

func TestWorkerJournalAtomicityAndUpgrade(t *testing.T) {
	db, s := installed(t)
	stage(t, s, fixture()[0])
	exec(t, db, "CREATE TRIGGER fail_job BEFORE INSERT ON reader_store_changes BEGIN SELECT RAISE(ABORT,'job failure'); END")
	if _, err := s.Drain(ctx, 0, 32); err == nil {
		t.Fatal("injected journal failure accepted")
	}
	if count(t, db, "fn_reader_book") != 0 || count(t, db, "reader_store_changes") != 0 {
		t.Fatal("partial mirror/job commit")
	}
	var state string
	db.QueryRow("SELECT state FROM reader_store_incoming").Scan(&state)
	if state != "pending" {
		t.Fatal("failed job settled receipt")
	}
	exec(t, db, "DROP TRIGGER fail_job")
	drain(t, s)
	if count(t, db, "reader_store_changes") != 1 {
		t.Fatal("missing durable job")
	}
	// Simulate a v1 candidate DB whose rows were materialized before the queue
	// existed. Only disposable test tables are removed; the mirror stays intact.
	exec(t, db, "DROP TABLE reader_store_changes")
	exec(t, db, "DROP TABLE reader_store_change_cursor")
	exec(t, db, "UPDATE reader_store_version SET version=1")
	for i := 0; i < 2; i++ {
		if err := Install(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	if count(t, db, "reader_store_changes") != 1 || count(t, db, "fn_reader_book") != 1 {
		t.Fatal("upgrade lost/duplicated existing winner")
	}
	var version int
	db.QueryRow("SELECT version FROM reader_store_version").Scan(&version)
	if version != 2 {
		t.Fatal("worker schema not upgraded")
	}
}

func TestWorkerRestartDeliveryRetryAndContiguousCompletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.db")
	db := open(t, path)
	if err := Install(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := New(db)
	stage(t, s, fixture()[0])
	drain(t, s)
	changes, err := s.Changes(ctx, 128)
	if err != nil || len(changes) != 1 {
		t.Fatal(err)
	}
	if err := s.CompleteChange(ctx, changes[0].Seq+1); err == nil {
		t.Fatal("skipped unprocessed work")
	}
	exec(t, db, "CREATE TABLE scheduled(seq INTEGER PRIMARY KEY, key TEXT)")
	exec(t, db, "CREATE TRIGGER fail_checkpoint BEFORE UPDATE ON reader_store_change_cursor BEGIN SELECT RAISE(ABORT,'checkpoint failure'); END")
	notified := make(chan struct{}, 1)
	process := func(c context.Context, change Change) error {
		_, err := db.ExecContext(c, "INSERT OR IGNORE INTO scheduled VALUES(?,?)", change.Seq, change.Key.ID)
		signal(notified)
		return err
	}
	o := workerOptions()
	o.RetryDelay = time.Second
	stop := startWorker(t, newWorker(t, s, process, o))
	select {
	case <-notified:
	case <-time.After(3 * time.Second):
		t.Fatal("no delivery")
	}
	stop()
	if count(t, db, "scheduled") != 1 {
		t.Fatal("consumer did not schedule")
	}
	var cursor int
	db.QueryRow("SELECT seq FROM reader_store_change_cursor").Scan(&cursor)
	if cursor != 0 {
		t.Fatal("failed checkpoint advanced")
	}
	exec(t, db, "DROP TRIGGER fail_checkpoint")
	db.Close()
	db = open(t, path)
	s = New(db)
	stop = startWorker(t, newWorker(t, s, process, workerOptions()))
	eventually(t, func() bool { changes, err := s.Changes(ctx, 128); return err == nil && len(changes) == 0 })
	stop()
	if count(t, db, "scheduled") != 1 {
		t.Fatal("retry duplicated idempotent job")
	}
	stage(t, s, raw("reader_book_title", bookID, 6, map[string]any{"title": "new"}))
	drain(t, s)
	if err := s.CompleteChange(ctx, changes[0].Seq); err != nil {
		t.Fatal(err)
	}
	remaining, err := s.Changes(ctx, 128)
	if err != nil || len(remaining) != 1 || remaining[0].Seq <= changes[0].Seq {
		t.Fatal("old completion cleared new job")
	}
}

func TestWorkerSlowConsumerDoesNotBlockMaterializationOrHoldWriter(t *testing.T) {
	db, s := installed(t)
	db.SetMaxOpenConns(1)
	stage(t, s, fixture()[0])
	entered := make(chan struct{}, 1)
	w := newWorker(t, s, func(c context.Context, change Change) error { signal(entered); <-c.Done(); return c.Err() }, workerOptions())
	stop := startWorker(t, w)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("consumer not started")
	}
	if err := w.Run(ctx); err == nil {
		t.Fatal("duplicate worker owner started")
	}
	stage(t, s, raw("reader_book_title", bookID, 6, map[string]any{"title": "does not wait"}))
	w.Wake()
	eventually(t, func() bool { return count(t, db, "fn_reader_book_title") == 1 })
	stop()
	changes, err := s.Changes(ctx, 128)
	if err != nil || len(changes) != 2 {
		t.Fatal("canceled delivery discarded jobs")
	}
}

func TestWorkerRetriesFailedPageWithoutBusyLoop(t *testing.T) {
	db, s := installed(t)
	stage(t, s, fixture()[0])
	exec(t, db, "CREATE TRIGGER fail BEFORE INSERT ON fn_reader_book BEGIN SELECT RAISE(ABORT,'busy fixture'); END")
	var failures atomic.Int64
	o := workerOptions()
	o.OnError = func(error) { failures.Add(1) }
	w := newWorker(t, s, nil, o)
	stop := startWorker(t, w)
	eventually(t, func() bool { return failures.Load() > 0 })
	for i := 0; i < 100; i++ {
		w.Wake()
	}
	time.Sleep(60 * time.Millisecond)
	if failures.Load() > 5 || count(t, db, "reader_store_changes") != 0 {
		t.Fatal("wake storm bypassed backoff or failed page queued job")
	}
	exec(t, db, "DROP TRIGGER fail")
	eventually(t, func() bool { return count(t, db, "fn_reader_book") == 1 })
	stop()
	if count(t, db, "reader_store_changes") != 1 {
		t.Fatal("retry lost/duplicated durable change")
	}
}
