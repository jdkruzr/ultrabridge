package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sysop/ultrabridge/internal/syncidentity"
)

type restoreRequest struct {
	Source string `json:"source"`
	ID     string `json:"id"`
	Site   string `json:"site"`
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Only a new, operation-owned directory is used. The sidecar fences startup even
// if SQLite rolls back the restore transaction or the process dies while copying.
func prepareRestore(ctx context.Context, source, dir, id string, gate func(string)) (map[string]any, error) {
	if id == "" {
		return nil, fmt.Errorf("restore attempt ID required")
	}
	source, err := filepath.Abs(source)
	if err != nil {
		return nil, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	fresh := false
	if err = os.Mkdir(dir, 0700); err == nil {
		fresh = true
	} else if !os.IsExist(err) {
		return nil, err
	}
	lock, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	manifest := filepath.Join(dir, "restore-request.json")
	var request restoreRequest
	if fresh {
		// 128 random bits encoded as a syntactically valid ULID replica identifier.
		b := make([]byte, 16)
		if _, err = rand.Read(b); err != nil {
			return nil, err
		}
		n := new(big.Int).SetBytes(b)
		alphabet := "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
		site := make([]byte, 26)
		for i := 25; i >= 0; i-- {
			site[i] = alphabet[new(big.Int).And(n, big.NewInt(31)).Int64()]
			n.Rsh(n, 5)
		}
		request = restoreRequest{source, id, string(site)}
		bytes, _ := json.Marshal(request)
		f, e := os.OpenFile(manifest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return nil, e
		}
		_, err = f.Write(bytes)
		if err == nil {
			err = f.Sync()
		}
		f.Close()
		if err != nil {
			return nil, err
		}
		if err = syncDirectory(dir); err != nil {
			return nil, err
		}
		if err = syncDirectory(filepath.Dir(dir)); err != nil {
			return nil, err
		}
	} else {
		b, e := os.ReadFile(manifest)
		if e != nil {
			return nil, fmt.Errorf("unowned/incomplete restore destination: %w", e)
		}
		if err = json.Unmarshal(b, &request); err != nil {
			return nil, err
		}
		if request.Source != source || request.ID != id {
			return nil, fmt.Errorf("restore request differs from reserved destination")
		}
	}
	gate("restore_reserved")
	target := filepath.Join(dir, "restored.db")
	if _, err = os.Stat(target); os.IsNotExist(err) {
		// A previous incomplete VACUUM is retained as evidence, never overwritten.
		stage := filepath.Join(dir, "snapshot.stage")
		if _, e := os.Stat(stage); e == nil {
			if e = os.Rename(stage, filepath.Join(dir, fmt.Sprintf("incomplete-%s.db", randomSuffix()))); e != nil {
				return nil, e
			}
		}
		u := url.URL{Scheme: "file", Path: source, RawQuery: "mode=ro"}
		src, e := sql.Open("sqlite", u.String())
		if e != nil {
			return nil, e
		}
		_, e = backupFixture(ctx, src, stage, false)
		src.Close()
		if e != nil {
			return nil, e
		}
		gate("restore_snapshot")
		if err = os.Rename(stage, target); err != nil {
			return nil, err
		}
		if err = syncDirectory(dir); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", target)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, "PRAGMA synchronous=FULL"); err != nil {
		return nil, err
	}
	if err = syncidentity.Install(ctx, db); err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS sync_restore_receipt(id INTEGER PRIMARY KEY CHECK(id=1),request_id TEXT NOT NULL,replica_id TEXT NOT NULL)`); err != nil {
		return nil, err
	}
	var completed, replica string
	err = tx.QueryRowContext(ctx, `SELECT request_id,replica_id FROM sync_restore_receipt WHERE id=1`).Scan(&completed, &replica)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if completed != id {
		if err = syncidentity.FenceRestore(ctx, tx, request.Site); err != nil {
			return nil, err
		}
		gate("restore_fenced")
		if _, err = tx.ExecContext(ctx, `INSERT INTO sync_restore_receipt VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET request_id=excluded.request_id,replica_id=excluded.replica_id`, id, request.Site); err != nil {
			return nil, err
		}
	} else if replica != request.Site {
		return nil, fmt.Errorf("restore receipt identity mismatch")
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	gate("restore_committed")
	return map[string]any{"ready": true, "path": target, "replica": request.Site, "reconciliation": "snapshot_baseline_only"}, nil
}

func randomSuffix() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b)
}

// An explicit restore directory cannot be served as an unfenced normal fixture.
func validateRestoreStartup(path string, enrolled bool) error {
	manifest := filepath.Join(filepath.Dir(path), "restore-request.json")
	b, err := os.ReadFile(manifest)
	if os.IsNotExist(err) {
		if _, e := os.Stat(path); os.IsNotExist(e) {
			return nil
		}
		// The durable DB receipt also protects a prepared target moved elsewhere.
		u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
		db, e := sql.Open("sqlite", u.String())
		if e != nil {
			return e
		}
		defer db.Close()
		var n int
		e = db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='sync_restore_receipt'`).Scan(&n)
		if e != nil {
			return e
		}
		if n != 0 {
			var id, site string
			if e = db.QueryRow(`SELECT request_id,replica_id FROM sync_restore_receipt WHERE id=1`).Scan(&id, &site); e != nil || id == "" || site == "" || !enrolled {
				return fmt.Errorf("restored target requires completed preparation and enrolled admission")
			}
		}
		return nil
	}
	if err != nil {
		return err
	}
	var request restoreRequest
	if err = json.Unmarshal(b, &request); err != nil {
		return err
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	defer db.Close()
	var id, site string
	if err = db.QueryRow(`SELECT request_id,replica_id FROM sync_restore_receipt WHERE id=1`).Scan(&id, &site); err != nil {
		return fmt.Errorf("restore preparation incomplete")
	}
	if !enrolled || id != request.ID || site != request.Site {
		return fmt.Errorf("restore fence or enrolled admission missing")
	}
	return nil
}

func restoreGate(selected string) func(string) {
	return func(name string) {
		if selected == name {
			fmt.Printf("{\"checkpoint\":%q}\n", name)
			var line string
			fmt.Scanln(&line)
			if strings.TrimSpace(line) != "resume" {
				os.Exit(2)
			}
		}
	}
}
