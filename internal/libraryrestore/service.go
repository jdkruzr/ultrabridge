package libraryrestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jdkruzr/rhizome/server-go/assets"
	"github.com/sysop/ultrabridge/internal/readercontract"
	"github.com/sysop/ultrabridge/internal/syncgeneration"
	"github.com/sysop/ultrabridge/internal/syncstore"
)

// Exclusive MUST stop and join every library materializer/search/authoring worker,
// execute publish, then restart them against the committed state (even on failure).
// Normal HTTP row/asset writers serialize and recheck through syncgeneration.
type Exclusive func(context.Context, func() error) error
type Service struct {
	DB        *sql.DB
	StageRoot string
	Exclusive Exclusive
	// Host-owned derived state only, in the SAME transaction as replacement.
	// No external I/O or cache mutation: a failure must roll back everything.
	ReplaceDerived func(context.Context, *sql.Tx) error
	mu             sync.Mutex
}
type Baseline struct {
	Generation string `json:"generation"`
	Snapshot   string `json:"snapshot"`
	Publisher  string `json:"publisher"`
	HighWater  int64  `json:"high_water"`
	Cursor     int64  `json:"cursor"`
}

func Install(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS sync_restore_baseline(id INTEGER PRIMARY KEY CHECK(id=1),generation TEXT NOT NULL,snapshot TEXT NOT NULL,publisher TEXT NOT NULL,high_water INTEGER NOT NULL,cursor INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS sync_restore_adoption(old_site TEXT NOT NULL,generation TEXT NOT NULL,new_site TEXT NOT NULL UNIQUE,token_hash TEXT NOT NULL UNIQUE,snapshot TEXT NOT NULL,PRIMARY KEY(old_site,generation))`,
	} {
		if _, err := tx.ExecContext(ctx, ddl); err != nil {
			return err
		}
		expected := strings.Replace(ddl, " IF NOT EXISTS", "", 1)
		name := strings.Split(strings.TrimPrefix(expected, "CREATE TABLE "), "(")[0]
		var actual string
		if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&actual); err != nil {
			return err
		}
		if strings.Join(strings.Fields(actual), " ") != strings.Join(strings.Fields(expected), " ") {
			return errors.New("invalid_restore_schema")
		}
	}
	return tx.Commit()
}

func (s *Service) Baseline(ctx context.Context) (Baseline, error) {
	var b Baseline
	err := s.DB.QueryRowContext(ctx, `SELECT b.generation,b.snapshot,b.publisher,b.high_water,b.cursor FROM sync_restore_baseline b JOIN sync_library_generation g ON g.id=1 AND b.generation=g.generation WHERE b.id=1`).Scan(&b.Generation, &b.Snapshot, &b.Publisher, &b.HighWater, &b.Cursor)
	return b, err
}

// Receipt is a read-only lost-response check using the publisher's own device
// key. It never grants a device key permission to initiate a replacement.
func (s *Service) Receipt(ctx context.Context, publisher, id string) (Baseline, error) {
	if !assets.ValidID(id) {
		return Baseline{}, syncgeneration.ErrInvalid
	}
	var generation string
	if err := s.DB.QueryRowContext(ctx, `SELECT generation FROM sync_library_replacement WHERE request_id=? AND publisher=?`, id, publisher).Scan(&generation); err != nil {
		return Baseline{}, err
	}
	b, err := s.Baseline(ctx)
	if err != nil {
		return Baseline{}, err
	}
	if b.Generation != generation {
		return Baseline{}, syncgeneration.ErrConflict
	}
	return b, nil
}

func (s *Service) Publish(ctx context.Context, r syncgeneration.Request) (Baseline, error) {
	if !assets.ValidID(r.ID) || !assets.ValidID(r.Expected) || !assets.ValidID(r.SnapshotHash) || !syncstore.IsULID(r.Publisher) || r.Publisher[0] > '7' {
		return Baseline{}, syncgeneration.ErrInvalid
	}
	if s.Exclusive == nil {
		return Baseline{}, errors.New("restore_worker_boundary_required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Resolve lost replies before needing the staged bytes or taking down workers.
	var oldExpected, oldHash, oldPublisher, oldGeneration string
	err := s.DB.QueryRowContext(ctx, `SELECT expected_generation,snapshot_hash,publisher,generation FROM sync_library_replacement WHERE request_id=?`, r.ID).Scan(&oldExpected, &oldHash, &oldPublisher, &oldGeneration)
	if err == nil {
		b, e := s.Baseline(ctx)
		if e != nil || oldExpected != r.Expected || oldHash != r.SnapshotHash || oldPublisher != r.Publisher || b.Generation != oldGeneration {
			return Baseline{}, syncgeneration.ErrConflict
		}
		return b, nil
	}
	if err != sql.ErrNoRows {
		return Baseline{}, err
	}
	active, err := syncgeneration.Current(ctx, s.DB)
	if err != nil {
		return Baseline{}, err
	}
	if active != r.Expected {
		return Baseline{}, syncgeneration.ErrConflict
	}
	p, err := prepare(ctx, &assets.SQLStore{DB: s.DB}, r.SnapshotHash, s.StageRoot)
	if err != nil {
		return Baseline{}, err
	}
	defer p.close()
	if p.manifest.Publisher != r.Publisher {
		return Baseline{}, ErrSnapshot
	}
	err = s.Exclusive(ctx, func() error {
		_, e := syncgeneration.PublishWithGeneration(ctx, s.DB, r, func(ctx context.Context, tx *sql.Tx, next string) error {
			if s.ReplaceDerived != nil {
				if err := s.ReplaceDerived(ctx, tx); err != nil {
					return err
				}
			}
			// Explicit allowlist: no settings, sources, credentials, tasks, or other UB tables.
			for _, def := range readercontract.CandidateCombined().Tables {
				if e := replaceTable(ctx, tx, p.db, "fn_"+def.Name); e != nil {
					return e
				}
			}
			for _, table := range []string{"sync_ops", "sync_seq", "reader_store_incoming", "reader_store_changes", "reader_store_change_cursor"} {
				if e := replaceTable(ctx, tx, p.db, table); e != nil {
					return e
				}
			}
			for _, q := range []string{
				`DELETE FROM sync_cursors`,
				`DELETE FROM reader_search_fts`, `DELETE FROM reader_search_documents`, `DELETE FROM reader_search_jobs`,
				`INSERT INTO reader_search_jobs(seq,table_name,pk) SELECT seq,table_name,pk FROM reader_store_changes`,
				`UPDATE sync_site SET last_hlc=MAX(last_hlc,COALESCE((SELECT MAX(wall_ts) FROM sync_ops),0)),last_op_seq=MAX(last_op_seq,COALESCE((SELECT MAX(op_seq) FROM sync_ops WHERE site_id=sync_site.site_id),0)) WHERE id=1`,
			} {
				if _, e := tx.ExecContext(ctx, q); e != nil {
					return e
				}
			}
			if _, e := tx.ExecContext(ctx, `INSERT INTO sync_cursors(site_id,last_pull_seq,acked_op_seq,updated_at) VALUES(?,?,?,?)`, r.Publisher, p.seq, p.manifest.HighWater, time.Now().UnixMilli()); e != nil {
				return e
			}
			// Immutable content-addressed bytes may be retained unreferenced; they are
			// not library rows. Replace included bytes (also repairs an invalid cache).
			if _, e := tx.ExecContext(ctx, `CREATE TEMP TABLE restore_asset_ids(id TEXT PRIMARY KEY)`); e != nil {
				return e
			}
			defer tx.ExecContext(context.Background(), `DROP TABLE IF EXISTS temp.restore_asset_ids`)
			rows, e := p.db.QueryContext(ctx, `SELECT asset_id FROM rhizome_asset`)
			if e != nil {
				return e
			}
			for rows.Next() {
				var id string
				if e = rows.Scan(&id); e != nil {
					break
				}
				if _, e = tx.ExecContext(ctx, `INSERT INTO temp.restore_asset_ids VALUES(?)`, id); e != nil {
					break
				}
			}
			if e == nil {
				e = rows.Err()
			}
			rows.Close()
			if e != nil {
				return e
			}
			for _, table := range []string{"rhizome_asset_chunk", "rhizome_asset"} {
				if _, e = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE asset_id IN (SELECT id FROM temp.restore_asset_ids)"); e != nil {
					return e
				}
			}
			for _, table := range []string{"rhizome_asset", "rhizome_asset_chunk"} {
				if e = copyTable(ctx, tx, p.db, table); e != nil {
					return e
				}
			}
			_, e = tx.ExecContext(ctx, `INSERT OR REPLACE INTO sync_restore_baseline VALUES(1,?,?,?,?,?)`, next, r.SnapshotHash, r.Publisher, p.manifest.HighWater, p.seq)
			return e
		})
		if e != nil {
			return e
		}
		return nil
	})
	if err != nil {
		return Baseline{}, err
	}
	return s.Baseline(ctx)
}

func replaceTable(ctx context.Context, tx *sql.Tx, source *sql.DB, table string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
		return err
	}
	return copyTable(ctx, tx, source, table)
}
func copyTable(ctx context.Context, tx *sql.Tx, source *sql.DB, table string) error {
	rows, err := source.QueryContext(ctx, "SELECT * FROM "+table)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO "+table+"("+strings.Join(cols, ",")+") VALUES("+strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")+")")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for rows.Next() {
		v := make([]any, len(cols))
		ptr := make([]any, len(cols))
		for i := range v {
			ptr[i] = &v[i]
		}
		if err = rows.Scan(ptr...); err != nil {
			return err
		}
		if _, err = stmt.ExecContext(ctx, v...); err != nil {
			return err
		}
	}
	return rows.Err()
}

type Adoption struct {
	Generation string `json:"generation"`
	Snapshot   string `json:"snapshot"`
	Site       string `json:"site_id"`
	TokenHash  string `json:"token_hash"`
}

// Adopt is called only with an authenticated previous device key. The native host
// must durably install/validate the baseline and save its fresh key BEFORE this ACK.
// Old keys remain read/adoption-only so a lost response can be retried safely.
func (s *Service) Adopt(ctx context.Context, old string, a Adoption) (Baseline, error) {
	if !syncstore.IsULID(a.Site) || a.Site[0] > '7' || !assets.ValidID(a.TokenHash) || !assets.ValidID(a.Generation) || !assets.ValidID(a.Snapshot) {
		return Baseline{}, ErrSnapshot
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Baseline{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE sync_library_generation SET generation=generation WHERE id=1`); err != nil {
		return Baseline{}, err
	}
	var b Baseline
	err = tx.QueryRowContext(ctx, `SELECT b.generation,b.snapshot,b.publisher,b.high_water,b.cursor FROM sync_restore_baseline b JOIN sync_library_generation g ON b.generation=g.generation WHERE b.id=1`).Scan(&b.Generation, &b.Snapshot, &b.Publisher, &b.HighWater, &b.Cursor)
	if err != nil {
		return Baseline{}, err
	}
	if b.Generation != a.Generation || b.Snapshot != a.Snapshot {
		return Baseline{}, syncgeneration.ErrConflict
	}
	var bound string
	var revoked int
	if err = tx.QueryRowContext(ctx, `SELECT g.generation,i.revoked FROM sync_device_identity i JOIN sync_device_generation g USING(site_id) WHERE site_id=?`, old).Scan(&bound, &revoked); err != nil {
		return Baseline{}, err
	}
	if revoked != 0 || bound == b.Generation {
		return Baseline{}, syncgeneration.ErrConflict
	}
	var site, hash string
	err = tx.QueryRowContext(ctx, `SELECT new_site,token_hash FROM sync_restore_adoption WHERE old_site=? AND generation=?`, old, b.Generation).Scan(&site, &hash)
	if err == nil {
		if site != a.Site || hash != a.TokenHash {
			return Baseline{}, syncgeneration.ErrConflict
		}
		return b, tx.Commit()
	}
	if err != sql.ErrNoRows {
		return Baseline{}, err
	}
	var used int
	checks := []string{`SELECT count(*) FROM sync_device_identity WHERE site_id=? OR token_hash=?`, `SELECT count(*) FROM sync_retired_replica WHERE site_id=?`, `SELECT count(*) FROM sync_site WHERE site_id=?`, `SELECT count(*) FROM sync_ops WHERE site_id=?`}
	for _, q := range checks {
		args := []any{a.Site}
		if strings.Contains(q, "token_hash") {
			args = append(args, a.TokenHash)
		}
		if err = tx.QueryRowContext(ctx, q, args...).Scan(&used); err != nil {
			return Baseline{}, err
		}
		if used != 0 {
			return Baseline{}, syncgeneration.ErrConflict
		}
	}
	for _, def := range readercontract.CandidateCombined().Tables {
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM fn_"+def.Name+" WHERE lww_site_id=?", a.Site).Scan(&used); err != nil {
			return Baseline{}, err
		}
		if used != 0 {
			return Baseline{}, syncgeneration.ErrConflict
		}
	}
	for _, stmt := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO sync_device_identity(site_id,token_hash,created_at) VALUES(?,?,?)`, []any{a.Site, a.TokenHash, time.Now().UnixMilli()}},
		{`INSERT INTO sync_device_generation VALUES(?,?)`, []any{a.Site, b.Generation}},
		{`INSERT INTO sync_cursors(site_id,last_pull_seq,acked_op_seq,updated_at) VALUES(?,?,0,?)`, []any{a.Site, b.Cursor, time.Now().UnixMilli()}},
		{`INSERT INTO sync_restore_adoption VALUES(?,?,?,?,?)`, []any{old, b.Generation, a.Site, a.TokenHash, b.Snapshot}},
	} {
		if _, err = tx.ExecContext(ctx, stmt.q, stmt.args...); err != nil {
			return Baseline{}, fmt.Errorf("adoption: %w", err)
		}
	}
	return b, tx.Commit()
}
