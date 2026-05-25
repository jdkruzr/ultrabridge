package digeststore

import (
	"database/sql"
	"fmt"
)

// openTestDB opens a WAL-mode SQLite database at path with foreign keys on,
// mirroring how notedb/taskdb open the shared pool.
func openTestDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
