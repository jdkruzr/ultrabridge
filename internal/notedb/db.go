package notedb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Open opens (or creates) the SQLite database at path, applies schema migrations,
// and returns the connection pool. Enables WAL mode, foreign keys, and a
// busy_timeout so lock contention waits rather than failing instantly.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("notedb open: %w", err)
	}
	// On-disk WAL serves many concurrent readers alongside a single writer, so a
	// real pool keeps interactive reads from serializing behind background writes
	// (the cause of folder-browse timeouts). busy_timeout (DSN) makes the rare
	// write-write collision wait rather than erroring; ConnMaxIdleTime reaps idle
	// readers so they cannot pin old WAL snapshots and starve checkpoints.
	//
	// In-memory databases are per-connection: a pool would hand each connection its
	// own empty database. Pin them to one connection so the schema stays coherent.
	if isInMemory(path) {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(8)
		db.SetMaxIdleConns(4)
		db.SetConnMaxIdleTime(5 * time.Minute)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("notedb ping: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("notedb migrate: %w", err)
	}
	return db, nil
}

// isInMemory reports whether path designates a SQLite in-memory database, which
// is per-connection and therefore cannot be safely pooled across connections.
func isInMemory(path string) bool {
	return strings.Contains(path, ":memory:") || strings.Contains(path, "mode=memory")
}
