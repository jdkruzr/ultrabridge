package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/jdkruzr/rhizome/server-go/assets"
)

// Test harness only. VACUUM INTO takes a consistent SQLite snapshot including
// committed WAL data. Never copy a live DB file or overwrite an existing target.
func backupFixture(ctx context.Context, source *sql.DB, path string, metadataOnly bool) (map[string]any, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	if _, err = source.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return nil, err
	}
	copy, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	defer copy.Close()
	copy.SetMaxOpenConns(1)
	if metadataOnly {
		tx, err := copy.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		// These deletes target ONLY the newly created fixture snapshot, never source.
		if _, err = tx.ExecContext(ctx, "DELETE FROM rhizome_asset_chunk"); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM rhizome_asset"); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
	}
	return inventoryFixture(ctx, copy)
}

func quoteIdentifier(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// Hash every persisted table and its definition, without emitting book contents.
// One read transaction makes the cross-table inventory coherent while UB runs.
func inventoryFixture(ctx context.Context, db *sql.DB) (map[string]any, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var integrity string
	if err = tx.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return nil, err
	}
	type table struct{ name, ddl string }
	var tables []table
	rs, err := tx.QueryContext(ctx, "SELECT name,sql FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, err
	}
	for rs.Next() {
		var t table
		if err = rs.Scan(&t.name, &t.ddl); err != nil {
			rs.Close()
			return nil, err
		}
		tables = append(tables, t)
	}
	if err = rs.Err(); err != nil {
		rs.Close()
		return nil, err
	}
	rs.Close()
	hashes := map[string]string{}
	for _, t := range tables {
		rows, err := tx.QueryContext(ctx, "SELECT * FROM "+quoteIdentifier(t.name)+" LIMIT 0")
		if err != nil {
			return nil, err
		}
		columns, err := rows.Columns()
		rows.Close()
		if err != nil {
			return nil, err
		}
		order := make([]string, len(columns))
		for i, c := range columns {
			order[i] = quoteIdentifier(c)
		}
		rows, err = tx.QueryContext(ctx, "SELECT * FROM "+quoteIdentifier(t.name)+" ORDER BY "+strings.Join(order, ","))
		if err != nil {
			return nil, err
		}
		hash := sha256.New()
		fmt.Fprintln(hash, t.ddl)
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err = rows.Scan(pointers...); err != nil {
				rows.Close()
				return nil, err
			}
			if err = json.NewEncoder(hash).Encode(values); err != nil {
				rows.Close()
				return nil, err
			}
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		hashes[t.name] = hex.EncodeToString(hash.Sum(nil))
	}
	// Verify roots and lengths from that SAME snapshot, not merely stored ready flags.
	rs, err = tx.QueryContext(ctx, "SELECT asset_id,byte_length FROM fn_reader_book ORDER BY id")
	if err != nil {
		return nil, err
	}
	type book struct {
		id   string
		size int64
	}
	var books []book
	for rs.Next() {
		var b book
		if err = rs.Scan(&b.id, &b.size); err != nil {
			rs.Close()
			return nil, err
		}
		books = append(books, b)
	}
	if err = rs.Err(); err != nil {
		rs.Close()
		return nil, err
	}
	rs.Close()
	verified := 0
	for _, b := range books {
		var state string
		var length int64
		err = tx.QueryRowContext(ctx, "SELECT state,byte_length FROM rhizome_asset WHERE asset_id=?", b.id).Scan(&state, &length)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		if state != "ready" || length != b.size {
			continue
		}
		rows, err := tx.QueryContext(ctx, "SELECT chunk_index,sha256,bytes FROM rhizome_asset_chunk WHERE asset_id=? ORDER BY chunk_index", b.id)
		if err != nil {
			return nil, err
		}
		root := sha256.New()
		var index, total int64
		valid := true
		for rows.Next() {
			var i int64
			var digest string
			var bytes []byte
			if err = rows.Scan(&i, &digest, &bytes); err != nil {
				rows.Close()
				return nil, err
			}
			want := min(int64(assets.ChunkBytes), b.size-total)
			sum := sha256.Sum256(bytes)
			if i != index || int64(len(bytes)) != want || hex.EncodeToString(sum[:]) != digest {
				valid = false
			}
			root.Write(bytes)
			total += int64(len(bytes))
			index++
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		if valid && total == b.size && hex.EncodeToString(root.Sum(nil)) == b.id {
			verified++
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"integrity": integrity, "tables": hashes, "books": len(books), "verified_books": verified, "backup_complete": len(books) == verified}, nil
}
