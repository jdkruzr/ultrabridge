package syncidentity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sysop/ultrabridge/internal/syncstore"
	_ "modernc.org/sqlite"
)

const siteA = "0000000000000000000000000A"
const siteB = "0000000000000000000000000B"

var ctx = context.Background()

func key() (string, string) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	token := TokenPrefix + hex.EncodeToString(b[:])
	h := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(h[:])
}
func open(t *testing.T, path string) Store {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { db.Close() })
	if err = syncstore.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err = Install(ctx, db); err != nil {
		t.Fatal(err)
	}
	return Store{db}
}
func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func TestEnrollmentRetryRestartRevokeAndNoCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.db")
	s := open(t, path)
	token, hash := key()
	e := Enrollment{SiteID: siteA, TokenHash: hash}
	check(t, s.Enroll(ctx, e))
	check(t, s.Enroll(ctx, e))
	var n int
	check(t, s.DB.QueryRow(`SELECT count(*) FROM sync_cursors`).Scan(&n))
	if n != 0 {
		t.Fatal("Enrollment fabricated cursor")
	}
	var stored string
	check(t, s.DB.QueryRow(`SELECT token_hash FROM sync_device_identity`).Scan(&stored))
	if stored != hash || stored == token {
		t.Fatal("Wrong credential persistence")
	}
	check(t, s.DB.Close())
	s = open(t, path)
	got, err := s.Resolve(ctx, token)
	check(t, err)
	if got != siteA {
		t.Fatal("Identity lost after restart")
	}
	for _, bad := range []string{hash, "assetlab", token + "x", TokenPrefix + hash, "mcp-token"} {
		if _, err = s.Resolve(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Fatal("Noncredential authenticated")
		}
	}
	_, other := key()
	if err = s.Enroll(ctx, Enrollment{SiteID: siteA, TokenHash: other}); !errors.Is(err, ErrConflict) {
		t.Fatal("Replaced enrolled site")
	}
	if err = s.Enroll(ctx, Enrollment{SiteID: siteB, TokenHash: hash}); !errors.Is(err, ErrConflict) {
		t.Fatal("Credential bound to two sites")
	}
	check(t, s.Revoke(ctx, siteA))
	check(t, s.Revoke(ctx, siteA))
	if _, err = s.Resolve(ctx, token); !errors.Is(err, ErrInvalid) {
		t.Fatal("Revoked key admitted")
	}
	if err = s.Enroll(ctx, e); !errors.Is(err, ErrConflict) {
		t.Fatal("Retry revived revoked key")
	}
	check(t, s.DB.Close())
	s = open(t, path)
	if _, err = s.Resolve(ctx, token); !errors.Is(err, ErrInvalid) {
		t.Fatal("Restart revived revoked key")
	}
}
func TestConcurrentEnrollmentCannotReplaceBinding(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "identity.db"))
	tokens := make([]string, 12)
	hashes := make([]string, 12)
	result := make([]error, 12)
	var wg sync.WaitGroup
	for i := range tokens {
		tokens[i], hashes[i] = key()
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result[i] = s.Enroll(ctx, Enrollment{SiteID: siteA, TokenHash: hashes[i]})
		}(i)
	}
	wg.Wait()
	winners := 0
	for i, err := range result {
		if err == nil {
			winners++
			got, e := s.Resolve(ctx, tokens[i])
			check(t, e)
			if got != siteA {
				t.Fatal("wrong winner")
			}
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d winners", winners)
	}
}
func TestLegacyAdoptionChecksSurvivingMirrorAndPreservesProvenance(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "identity.db"))
	_, hash := key()
	// A mirror-only author remains known even after cursor/relay compaction.
	_, err := s.DB.Exec(`CREATE TABLE fn_fixture(id TEXT,lww_site_id TEXT,lww_op_seq INTEGER); INSERT INTO fn_fixture VALUES('old',?,123)`, siteA)
	check(t, err)
	e := Enrollment{SiteID: siteA, TokenHash: hash}
	if err = s.Enroll(ctx, e); !errors.Is(err, ErrAdoption) {
		t.Fatal("Silently adopted known author")
	}
	e.AdoptLegacy = true
	check(t, s.Enroll(ctx, e))
	var seq int
	check(t, s.DB.QueryRow(`SELECT lww_op_seq FROM fn_fixture WHERE lww_site_id=?`, siteA).Scan(&seq))
	if seq != 123 {
		t.Fatal("Adoption re-authored history")
	}
	var server string
	check(t, s.DB.QueryRow(`SELECT site_id FROM sync_site`).Scan(&server))
	_, hash = key()
	if err = s.Enroll(ctx, Enrollment{SiteID: server, TokenHash: hash, AdoptLegacy: true}); !errors.Is(err, ErrServerSite) {
		t.Fatal("Enrolled UB as client")
	}
}
func TestInvalidEnrollmentAndFailedWriteDoNotReserveIdentity(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "identity.db"))
	_, hash := key()
	for _, e := range []Enrollment{{SiteID: "bad", TokenHash: hash}, {SiteID: "Z000000000000000000000000A", TokenHash: hash}, {SiteID: siteA, TokenHash: "bad"}} {
		if !errors.Is(s.Enroll(ctx, e), ErrInvalid) {
			t.Fatal("Accepted invalid enrollment")
		}
	}
	_, err := s.DB.Exec(`CREATE TRIGGER fail_identity BEFORE INSERT ON sync_device_identity BEGIN SELECT RAISE(ABORT,'failure'); END`)
	check(t, err)
	if s.Enroll(ctx, Enrollment{SiteID: siteA, TokenHash: hash}) == nil {
		t.Fatal("Ignored failed commit")
	}
	_, err = s.DB.Exec(`DROP TRIGGER fail_identity`)
	check(t, err)
	check(t, s.Enroll(ctx, Enrollment{SiteID: siteA, TokenHash: hash}))
}

func TestUnknownIdentitySchemaFailsClosedWithoutRepair(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "identity.db"))
	_, err := s.DB.Exec(`DROP TABLE sync_device_identity; CREATE TABLE sync_device_identity(site_id TEXT,token_hash TEXT,created_at INTEGER,revoked INTEGER); INSERT INTO sync_device_identity VALUES('keep','unchanged',1,1)`)
	check(t, err)
	if !errors.Is(Install(ctx, s.DB), ErrSchema) {
		t.Fatal("Unknown uniqueness/revocation schema admitted")
	}
	var value string
	check(t, s.DB.QueryRow(`SELECT token_hash FROM sync_device_identity WHERE site_id='keep'`).Scan(&value))
	if value != "unchanged" {
		t.Fatal("Unknown schema was repaired destructively")
	}
}
