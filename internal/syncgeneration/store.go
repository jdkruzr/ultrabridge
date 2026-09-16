// Package syncgeneration fences one author's shared library across deliberate
// whole-library replacement. A generation is not a user, tenant, or document ID.
// Candidate-only: there is no externally reachable restore/publish endpoint yet.
package syncgeneration

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
)

var (
	ErrReplaced = errors.New("library_replaced")
	ErrInvalid  = errors.New("invalid_library_replacement")
	ErrConflict = errors.New("library_replacement_conflict")
	ErrSchema   = errors.New("invalid_library_generation_schema")
)

const Header = "X-Alexandria-Library-Generation"

func digest(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func newGeneration() (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

// InstallTx upgrades an existing identity store exactly once. A later startup
// never enrolls old devices into a newer generation or repairs a missing fence.
func InstallTx(ctx context.Context, tx *sql.Tx) error {
	var present int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name IN ('sync_library_generation','sync_device_generation','sync_library_replacement')`).Scan(&present); err != nil {
		return err
	}
	if present != 0 && present != 3 {
		return ErrSchema
	}
	for _, ddl := range []string{
		`CREATE TABLE sync_library_generation(id INTEGER PRIMARY KEY CHECK(id=1),generation TEXT NOT NULL)`,
		`CREATE TABLE sync_device_generation(site_id TEXT PRIMARY KEY NOT NULL,generation TEXT NOT NULL)`,
		`CREATE TABLE sync_library_replacement(request_id TEXT PRIMARY KEY NOT NULL,expected_generation TEXT NOT NULL,snapshot_hash TEXT NOT NULL,publisher TEXT NOT NULL,generation TEXT NOT NULL UNIQUE)`,
	} {
		if _, err := tx.ExecContext(ctx, strings.Replace(ddl, "CREATE TABLE ", "CREATE TABLE IF NOT EXISTS ", 1)); err != nil {
			return err
		}
		name := strings.Split(strings.TrimPrefix(ddl, "CREATE TABLE "), "(")[0]
		var actual string
		if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&actual); err != nil {
			return err
		}
		if strings.Join(strings.Fields(actual), " ") != strings.Join(strings.Fields(ddl), " ") {
			return ErrSchema
		}
	}
	if present == 0 {
		generation, err := newGeneration()
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO sync_library_generation VALUES(1,?)`, generation); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sync_device_generation SELECT site_id,? FROM sync_device_identity`, generation)
		return err
	}
	_, err := current(ctx, tx)
	return err
}

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func current(ctx context.Context, q querier) (string, error) {
	var generation string
	if err := q.QueryRowContext(ctx, `SELECT generation FROM sync_library_generation WHERE id=1`).Scan(&generation); err != nil {
		return "", err
	}
	if !digest(generation) {
		return "", ErrSchema
	}
	return generation, nil
}
func Current(ctx context.Context, db *sql.DB) (string, error) { return current(ctx, db) }

// EnrollTx is only for a newly approved identity. Existing keys must use CheckTx;
// replaying an old enrollment request must never consent to adopting a restore.
func EnrollTx(ctx context.Context, tx *sql.Tx, site string) error {
	generation, err := current(ctx, tx)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sync_device_generation VALUES(?,?)`, site, generation)
	return err
}

type admission struct{ site, generation string }
type admissionKey struct{}

// CheckRequestTx is the shared guard for host asset-store mutations.
func CheckRequestTx(ctx context.Context, tx *sql.Tx) error {
	claim, _ := ctx.Value(admissionKey{}).(admission)
	return CheckTx(ctx, tx, claim.site)
}

// Admit follows credential verification and pins its generation to the request.
// Reading this header/state is not permission to change the device's binding.
func Admit(ctx context.Context, db *sql.DB, site string) (context.Context, string, error) {
	var bound, active string
	err := db.QueryRowContext(ctx, `SELECT d.generation,g.generation FROM sync_device_generation d CROSS JOIN sync_library_generation g WHERE d.site_id=? AND g.id=1`, site).Scan(&bound, &active)
	if err == sql.ErrNoRows {
		return ctx, "", ErrReplaced
	}
	if err != nil {
		return ctx, "", err
	}
	if !digest(active) || !digest(bound) {
		return ctx, "", ErrSchema
	}
	if bound != active {
		return ctx, active, ErrReplaced
	}
	return context.WithValue(ctx, admissionKey{}, admission{site, active}), active, nil
}

