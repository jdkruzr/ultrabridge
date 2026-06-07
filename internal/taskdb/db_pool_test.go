package taskdb

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestOpen_ConcurrentReadNotBlockedByHeldWrite mirrors the notedb guard: the task
// DB must also let reads proceed while a write transaction is in flight.
func TestOpen_ConcurrentReadNotBlockedByHeldWrite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := Open(ctx, filepath.Join(dir, "tasks.db"))
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
		close(writeHeld)
		<-releaseWrite
		_ = tx.Commit()
	}()

	<-writeHeld

	readCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	var n int
	readErr := db.QueryRowContext(readCtx, "SELECT count(*) FROM scratch").Scan(&n)

	close(releaseWrite)
	<-writeDone

	if readErr != nil {
		t.Fatalf("concurrent read was blocked by an in-flight write (got %v); reads must not serialize behind writes", readErr)
	}
}
