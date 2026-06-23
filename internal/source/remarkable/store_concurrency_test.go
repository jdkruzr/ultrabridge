package remarkable

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// concurrentDB mirrors production's notedb pragmas (WAL + busy_timeout) and,
// crucially, allows more than one open connection. testDB pins MaxOpenConns(1),
// which serializes everything and so cannot exercise the write-write contention
// that putBlob must survive.
func concurrentDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "remarkable.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(8)
	if err := migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestPutBlobConcurrentDistinct hammers putBlob from many goroutines writing
// distinct blobs at once. Before the write-first fix this deadlocked on the
// read->write lock upgrade and surfaced as SQLITE_BUSY 500s; it must now all
// succeed.
func TestPutBlobConcurrentDistinct(t *testing.T) {
	st := newStore(concurrentDB(t), t.TempDir())
	if err := st.ensurePaths(); err != nil {
		t.Fatalf("ensurePaths: %v", err)
	}

	const workers, perWorker = 8, 40
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				id := fmt.Sprintf("blob-%d-%d", w, i)
				if _, err := st.putBlob(context.Background(), id, strings.NewReader(id), 0); err != nil {
					errs <- fmt.Errorf("%s: %w", id, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent putBlob failed: %v", err)
	}
}

// TestPutBlobConcurrentSameBlob writes the same blob from many goroutines. Every
// write must succeed and the generation must advance once per write with no
// gaps or duplicates, proving the atomic upsert increments correctly under
// contention.
func TestPutBlobConcurrentSameBlob(t *testing.T) {
	st := newStore(concurrentDB(t), t.TempDir())
	if err := st.ensurePaths(); err != nil {
		t.Fatalf("ensurePaths: %v", err)
	}

	const writes = 200
	var wg sync.WaitGroup
	gens := make(chan int64, writes)
	for i := 0; i < writes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			gen, err := st.putBlob(context.Background(), "shared", strings.NewReader(fmt.Sprint(i)), 0)
			if err != nil {
				t.Errorf("putBlob: %v", err)
				return
			}
			gens <- gen
		}(i)
	}
	wg.Wait()
	close(gens)

	seen := map[int64]bool{}
	var max int64
	for g := range gens {
		if seen[g] {
			t.Fatalf("duplicate generation %d handed out", g)
		}
		seen[g] = true
		if g > max {
			max = g
		}
	}
	if max != writes {
		t.Fatalf("final generation = %d, want %d (one per write)", max, writes)
	}
}
