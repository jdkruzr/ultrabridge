package syncgeneration_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jdkruzr/rhizome/server-go/assets"
	"github.com/sysop/ultrabridge/internal/syncgeneration"
	"github.com/sysop/ultrabridge/internal/syncidentity"
	"github.com/sysop/ultrabridge/internal/syncstore"
	_ "modernc.org/sqlite"
)

var ctx = context.Background()

const publisher = "0000000000000000000000000A"
const peer = "0000000000000000000000000B"

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func open(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	check(t, err)
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { db.Close() })
	check(t, syncstore.Migrate(ctx, db))
	check(t, syncidentity.Install(ctx, db))
	return db
}
func key(site string) (string, string) {
	raw := syncidentity.TokenPrefix + strings.Repeat(strings.ToLower(site[len(site)-1:]), 64)
	hash := sha256.Sum256([]byte(raw))
	return raw, hex.EncodeToString(hash[:])
}
func enroll(t *testing.T, db *sql.DB, site string) {
	_, hash := key(site)
	check(t, (syncidentity.Store{DB: db}).Enroll(ctx, syncidentity.Enrollment{SiteID: site, TokenHash: hash}))
}
func generation(t *testing.T, db *sql.DB) string {
	t.Helper()
	v, err := syncgeneration.Current(ctx, db)
	check(t, err)
	return v
}
func fixture(t *testing.T) (*sql.DB, string) {
	path := filepath.Join(t.TempDir(), "generation.db")
	db := open(t, path)
	enroll(t, db, publisher)
	enroll(t, db, peer)
	_, err := db.Exec(`CREATE TABLE restore_fixture(value TEXT); INSERT INTO restore_fixture VALUES('newer live content')`)
	check(t, err)
	return db, path
}
func request(t *testing.T, db *sql.DB, id string) syncgeneration.Request {
	return syncgeneration.Request{ID: strings.Repeat(id, 64), Expected: generation(t, db), SnapshotHash: strings.Repeat("d", 64), Publisher: publisher}
}
func replace(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE restore_fixture SET value='restored backup'`)
	return err
}
func value(t *testing.T, db *sql.DB) string {
	t.Helper()
	var v string
	check(t, db.QueryRow(`SELECT value FROM restore_fixture`).Scan(&v))
	return v
}
func guarded(t *testing.T, db *sql.DB, claim context.Context, site string) error {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	check(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`UPDATE sync_library_generation SET generation=generation WHERE id=1`)
	check(t, err)
	return syncgeneration.CheckTx(claim, tx, site)
}

func TestReplacementRejectsOldAdmissionAndOldEnrollmentAcrossRestart(t *testing.T) {
	db, path := fixture(t)
	req := request(t, db, "1")
	oldPeer, _, err := syncgeneration.Admit(ctx, db, peer)
	check(t, err)
	oldPublisher, _, err := syncgeneration.Admit(ctx, db, publisher)
	check(t, err)
	receipt, err := syncgeneration.Publish(ctx, db, req, replace)
	check(t, err)
	if receipt.Replayed || receipt.Generation == req.Expected || value(t, db) != "restored backup" {
		t.Fatal("replacement not published")
	}
	for _, claim := range []struct {
		ctx  context.Context
		site string
	}{{oldPeer, peer}, {oldPublisher, publisher}} {
		if err = guarded(t, db, claim.ctx, claim.site); !errors.Is(err, syncgeneration.ErrReplaced) {
			t.Fatalf("in-flight request accepted: %v", err)
		}
	}
	if _, active, err := syncgeneration.Admit(ctx, db, peer); !errors.Is(err, syncgeneration.ErrReplaced) || active != receipt.Generation {
		t.Fatal("old peer admitted")
	}
	current, _, err := syncgeneration.Admit(ctx, db, publisher)
	check(t, err)
	check(t, guarded(t, db, current, publisher))
	if err = guarded(t, db, current, peer); !errors.Is(err, syncgeneration.ErrReplaced) {
		t.Fatal("borrowed another site's admission")
	}
	_, hash := key(peer)
	if err = (syncidentity.Store{DB: db}).Enroll(ctx, syncidentity.Enrollment{SiteID: peer, TokenHash: hash}); !errors.Is(err, syncgeneration.ErrReplaced) {
		t.Fatal("enrollment retry crossed fence")
	}
	check(t, db.Close())
	db = open(t, path)
	if generation(t, db) != receipt.Generation {
		t.Fatal("generation changed at startup")
	}
	if _, _, err = syncgeneration.Admit(ctx, db, peer); !errors.Is(err, syncgeneration.ErrReplaced) {
		t.Fatal("startup rebound peer")
	}
}

func TestFailedPublicationRollsBackContentGenerationAndReceipt(t *testing.T) {
	db, _ := fixture(t)
	req := request(t, db, "2")
	failure := errors.New("injected storage failure")
	_, err := syncgeneration.Publish(ctx, db, req, func(ctx context.Context, tx *sql.Tx) error {
		if err := replace(ctx, tx); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) || generation(t, db) != req.Expected || value(t, db) != "newer live content" {
		t.Fatal("failed restore leaked changes")
	}
	var n int
	check(t, db.QueryRow(`SELECT count(*) FROM sync_library_replacement`).Scan(&n))
	if n != 0 {
		t.Fatal("failed receipt persisted")
	}
	_, _, err = syncgeneration.Admit(ctx, db, peer)
	check(t, err)
	_, err = syncgeneration.Publish(ctx, db, req, replace)
	check(t, err)
}

func TestLostReplyRetriesAreExactAndCannotUndoLaterRestore(t *testing.T) {
	db, _ := fixture(t)
	req := request(t, db, "3")
	first, err := syncgeneration.Publish(ctx, db, req, replace)
	check(t, err)
	never := func(context.Context, *sql.Tx) error { t.Fatal("retry executed replacement again"); return nil }
	retry, err := syncgeneration.Publish(ctx, db, req, never)
	check(t, err)
	if !retry.Replayed || retry.Generation != first.Generation {
		t.Fatal("lost reply did not return same receipt")
	}
	changed := req
	changed.SnapshotHash = strings.Repeat("e", 64)
	if _, err = syncgeneration.Publish(ctx, db, changed, never); !errors.Is(err, syncgeneration.ErrConflict) {
		t.Fatal("changed retry accepted")
	}
	next := request(t, db, "4")
	_, err = syncgeneration.Publish(ctx, db, next, replace)
	check(t, err)
	if _, err = syncgeneration.Publish(ctx, db, req, never); !errors.Is(err, syncgeneration.ErrConflict) {
		t.Fatal("old retry reverted later restore")
	}
}

func TestReplacementCannotRemoveReservedPublicationState(t *testing.T) {
	for _, query := range []string{
		`DELETE FROM sync_library_generation`,
		`DELETE FROM sync_device_generation WHERE site_id='` + publisher + `'`,
	} {
		t.Run(query, func(t *testing.T) {
			db, _ := fixture(t)
			req := request(t, db, "a")
			_, err := syncgeneration.Publish(ctx, db, req, func(ctx context.Context, tx *sql.Tx) error {
				if err := replace(ctx, tx); err != nil {
					return err
				}
				_, err := tx.ExecContext(ctx, query)
				return err
			})
			if !errors.Is(err, syncgeneration.ErrConflict) || generation(t, db) != req.Expected || value(t, db) != "newer live content" {
				t.Fatalf("invalid replacement escaped rollback: %v", err)
			}
			_, _, err = syncgeneration.Admit(ctx, db, publisher)
			check(t, err)
			_, err = syncgeneration.Publish(ctx, db, req, replace)
			check(t, err)
		})
	}
}

func TestCompetingRestoresHaveExactlyOneWinnerAcrossConnections(t *testing.T) {
	db, path := fixture(t)
	other := open(t, path)
	a, b := request(t, db, "5"), request(t, db, "6")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i, pair := range []struct {
		db *sql.DB
		r  syncgeneration.Request
	}{{db, a}, {other, b}} {
		wg.Add(1)
		go func(i int, db *sql.DB, r syncgeneration.Request) {
			defer wg.Done()
			<-start
			_, errs[i] = syncgeneration.Publish(ctx, db, r, replace)
		}(i, pair.db, pair.r)
	}
	close(start)
	wg.Wait()
	winners := 0
	for _, err := range errs {
		if err == nil {
			winners++
		} else if !errors.Is(err, syncgeneration.ErrConflict) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("got %d winners", winners)
	}
}

func TestRevocationAfterAdmissionIsRecheckedInsideWriter(t *testing.T) {
	db, _ := fixture(t)
	claim, _, err := syncgeneration.Admit(ctx, db, peer)
	check(t, err)
	check(t, (syncidentity.Store{DB: db}).Revoke(ctx, peer))
	if err = guarded(t, db, claim, peer); !errors.Is(err, syncgeneration.ErrReplaced) {
		t.Fatal("revoked in-flight request accepted")
	}
}

func TestPartialOrMissingFenceFailsClosed(t *testing.T) {
	for _, damage := range []string{`DROP TABLE sync_device_generation`, `DELETE FROM sync_library_generation`, `UPDATE sync_library_generation SET generation='broken'`} {
		t.Run(damage, func(t *testing.T) {
			db, _ := fixture(t)
			_, err := db.Exec(damage)
			check(t, err)
			if err = syncidentity.Install(ctx, db); err == nil {
				t.Fatal("startup repaired an uncertain fence")
			}
		})
	}
}

func TestInitialUpgradePinsExistingBindingsOnlyOnce(t *testing.T) {
	db, _ := fixture(t)
	// Simulate the previous version's identity schema, before this feature existed.
	_, err := db.Exec(`DROP TABLE sync_device_generation; DROP TABLE sync_library_generation; DROP TABLE sync_library_replacement`)
	check(t, err)
	check(t, syncidentity.Install(ctx, db))
	before := generation(t, db)
	_, _, err = syncgeneration.Admit(ctx, db, peer)
	check(t, err)
	check(t, syncidentity.Install(ctx, db))
	if generation(t, db) != before {
		t.Fatal("startup minted a new generation")
	}
}

func TestEveryAssetMutationChecksTheSamePublicationFence(t *testing.T) {
	db, _ := fixture(t)
	store := &assets.SQLStore{DB: db, BeforeWrite: syncgeneration.CheckRequestTx}
	check(t, store.Migrate(ctx))
	claim, _, err := syncgeneration.Admit(ctx, db, peer)
	check(t, err)
	data := []byte("original book bytes")
	id := assets.Digest(data)
	descriptor := assets.Descriptor{ID: id, ByteLength: int64(len(data)), ChunkBytes: assets.ChunkBytes}
	_, _, err = store.Stage(claim, descriptor)
	check(t, err)
	check(t, store.WriteChunk(claim, id, 0, data, id))
	_, err = syncgeneration.Publish(ctx, db, request(t, db, "7"), replace)
	check(t, err)
	mutations := []func() error{
		func() error {
			_, _, err := store.Stage(claim, assets.Descriptor{ID: strings.Repeat("a", 64), ByteLength: 1, ChunkBytes: assets.ChunkBytes})
			return err
		},
		func() error { return store.WriteChunk(claim, id, 0, data, id) },
		func() error { _, err := store.Complete(claim, id); return err },
		func() error { return store.ResetInvalid(claim, id) },
	}
	for i, run := range mutations {
		if err = run(); !errors.Is(err, syncgeneration.ErrReplaced) {
			t.Fatalf("asset mutation %d crossed fence: %v", i, err)
		}
	}
	info, err := store.Describe(ctx, id)
	check(t, err)
	if info.State != "staging" {
		t.Fatal("old verifier altered state")
	}
	var n int
	check(t, db.QueryRow(`SELECT count(*) FROM rhizome_asset`).Scan(&n))
	if n != 1 {
		t.Fatal("old asset stage inserted bytes")
	}
	current, _, err := syncgeneration.Admit(ctx, db, publisher)
	check(t, err)
	info, err = store.Complete(current, id)
	check(t, err)
	if info.State != "ready" {
		t.Fatal("publisher cannot finish asset")
	}
}

func TestAssetVerificationRechecksAdmissionAfterHashing(t *testing.T) {
	db, _ := fixture(t)
	calls := 0
	reject := false
	store := &assets.SQLStore{DB: db, BeforeWrite: func(ctx context.Context, tx *sql.Tx) error {
		calls++
		if reject && calls == 2 {
			return syncgeneration.ErrReplaced
		}
		return syncgeneration.CheckRequestTx(ctx, tx)
	}}
	check(t, store.Migrate(ctx))
	claim, _, err := syncgeneration.Admit(ctx, db, peer)
	check(t, err)
	data := []byte("book")
	id := assets.Digest(data)
	_, _, err = store.Stage(claim, assets.Descriptor{ID: id, ByteLength: 4, ChunkBytes: assets.ChunkBytes})
	check(t, err)
	check(t, store.WriteChunk(claim, id, 0, data, id))
	calls = 0
	reject = true
	if _, err = store.Complete(claim, id); !errors.Is(err, syncgeneration.ErrReplaced) {
		t.Fatal("verifier did not recheck")
	}
	if calls != 2 {
		t.Fatalf("guard calls %d", calls)
	}
	info, err := store.Describe(ctx, id)
	check(t, err)
	if info.State != "verifying" {
		t.Fatal("rejected verifier published ready")
	}
	reject = false
	_, err = store.Complete(claim, id)
	check(t, err)
}
