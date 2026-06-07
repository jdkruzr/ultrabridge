package notedb

import (
	"context"
	"sync"
	"testing"
)

// TestOpen_InMemoryConcurrentQueriesSeeSchema guards the in-memory case: a
// :memory: database is per-connection, so a multi-connection pool would hand
// concurrent queries their own empty databases ("no such table"). Open must keep
// in-memory DSNs coherent (single connection).
func TestOpen_InMemoryConcurrentQueriesSeeSchema(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			var c int
			errs[idx] = db.QueryRowContext(ctx, "SELECT count(*) FROM notes").Scan(&c)
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("query %d failed (in-memory schema not visible across pool): %v", i, e)
		}
	}
}
