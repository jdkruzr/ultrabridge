package main

import (
	"context"
	"database/sql"
	"github.com/sysop/ultrabridge/internal/readerlab"
)

func inspectFixture(ctx context.Context, db *sql.DB) (map[string]any, error) {
	projection, err := readerlab.ProjectFixture(ctx, db, "n")
	if err != nil {
		return nil, err
	}
	result := map[string]any{"projection": projection}
	var integrity string
	if err = db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return nil, err
	}
	result["integrity"] = integrity
	for name, query := range map[string]string{
		"notebooks":       "SELECT count(*) FROM fn_notebook",
		"pages":           "SELECT count(*) FROM fn_page",
		"strokes":         "SELECT count(*) FROM fn_stroke",
		"assets":          "SELECT count(*) FROM rhizome_asset WHERE state='ready'",
		"pending":         "SELECT count(*) FROM reader_store_incoming WHERE state='pending'",
		"quarantined":     "SELECT count(*) FROM reader_store_incoming WHERE state='quarantined'",
		"duplicate_ops":   "SELECT count(*) FROM (SELECT site_id,op_seq FROM sync_ops GROUP BY site_id,op_seq HAVING count(*)>1)",
		"local_table_ops": "SELECT count(*) FROM sync_ops WHERE table_name IN ('reader_local_preferences','reader_command','rhizome_transfer_job','rhizome_transfer_schedule')",
	} {
		var n int64
		if err = db.QueryRowContext(ctx, query).Scan(&n); err != nil {
			return nil, err
		}
		result[name] = n
	}
	return result, nil
}