// CheckTx must execute after acquiring the writer and BEFORE any mutation, ACK,
// cursor, receipt or relay. An admission made before a restore is not enough.
func CheckTx(ctx context.Context, tx *sql.Tx, site string) error {
	var installed int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='sync_library_generation'`).Scan(&installed); err != nil {
		return err
	}
	claim, claimed := ctx.Value(admissionKey{}).(admission)
	if installed == 0 && !claimed {
		return nil
	} // Unenrolled legacy/fixture path only.
	if installed != 1 || !claimed || claim.site != site {
		return ErrReplaced
	}
	active, err := current(ctx, tx)
	if err != nil {
		return err
	}
	var bound string
	if err = tx.QueryRowContext(ctx, `SELECT generation FROM sync_device_generation WHERE site_id=?`, site).Scan(&bound); err != nil {
		if err == sql.ErrNoRows {
			return ErrReplaced
		}
		return err
	}
	if claim.generation != active || bound != active {
		return ErrReplaced
	}
	// Credential revocation between HTTP admission and writer acquisition also
	// cannot sneak a late request into the replacement transaction's successor.
	var revoked int
	if err = tx.QueryRowContext(ctx, `SELECT revoked FROM sync_device_identity WHERE site_id=?`, site).Scan(&revoked); err != nil {
		return err
	}
	if revoked != 0 {
		return ErrReplaced
	}
	return nil
}

// Request pins one explicit approval to a base generation and exact staged
// snapshot. The publisher must be a fresh local restore identity, never an old
// peer borrowing a current generation. Validation of that snapshot is the host's job.
type Request struct{ ID, Expected, SnapshotHash, Publisher string }
type Receipt struct {
	Generation string
	Replayed   bool
}

// Publish atomically couples the host's prevalidated replacement with the fence.
// replace must use only the supplied transaction: no network, callbacks to live
// workers, or writes to unrelated UB sources. It is deliberately not an HTTP API.
// No-op retries do not run replace again. A superseded retry cannot roll back a
// later restore, even when a previous success response was lost.
func Publish(ctx context.Context, db *sql.DB, r Request, replace func(context.Context, *sql.Tx) error) (Receipt, error) {
	if replace == nil {
		return Receipt{}, ErrInvalid
	}
	return PublishWithGeneration(ctx, db, r, func(c context.Context, t *sql.Tx, _ string) error { return replace(c, t) })
}

// PublishWithGeneration lets a host bind its baseline descriptor to the exact
// successor in the same transaction; there is never a separately committed pointer.
func PublishWithGeneration(ctx context.Context, db *sql.DB, r Request, replace func(context.Context, *sql.Tx, string) error) (Receipt, error) {
	if !digest(r.ID) || !digest(r.Expected) || !digest(r.SnapshotHash) || r.Publisher == "" || len(r.Publisher) > 128 || replace == nil {
		return Receipt{}, ErrInvalid
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Receipt{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE sync_library_generation SET generation=generation WHERE id=1`); err != nil {
		return Receipt{}, err
	}
	active, err := current(ctx, tx)
	if err != nil {
		return Receipt{}, err
	}
	var old Request
	var next string
	err = tx.QueryRowContext(ctx, `SELECT expected_generation,snapshot_hash,publisher,generation FROM sync_library_replacement WHERE request_id=?`, r.ID).Scan(&old.Expected, &old.SnapshotHash, &old.Publisher, &next)
	if err == nil {
		if old.Expected != r.Expected || old.SnapshotHash != r.SnapshotHash || old.Publisher != r.Publisher || active != next {
			return Receipt{}, ErrConflict
		}
		return Receipt{next, true}, tx.Commit()
	}
	if err != sql.ErrNoRows {
		return Receipt{}, err
	}
	if active != r.Expected {
		return Receipt{}, ErrConflict
	}
	// The host supplies prior admin approval and a validated staged snapshot;
	// this primitive additionally requires an active publisher binding.
	var bound string
	err = tx.QueryRowContext(ctx, `SELECT g.generation FROM sync_device_generation g JOIN sync_device_identity i USING(site_id) WHERE g.site_id=? AND i.revoked=0`, r.Publisher).Scan(&bound)
	if err != nil {
		if err == sql.ErrNoRows {
			return Receipt{}, ErrInvalid
		}
		return Receipt{}, err
	}
	if bound != active {
		return Receipt{}, ErrReplaced
	}
	if next, err = newGeneration(); err != nil {
		return Receipt{}, err
	}
	if err = replace(ctx, tx, next); err != nil {
		return Receipt{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE sync_library_generation SET generation=? WHERE id=1 AND generation=?`, next, r.Expected)
	if err != nil {
		return Receipt{}, err
	}
	if n, e := result.RowsAffected(); e != nil {
		return Receipt{}, e
	} else if n != 1 {
		return Receipt{}, ErrConflict
	}
	// Only the publishing replica already holds the approved snapshot. All other
	// replicas stay behind the old fence until whole-library adoption is complete.
	result, err = tx.ExecContext(ctx, `UPDATE sync_device_generation SET generation=? WHERE site_id=? AND generation=?`, next, r.Publisher, r.Expected)
	if err != nil {
		return Receipt{}, err
	}
	if n, e := result.RowsAffected(); e != nil {
		return Receipt{}, e
	} else if n != 1 {
		return Receipt{}, ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sync_library_replacement VALUES(?,?,?,?,?)`, r.ID, r.Expected, r.SnapshotHash, r.Publisher, next); err != nil {
		return Receipt{}, err
	}
	if err = tx.Commit(); err != nil {
		return Receipt{}, err
	}
	return Receipt{next, false}, nil
}
