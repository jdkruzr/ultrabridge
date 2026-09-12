// Package readerstore is the inactive ForestRead notedb adapter. Explicit install
// only; no production router, registry, migration runner or acknowledgements.
package readerstore

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"

	"github.com/jdkruzr/rhizome/server-go/registry"
	"github.com/sysop/ultrabridge/internal/readercontract"
)

var definitions = readercontract.Registry().ByName()

type column struct {
	name, affinity string
	required, pk   int
}

func columns(table string) []column {
	result := []column{{"id", "TEXT", 1, 1}}
	for _, c := range definitions[table].Columns {
		affinity := "INTEGER"
		if c.Type == registry.Text {
			affinity = "TEXT"
		}
		if c.Type == registry.Blob {
			affinity = "BLOB"
		}
		required := 1
		if c.Nullable {
			required = 0
		}
		result = append(result, column{c.Name, affinity, required, 0})
	}
	return append(result, column{"lww_op_ts", "INTEGER", 1, 0}, column{"lww_op_seq", "INTEGER", 1, 0}, column{"lww_site_id", "TEXT", 1, 0})
}
func installTable(ctx context.Context, tx *sql.Tx, name string, cols []column, tail string) error {
	parts := make([]string, 0, len(cols))
	expected := map[string]column{}
	for _, c := range cols {
		s := c.name + " " + c.affinity
		if c.required != 0 {
			s += " NOT NULL"
		}
		if c.pk != 0 {
			s += " PRIMARY KEY"
		}
		parts = append(parts, s)
		expected[c.name] = c
	}
	if tail != "" {
		parts = append(parts, tail)
	}
	if _, err := tx.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+name+"("+strings.Join(parts, ",")+")"); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+name+")")
	if err != nil {
		return err
	}
	actual := map[string]column{}
	for rows.Next() {
		var cid int
		var c column
		var defaultValue any
		if err = rows.Scan(&cid, &c.name, &c.affinity, &c.required, &defaultValue, &c.pk); err != nil {
			rows.Close()
			return err
		}
		c.affinity = strings.ToUpper(c.affinity)
		actual[c.name] = c
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("incompatible %s; original database preserved", name)
	}
	return nil
}

// Install is additive and atomic. It never changes notedb's user_version, existing
// writer/asset tables, sync clocks or accepted hashes, and never deletes/recreates.
func Install(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = installTable(ctx, tx, "reader_store_version", []column{{"id", "INTEGER", 1, 1}, {"version", "INTEGER", 1, 0}}, "CHECK(id=1)"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO reader_store_version VALUES(1,1)"); err != nil {
		return err
	}
	var version int
	if err = tx.QueryRowContext(ctx, "SELECT version FROM reader_store_version WHERE id=1").Scan(&version); err != nil {
		return err
	}
	if version != 1 && version != 2 {
		return fmt.Errorf("unsupported reader store version %d", version)
	}
	for _, t := range readercontract.Registry().Tables {
		if err = installTable(ctx, tx, "fn_"+t.Name, columns(t.Name), ""); err != nil {
			return err
		}
	}
	if err = installTable(ctx, tx, "reader_store_incoming", []column{{"seq", "INTEGER", 1, 1}, {"site_id", "TEXT", 1, 0}, {"op_seq", "INTEGER", 1, 0}, {"payload", "TEXT", 1, 0}, {"state", "TEXT", 1, 0}, {"reason", "TEXT", 1, 0}}, "CHECK(state IN ('pending','applied','quarantined'))"); err != nil {
		return err
	}
	if err = installTable(ctx, tx, "reader_store_changes", []column{{"seq", "INTEGER", 1, 1}, {"table_name", "TEXT", 1, 0}, {"pk", "TEXT", 1, 0}}, ""); err != nil {
		return err
	}
	if err = installTable(ctx, tx, "reader_store_change_cursor", []column{{"id", "INTEGER", 1, 1}, {"seq", "INTEGER", 1, 0}}, "CHECK(id=1)"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO reader_store_change_cursor VALUES(1,0)"); err != nil {
		return err
	}
	if version == 1 {
		// One-time migration of already materialized candidate libraries. SQL
		// copies keys only; no ink is loaded, decoded or re-authored. Retaining
		// the journal (including delivered entries) prevents sequence reuse.
		for _, t := range readercontract.Registry().Tables {
			if _, err = tx.ExecContext(ctx, "INSERT INTO reader_store_changes(table_name,pk) SELECT ?,id FROM fn_"+t.Name, t.Name); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE reader_store_version SET version=2 WHERE id=1"); err != nil {
			return err
		}
	}
	for _, s := range []string{
		"CREATE UNIQUE INDEX IF NOT EXISTS reader_store_incoming_identity ON reader_store_incoming(site_id,op_seq)",
		"CREATE INDEX IF NOT EXISTS reader_store_incoming_pending ON reader_store_incoming(state,seq)",
		"CREATE INDEX IF NOT EXISTS reader_store_annotations_book ON fn_reader_annotation(book_id,id)",
		"CREATE INDEX IF NOT EXISTS reader_store_sessions_annotation ON fn_reader_edit_session(annotation_id,id)",
		"CREATE INDEX IF NOT EXISTS reader_store_strokes_annotation ON fn_reader_stroke(annotation_id,id)",
		"CREATE INDEX IF NOT EXISTS reader_store_claims_session ON fn_reader_erase_claim(session_id,id)",
		"CREATE INDEX IF NOT EXISTS reader_store_claims_stroke ON fn_reader_erase_claim(stroke_id,id)",
		"CREATE INDEX IF NOT EXISTS reader_store_values_session ON fn_reader_annotation_value(session_id,id)",
	} {
		if _, err = tx.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	// IF NOT EXISTS alone would silently accept an unrelated/non-unique index
	// with this name. Receipt identity is a correctness constraint, not an optimization.
	rows, err := tx.QueryContext(ctx, "PRAGMA index_list(reader_store_incoming)")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err = rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			break
		}
		if name == "reader_store_incoming_identity" {
			found = unique == 1 && partial == 0
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("incompatible incoming identity index")
	}
	rows, err = tx.QueryContext(ctx, "PRAGMA index_info(reader_store_incoming_identity)")
	if err != nil {
		return err
	}
	names := []string{}
	for rows.Next() {
		var seq, cid int
		var name string
		if err = rows.Scan(&seq, &cid, &name); err != nil {
			break
		}
		names = append(names, name)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(names, []string{"site_id", "op_seq"}) {
		return fmt.Errorf("incompatible incoming identity index columns")
	}
	return tx.Commit()
}
