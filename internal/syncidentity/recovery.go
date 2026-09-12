package syncidentity

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/sysop/ultrabridge/internal/syncstore"
	"strings"
)

// FenceRestore runs inside the offline host's restore transaction, before any
// listener/worker can run. Historical authorship is never rewritten. These are
// replica reservations for one person's devices, not a multi-user permission model.
func FenceRestore(ctx context.Context, tx *sql.Tx, newSite string) error {
	if !syncstore.IsULID(newSite) || newSite[0] > '7' {
		return ErrInvalid
	}
	var oldSite string
	if err := tx.QueryRowContext(ctx, `SELECT site_id FROM sync_site WHERE id=1`).Scan(&oldSite); err != nil {
		return err
	}
	known, err := knownAuthor(ctx, tx, newSite)
	if err != nil {
		return err
	}
	if known || newSite == oldSite {
		return ErrConflict
	}
	var reserved int
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_retired_replica WHERE site_id=? UNION ALL SELECT 1 FROM sync_device_identity WHERE site_id=?)`, newSite, newSite).Scan(&reserved); err != nil {
		return err
	}
	if reserved != 0 {
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO sync_retired_replica(site_id) SELECT site_id FROM sync_device_identity UNION SELECT site_id FROM sync_ops UNION SELECT site_id FROM sync_cursors UNION SELECT site_id FROM sync_site`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.name FROM sqlite_master m JOIN pragma_table_info(m.name) p WHERE m.type='table' AND ((m.name GLOB 'fn_*' AND p.name='lww_site_id') OR (m.name='reader_store_incoming' AND p.name='site_id'))`)
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, table := range tables {
		column := "lww_site_id"
		if table == "reader_store_incoming" {
			column = "site_id"
		}
		quoted := `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO sync_retired_replica(site_id) SELECT `+column+` FROM `+quoted+` WHERE `+column+` IS NOT NULL`); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sync_device_identity SET revoked=1`); err != nil {
		return err
	}
	// Keep the old clock floor, and include surviving mirror versions even when
	// their relay rows have been compacted. Do not reuse UB's old operation space.
	var clock int64
	if err = tx.QueryRowContext(ctx, `SELECT MAX(last_hlc,(SELECT COALESCE(MAX(wall_ts),0) FROM sync_ops)) FROM sync_site WHERE id=1`).Scan(&clock); err != nil {
		return err
	}
	for _, table := range tables {
		if table == "reader_store_incoming" {
			continue
		} // receipt timestamps remain in sync_ops
		var column string
		if err = tx.QueryRowContext(ctx, `SELECT name FROM pragma_table_info(?) WHERE name IN ('lww_wall_ts','lww_op_ts','op_ts') LIMIT 1`, table).Scan(&column); err != nil {
			return err
		}
		var floor int64
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(`+column+`),0) FROM "`+strings.ReplaceAll(table, `"`, `""`)+`"`).Scan(&floor); err != nil {
			return fmt.Errorf("restore clock floor: %w", err)
		}
		if floor > clock {
			clock = floor
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE sync_site SET site_id=?,last_op_seq=0,last_hlc=? WHERE id=1`, newSite, clock)
	return err
}
