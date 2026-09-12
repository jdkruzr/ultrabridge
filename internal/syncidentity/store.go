// Package syncidentity owns candidate device credentials, not Rhizome row state.
// It is explicitly installed by the disposable harness only; production is inert.
package syncidentity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/sysop/ultrabridge/internal/syncstore"
)

const TokenPrefix = "fn-device-v1_"

var (
	ErrInvalid    = errors.New("invalid_device_credential")
	ErrConflict   = errors.New("device_binding_conflict")
	ErrAdoption   = errors.New("legacy_site_requires_explicit_adoption")
	ErrServerSite = errors.New("server_site_cannot_be_enrolled")
	ErrSchema     = errors.New("unsupported_device_identity_schema")
)

type Store struct{ DB *sql.DB }

// Enrollment contains a HASH, never the credential. The client must durably save
// its random secret in private storage before requesting enrollment. Retrying
// this exact request is safe even if the first committed response was lost.
type Enrollment struct {
	SiteID      string `json:"site_id"`
	TokenHash   string `json:"token_hash"`
	AdoptLegacy bool   `json:"adopt_legacy"`
}

// Install does not populate cursors, change clocks, enable sync or register a
// production route. Revoked bindings are retained, not pruned with sync cursors.
func Install(ctx context.Context, db *sql.DB) error {
	const schema = `CREATE TABLE sync_device_identity (
		site_id TEXT NOT NULL PRIMARY KEY,
		token_hash TEXT NOT NULL UNIQUE,
		created_at INTEGER NOT NULL,
		revoked INTEGER NOT NULL DEFAULT 0 CHECK(revoked IN (0,1))
	)`
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, strings.Replace(schema, "CREATE TABLE ", "CREATE TABLE IF NOT EXISTS ", 1)); err != nil {
		return err
	}
	var actual string
	if err = tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='sync_device_identity'`).Scan(&actual); err != nil {
		return err
	}
	// This candidate has exactly one DDL version. Unknown shape/constraints are
	// refused, never silently accepted or repaired in place. A future version
	// requires an explicit migration, including its uniqueness/revocation rules.
	if strings.Join(strings.Fields(actual), " ") != strings.Join(strings.Fields(schema), " ") {
		return ErrSchema
	}
	const retired = `CREATE TABLE sync_retired_replica (site_id TEXT NOT NULL PRIMARY KEY)`
	if _, err = tx.ExecContext(ctx, strings.Replace(retired, "CREATE TABLE ", "CREATE TABLE IF NOT EXISTS ", 1)); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE name='sync_retired_replica'`).Scan(&actual); err != nil {
		return err
	}
	if strings.Join(strings.Fields(actual), " ") != strings.Join(strings.Fields(retired), " ") {
		return ErrSchema
	}
	return tx.Commit()
}

func validHash(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// Enroll is an ADMINISTRATIVE approval of an existing local author identity, not
// evidence of hardware identity. It never silently replaces an enrolled key or
// revives a revoked key. Lost-key recovery/clone reconciliation is a separate gate.
func (s Store) Enroll(ctx context.Context, e Enrollment) error {
	if !syncstore.IsULID(e.SiteID) || e.SiteID[0] > '7' || !validHash(e.TokenHash) {
		return ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Acquire the SQLite writer before checking identity collisions. Multiple
	// connections cannot both observe a missing binding and overwrite each other.
	if _, err = tx.ExecContext(ctx, `UPDATE sync_device_identity SET revoked=revoked WHERE site_id=?`, e.SiteID); err != nil {
		return err
	}
	var server string
	if err = tx.QueryRowContext(ctx, `SELECT site_id FROM sync_site WHERE id=1`).Scan(&server); err != nil {
		return err
	}
	if e.SiteID == server {
		return ErrServerSite
	}
	var retired int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM sync_retired_replica WHERE site_id=?`, e.SiteID).Scan(&retired); err != nil {
		return err
	}
	if retired != 0 {
		return ErrConflict
	}
	var hash string
	var revoked int
	err = tx.QueryRowContext(ctx, `SELECT token_hash,revoked FROM sync_device_identity WHERE site_id=?`, e.SiteID).Scan(&hash, &revoked)
	if err == nil {
		if hash != e.TokenHash || revoked != 0 {
			return ErrConflict
		}
		return tx.Commit()
	}
	if err != sql.ErrNoRows {
		return err
	}
	var used int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM sync_device_identity WHERE token_hash=?`, e.TokenHash).Scan(&used); err != nil {
		return err
	}
	if used != 0 {
		return ErrConflict
	}
	if !e.AdoptLegacy {
		known, err := knownAuthor(ctx, tx, e.SiteID)
		if err != nil {
			return err
		}
		if known {
			return ErrAdoption
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sync_device_identity(site_id,token_hash,created_at) VALUES(?,?,?)`, e.SiteID, e.TokenHash, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Check both relay and surviving mirror provenance: pruning a cursor or
// compacting relay history must not turn a known author into a fresh enrollment.
func knownAuthor(ctx context.Context, tx *sql.Tx, site string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_ops WHERE site_id=? UNION ALL SELECT 1 FROM sync_cursors WHERE site_id=?)`, site, site).Scan(&n); err != nil {
		return false, err
	}
	if n != 0 {
		return true, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.name FROM sqlite_master m JOIN pragma_table_info(m.name) p WHERE m.type='table' AND ((m.name GLOB 'fn_*' AND p.name='lww_site_id') OR (m.name='reader_store_incoming' AND p.name='site_id'))`)
	if err != nil {
		return false, err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return false, err
		}
		tables = append(tables, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	for _, table := range tables {
		column := "lww_site_id"
		if table == "reader_store_incoming" {
			column = "site_id"
		}
		quoted := `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+quoted+` WHERE `+column+`=?)`, site).Scan(&n); err != nil {
			return false, err
		}
		if n != 0 {
			return true, nil
		}
	}
	return false, nil
}

// Resolve returns only the site bound in server storage. Labels, request fields,
// generic MCP bearer tokens and account passwords never enter this path.
func (s Store) Resolve(ctx context.Context, token string) (string, error) {
	if !strings.HasPrefix(token, TokenPrefix) || !validHash(strings.TrimPrefix(token, TokenPrefix)) {
		return "", ErrInvalid
	}
	h := sha256.Sum256([]byte(token))
	var site string
	err := s.DB.QueryRowContext(ctx, `SELECT site_id FROM sync_device_identity WHERE token_hash=? AND revoked=0`, hex.EncodeToString(h[:])).Scan(&site)
	if err == sql.ErrNoRows {
		return "", ErrInvalid
	}
	return site, err
}

// Revoke only disables authentication. It preserves content, binding reservation,
// ACK/cursor/clock and all authored provenance. Already admitted requests may finish.
func (s Store) Revoke(ctx context.Context, site string) error {
	result, err := s.DB.ExecContext(ctx, `UPDATE sync_device_identity SET revoked=1 WHERE site_id=?`, site)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrInvalid
	}
	return nil
}
