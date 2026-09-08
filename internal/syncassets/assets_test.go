package syncassets

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jdkruzr/rhizome/server-go/assets"
	"github.com/sysop/ultrabridge/internal/auth"
	"github.com/sysop/ultrabridge/internal/syncstore"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, s := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL"} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
func request(t *testing.T, h http.Handler, method, path string, b []byte, digest string, want int) []byte {
	t.Helper()
	r := httptest.NewRequest(method, "/sync/assets/v1/"+path, bytes.NewReader(b))
	r.SetBasicAuth("test", "test")
	if digest != "" {
		r.Header.Set("Content-Type", "application/octet-stream")
		r.Header.Set("X-Rhizome-Chunk-SHA256", digest)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s: got %d %s; want %d", method, path, w.Code, w.Body.String(), want)
	}
	return w.Body.Bytes()
}
func handler(t *testing.T, db *sql.DB) http.Handler {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("test"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return Handler(db, auth.New("test", string(hash)))
}
func descriptor(b []byte) assets.Descriptor {
	return assets.Descriptor{ID: assets.Digest(b), ByteLength: int64(len(b)), ChunkBytes: assets.ChunkBytes}
}
func stage(t *testing.T, h http.Handler, d assets.Descriptor, status int) {
	t.Helper()
	b, _ := json.Marshal(d)
	request(t, h, "PUT", d.ID, b, "", status)
}

func TestDurableRoundTripAndLegacyTables(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "library.db")
	db := openDB(t, path)
	h := handler(t, db)
	if err := syncstore.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	b := bytes.Repeat([]byte("A"), assets.ChunkBytes+5)
	d := descriptor(b)
	if d.ID != "27b977cd4cd686fa69afab5a0d46a4aa2d3dba56e5f459b63ef5db53062fbe77" {
		t.Fatal("fixture digest")
	}
	stage(t, h, d, 201)
	stage(t, h, d, 200)
	request(t, h, "GET", d.ID+"/chunks/0", nil, "", 409)
	request(t, h, "POST", d.ID+"/complete", nil, "", 409)
	request(t, h, "PUT", d.ID+"/chunks/0", b[:assets.ChunkBytes], assets.Digest([]byte("wrong")), 422)
	var n int
	db.QueryRow(`SELECT count(*) FROM rhizome_asset_chunk`).Scan(&n)
	if n != 0 {
		t.Fatal("bad chunk persisted")
	}
	request(t, h, "PUT", d.ID+"/chunks/0", b[:assets.ChunkBytes], assets.Digest(b[:assets.ChunkBytes]), 204)
	// Lose the acknowledgement and the entire store instance, then retry.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openDB(t, path)
	h = handler(t, db)
	request(t, h, "PUT", d.ID+"/chunks/0", b[:assets.ChunkBytes], assets.Digest(b[:assets.ChunkBytes]), 204)
	request(t, h, "PUT", d.ID+"/chunks/1", b[assets.ChunkBytes:], assets.Digest(b[assets.ChunkBytes:]), 204)
	// Persisted verification interrupted by process death must restart, not assume ready.
	if _, err := db.Exec(`UPDATE rhizome_asset SET state='verifying'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db = openDB(t, path)
	h = handler(t, db)
	request(t, h, "POST", d.ID+"/complete", nil, "", 200)
	request(t, h, "POST", d.ID+"/complete", nil, "", 200)
	got := append(request(t, h, "GET", d.ID+"/chunks/0", nil, "", 200), request(t, h, "GET", d.ID+"/chunks/1", nil, "", 200)...)
	if !bytes.Equal(got, b) {
		t.Fatal("not byte-exact")
	}
	request(t, h, "POST", d.ID+"/reset-invalid", nil, "", 409)
	d.ByteLength++
	stage(t, h, d, 409)
	// Assets never author row operations or advance an acknowledgement.
	for _, table := range []string{"sync_ops", "sync_cursors"} {
		if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s changed: %d %v", table, n, err)
		}
	}
}

func TestInvalidRootResetEmptyAndBounds(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "library.db"))
	h := handler(t, db)
	d := descriptor([]byte("right"))
	stage(t, h, d, 201)
	request(t, h, "PUT", d.ID+"/chunks/0", []byte("wrong"), assets.Digest([]byte("wrong")), 204)
	request(t, h, "PUT", d.ID+"/chunks/0", []byte("right"), assets.Digest([]byte("right")), 409)
	request(t, h, "POST", d.ID+"/complete", nil, "", 422)
	request(t, h, "GET", d.ID+"/chunks/0", nil, "", 409)
	request(t, h, "POST", d.ID+"/reset-invalid", nil, "", 204)
	request(t, h, "PUT", d.ID+"/chunks/0", []byte("right"), assets.Digest([]byte("right")), 204)
	request(t, h, "POST", d.ID+"/complete", nil, "", 200)
	for _, query := range []string{"start=-1&limit=1", "start=2&limit=1", "start=0&limit=257", "start=0&limit=0", "start=9223372036854775808&limit=1"} {
		request(t, h, "GET", d.ID+"/chunks?"+query, nil, "", 400)
	}
	request(t, h, "GET", d.ID+"/chunks?start=1&limit=1", nil, "", 200)
	request(t, h, "PUT", d.ID+"/chunks/1", []byte("right"), assets.Digest([]byte("right")), 400)
	request(t, h, "PUT", d.ID+"/chunks/0", []byte("x"), assets.Digest([]byte("x")), 400)
	empty := descriptor(nil)
	stage(t, h, empty, 201)
	request(t, h, "POST", empty.ID+"/complete", nil, "", 200)
	max := assets.Descriptor{ID: d.ID, ByteLength: math.MaxInt64, ChunkBytes: assets.ChunkBytes}
	if max.ChunkCount() != 35184372088832 {
		t.Fatal("overflow")
	}
	if length, err := max.ChunkLength(max.ChunkCount() - 1); err != nil || length != 262143 {
		t.Fatal(length, err)
	}
}

func TestAuthenticationAndDiskFullNeverAcknowledge(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "library.db"))
	h := handler(t, db)
	b := bytes.Repeat([]byte("A"), assets.ChunkBytes)
	d := descriptor(b)
	wire, _ := json.Marshal(d)
	r := httptest.NewRequest("PUT", "/sync/assets/v1/"+d.ID, bytes.NewReader(wire))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM rhizome_asset`).Scan(&n)
	if n != 0 {
		t.Fatal("unauthorized write")
	}
	stage(t, h, d, 201)
	var pages int
	if err := db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA max_page_count=%d`, pages)); err != nil {
		t.Fatal(err)
	}
	request(t, h, "PUT", d.ID+"/chunks/0", b, assets.Digest(b), 507)
	if err := db.QueryRow(`SELECT count(*) FROM rhizome_asset_chunk`).Scan(&n); err != nil || n != 0 {
		t.Fatal("full DB acknowledged bytes", n, err)
	}
}

func TestConcurrentRetriesAndBoundedManifest(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "library.db"))
	s := &assets.SQLStore{DB: db}
	ctx := context.Background()
	b := []byte("hello")
	d := descriptor(b)
	if _, _, err := s.Stage(ctx, d); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for j := 0; j < 8; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.WriteChunk(ctx, d.ID, 0, b, assets.Digest(b)); err != nil {
				t.Error(err)
			}
			if _, err := s.Complete(ctx, d.ID); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	large := assets.Descriptor{ID: assets.Digest([]byte("large")), ByteLength: assets.ChunkBytes * 1000, ChunkBytes: assets.ChunkBytes}
	if _, _, err := s.Stage(ctx, large); err != nil {
		t.Fatal(err)
	}
	p, err := s.ListChunks(ctx, large.ID, 0, 256)
	if err != nil || len(p.Entries) != 256 || p.NextStart == nil || *p.NextStart != 256 {
		t.Fatal(p, err)
	}
	for _, e := range p.Entries {
		if e.SHA256 != nil {
			t.Fatal("missing chunk appeared verified")
		}
	}
	// HTTP response stays bounded even for a 1000-chunk descriptor.
	server := httptest.NewServer(handler(t, db))
	defer server.Close()
	req, _ := http.NewRequest("GET", server.URL+"/sync/assets/v1/"+large.ID+"/chunks?start=0&limit=256", nil)
	req.SetBasicAuth("test", "test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || len(body) > 65536 {
		t.Fatal(resp.StatusCode, len(body))
	}
}
