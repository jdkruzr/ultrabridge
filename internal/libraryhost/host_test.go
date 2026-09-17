package libraryhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jdkruzr/rhizome/server-go/bounded"
	"github.com/sysop/ultrabridge/internal/auth"
	"github.com/sysop/ultrabridge/internal/libraryrestore"
	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/readercontract"
	"github.com/sysop/ultrabridge/internal/readerstore"
	"github.com/sysop/ultrabridge/internal/syncidentity"
	"github.com/sysop/ultrabridge/internal/syncstore"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

var ctx = context.Background()

const site = "0000000000000000000000000A"

func fixture(t *testing.T, edit func(*Options)) (*Host, *sql.DB, string) {
	t.Helper()
	db, err := notedb.Open(ctx, filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err = syncstore.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("fixture"), bcrypt.MinCost)
	options := Options{Account: auth.New("owner", string(hash)), Worker: readerstore.DefaultWorkerOptions()}
	if edit != nil {
		edit(&options)
	}
	host, err := New(ctx, db, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)
	token := syncidentity.TokenPrefix + strings.Repeat("a", 64)
	digest := sha256.Sum256([]byte(token))
	if err = (syncidentity.Store{DB: db}).Enroll(ctx, syncidentity.Enrollment{SiteID: site, TokenHash: hex.EncodeToString(digest[:])}); err != nil {
		t.Fatal(err)
	}
	return host, db, token
}
func call(h http.Handler, method, path, token string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set(bounded.Header, "1")
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	out := httptest.NewRecorder()
	h.ServeHTTP(out, r)
	return out
}
func await(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("lifetime operation timed out")
	}
}

func TestEnrolledHostRequiresDeviceKeysAndRestoreOptIn(t *testing.T) {
	h, _, token := fixture(t, nil)
	for _, path := range []string{"/sync/v1", "/sync/capabilities", "/reader/search", "/sync/assets/v1/" + strings.Repeat("b", 64)} {
		if got := call(h, "GET", path, "", nil).Code; got != 401 {
			t.Fatalf("unprotected %s: %d", path, got)
		}
		req := httptest.NewRequest("GET", path, nil)
		req.SetBasicAuth("owner", "fixture")
		out := httptest.NewRecorder()
		h.ServeHTTP(out, req)
		if out.Code != 401 {
			t.Fatalf("shared account bypass: %s %d", path, out.Code)
		}
	}
	if got := call(h, "GET", "/sync/capabilities", token, nil).Code; got != 200 {
		t.Fatal(got)
	}
	if got := call(h, "GET", "/sync/restore/v1/state", token, nil).Code; got != 404 {
		t.Fatal("restore enabled by default", got)
	}
	h.Close()
	if got := call(h, "GET", "/sync/capabilities", token, nil).Code; got != 503 {
		t.Fatal("closed host admitted work", got)
	}
	if err := h.Do(ctx, func(context.Context) error { t.Fatal("closed manual admission"); return nil }); !errors.Is(err, libraryrestore.ErrWorkersClosed) {
		t.Fatal(err)
	}
}

