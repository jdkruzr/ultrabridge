package notedb

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestOpen_ConcurrentReadNotBlockedByHeldWrite guards the outage where a single
// shared connection (MaxOpenConns=1) made interactive reads serialize behind
// background writes: a read waited for the writer's connection and blew its
// request deadline. With a real read pool, a read must proceed while a write
// transaction is in flight.
func TestOpen_ConcurrentReadNotBlockedByHeldWrite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, "CREATE TABLE scratch (x INTEGER)"); err != nil {
		t.Fatalf("create scratch: %v", err)
	}

	writeHeld := make(chan struct{})
	releaseWrite := make(chan struct{})
	writeDone := make(chan struct{})

	go func() {
		defer close(writeDone)
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Errorf("begin write tx: %v", err)
			return
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO scratch (x) VALUES (1)"); err != nil {
			t.Errorf("write insert: %v", err)
			_ = tx.Rollback()
			return
		}
		close(writeHeld) // write transaction (and its connection) now held
		<-releaseWrite   // keep holding until the reader has had its chance
		_ = tx.Commit()
	}()

	<-writeHeld // ensure the write is in flight before we read

	readCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	var n int
	readErr := db.QueryRowContext(readCtx, "SELECT count(*) FROM scratch").Scan(&n)

	close(releaseWrite) // let the writer commit regardless of the read result
	<-writeDone

	if readErr != nil {
		t.Fatalf("concurrent read was blocked by an in-flight write (got %v); reads must not serialize behind writes", readErr)
	}
}
