// Package readersearch is the candidate annotation search consumer. It is not
// wired into production search, OCR, embeddings, sync authorities or migrations.
package readersearch

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
)

func Install(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS reader_search_version(id INTEGER PRIMARY KEY CHECK(id=1),version INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS reader_search_jobs(seq INTEGER PRIMARY KEY,table_name TEXT NOT NULL,pk TEXT NOT NULL,after_id TEXT NOT NULL DEFAULT '',done INTEGER NOT NULL DEFAULT 0,retry_at INTEGER NOT NULL DEFAULT 0,error_code TEXT NOT NULL DEFAULT '')`,
		`CREATE INDEX IF NOT EXISTS reader_search_pending ON reader_search_jobs(done,retry_at,seq)`,
		`CREATE TABLE IF NOT EXISTS reader_search_documents(id INTEGER PRIMARY KEY,annotation_id TEXT NOT NULL UNIQUE,book_id TEXT NOT NULL,title TEXT NOT NULL,quote TEXT NOT NULL,recognized_text TEXT NOT NULL,anchor TEXT NOT NULL,input_hash TEXT NOT NULL,alternatives TEXT NOT NULL,revision INTEGER NOT NULL)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS reader_search_fts USING fts5(title,quote,recognized_text)`,
		`CREATE INDEX IF NOT EXISTS reader_search_changes_key ON reader_store_changes(table_name,pk,seq)`,
		`CREATE INDEX IF NOT EXISTS reader_search_recognition_annotation ON fn_reader_recognition(annotation_id,id)`,
	} {
		if _, err = tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	var version int
	err = tx.QueryRowContext(ctx, "SELECT version FROM reader_search_version WHERE id=1").Scan(&version)
	if err == sql.ErrNoRows {
		// Bootstrap once, including changes a previous consumer already acked.
		// Keys only; completed jobs and their progress survive subsequent installs.
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO reader_search_jobs(seq,table_name,pk) SELECT seq,table_name,pk FROM reader_store_changes`); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO reader_search_version VALUES(1,1)"); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if version != 1 {
		return fmt.Errorf("unsupported reader search version %d", version)
	}
	// Fail closed on incompatible derived schemas; never drop/recreate a DB.
	for table, want := range map[string][]string{
		"reader_search_version":   {"id INTEGER 0 1", "version INTEGER 1 0"},
		"reader_search_jobs":      {"seq INTEGER 0 1", "table_name TEXT 1 0", "pk TEXT 1 0", "after_id TEXT 1 0", "done INTEGER 1 0", "retry_at INTEGER 1 0", "error_code TEXT 1 0"},
		"reader_search_documents": {"id INTEGER 0 1", "annotation_id TEXT 1 0", "book_id TEXT 1 0", "title TEXT 1 0", "quote TEXT 1 0", "recognized_text TEXT 1 0", "anchor TEXT 1 0", "input_hash TEXT 1 0", "alternatives TEXT 1 0", "revision INTEGER 1 0"},
	} {
		rows, e := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
		if e != nil {
			return e
		}
		var got []string
		for rows.Next() {
			var cid, nn, pk int
			var name, kind string
			var value any
			if e = rows.Scan(&cid, &name, &kind, &nn, &value, &pk); e != nil {
				break
			}
			got = append(got, fmt.Sprintf("%s %s %d %d", name, strings.ToUpper(kind), nn, pk))
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		if e != nil {
			return e
		}
		if !reflect.DeepEqual(got, want) {
			return fmt.Errorf("incompatible %s", table)
		}
	}
	var ftsDDL string
	if err = tx.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE name='reader_search_fts'").Scan(&ftsDDL); err != nil {
		return err
	}
	if !strings.Contains(strings.ToLower(ftsDDL), "using fts5(") {
		return fmt.Errorf("incompatible reader FTS schema")
	}
	for _, query := range []string{
		"SELECT seq,table_name,pk,after_id,done,retry_at,error_code FROM reader_search_jobs LIMIT 0",
		"SELECT id,annotation_id,book_id,title,quote,recognized_text,anchor,input_hash,alternatives,revision FROM reader_search_documents LIMIT 0",
		"SELECT rowid,title,quote,recognized_text FROM reader_search_fts LIMIT 0",
	} {
		rows, e := tx.QueryContext(ctx, query)
		if e != nil {
			return e
		}
		rows.Close()
	}
	return tx.Commit()
}