func TestMixedCommitNotifiesWriterOnlyAfterSuccess(t *testing.T) {
	var writes atomic.Int32
	var db *sql.DB
	h, opened, token := fixture(t, func(o *Options) {
		o.WriterChanged = func(c context.Context, changed []syncstore.TablePK) {
			// With a one-connection pool this would deadlock if called inside receipt.
			var count int
			if err := db.QueryRowContext(c, "SELECT COUNT(*) FROM fn_notebook").Scan(&count); err != nil || count != 1 {
				t.Errorf("callback before commit: %d %v", count, err)
			}
			if len(changed) != 1 || changed[0].Table != "page" {
				t.Errorf("wrong writer changes: %+v", changed)
			}
			writes.Add(1)
		}
	})
	db = opened
	raw, _ := json.Marshal(map[string]any{"table": "notebook", "pk": "00000000000000000000000001", "site_id": site, "op_seq": 1, "op_ts": 1,
		"cols": map[string]any{"name": "Shared writer", "sort_order": 0, "created_at": 1, "deleted_at": nil, "folder_id": nil, "aspect_long_axis": nil, "page_width": nil, "page_height": nil}})
	page, _ := json.Marshal(map[string]any{"table": "page", "pk": "00000000000000000000000002", "site_id": site, "op_seq": 2, "op_ts": 2,
		"cols": map[string]any{"notebook_id": "00000000000000000000000001", "sort_order": 0, "created_at": 1, "deleted_at": nil, "template": nil, "template_pitch_mm": nil}})
	req := bounded.Request{ProtocolVersion: 1, SchemaHash: readercontract.CandidateCombined().SchemaHash(), SiteID: site, Ops: []json.RawMessage{raw, page}}
	body, _ := json.Marshal(req)
	if out := call(h, "POST", "/sync/v1", token, body); out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	req.SiteID = "0000000000000000000000000B"
	body, _ = json.Marshal(req)
	if out := call(h, "POST", "/sync/v1", token, body); out.Code != 403 {
		t.Fatal(out.Code, out.Body.String())
	}
	if writes.Load() != 1 {
		t.Fatal("failed exchange notified writer", writes.Load())
	}
}

func TestReplacementWaitsForManualAdmissionAndRecreatesAllWorkersOnFailure(t *testing.T) {
	var starts, stops atomic.Int32
	h, _, _ := fixture(t, func(o *Options) {
		o.Auxiliary = func(c context.Context) func() {
			starts.Add(1)
			done := make(chan struct{})
			go func() { <-c.Done(); stops.Add(1); close(done) }()
			return func() { <-done }
		}
	})
	oldReader, oldSearch := h.workers.current.Load(), h.workers.search.Load()
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		_ = h.Do(ctx, func(context.Context) error { close(entered); <-release; return nil })
	}()
	await(t, entered)
	replacing, replaced := make(chan struct{}), make(chan error, 1)
	failure := errors.New("injected precommit failure")
	go func() {
		close(replacing)
		replaced <- h.Exclusive(ctx, func() error {
			if stops.Load() != 1 {
				t.Error("old auxiliary not joined")
			}
			select {
			case <-finished:
			default:
				t.Error("old request still active")
			}
			return failure
		})
	}()
	await(t, replacing)
	select {
	case err := <-replaced:
		t.Fatalf("crossed active request: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-replaced:
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("replacement stuck")
	}
	if starts.Load() != 2 || stops.Load() != 1 {
		t.Fatal(starts.Load(), stops.Load())
	}
	if h.workers.current.Load() == oldReader || h.workers.search.Load() == oldSearch {
		t.Fatal("stale worker survived")
	}
	h.Close()
	h.Close()
	if starts.Load() != 2 || stops.Load() != 2 {
		t.Fatal("incorrect final lifetime", starts.Load(), stops.Load())
	}
	if err := h.Exclusive(ctx, func() error { t.Fatal("replacement after shutdown"); return nil }); err == nil {
		t.Fatal("closed exclusive succeeded")
	}
}

func TestCloseCancelsAndJoinsAdmittedWork(t *testing.T) {
	h, _, _ := fixture(t, nil)
	entered, cancelled, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		_ = h.Do(ctx, func(c context.Context) error { close(entered); <-c.Done(); close(cancelled); <-release; return c.Err() })
	}()
	await(t, entered)
	go func() { h.Close(); close(done) }()
	await(t, cancelled)
	select {
	case <-done:
		t.Fatal("shutdown did not join")
	default:
	}
	close(release)
	await(t, done)
}

func TestRestoreRoutesUseSameEnrolledHost(t *testing.T) {
	h, _, token := fixture(t, func(o *Options) { o.Restore = true })
	if out := call(h, "GET", "/sync/restore/v1/state", token, nil); out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	if out := call(h, "POST", "/sync/restore/v1/publish", token, []byte(`{}`)); out.Code != 401 {
		t.Fatal("device key can publish", out.Code)
	}
	if out := call(h, "GET", "/sync/restore/v1/state", "", nil); out.Code != 401 {
		t.Fatal(out.Code)
	}
}
