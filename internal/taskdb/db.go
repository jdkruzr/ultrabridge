package taskdb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Open opens (or creates) the SQLite task database at path, applies schema
// migrations, and returns the connection pool. Enables WAL mode, foreign keys,
// and a busy_timeout so lock contention waits rather than failing instantly.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("taskdb open: %w", err)
	}
	// On-disk: pool reads alongside the single writer (busy_timeout handles the
	// rare write-write collision; ConnMaxIdleTime avoids WAL checkpoint starvation).
	// In-memory DBs are per-connection, so pin them to a single connection.
	if isInMemory(path) {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(8)
		db.SetMaxIdleConns(4)
		db.SetConnMaxIdleTime(5 * time.Minute)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("taskdb ping: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("taskdb migrate: %w", err)
	}
	return db, nil
}

// isInMemory reports whether path designates a SQLite in-memory database, which
// is per-connection and therefore cannot be safely pooled across connections.
func isInMemory(path string) bool {
	return strings.Contains(path, ":memory:") || strings.Contains(path, "mode=memory")
}
