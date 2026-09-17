package forestnote

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jdkruzr/rhizome/server-go/assets"
	"github.com/jdkruzr/rhizome/server-go/bounded"
	"github.com/sysop/ultrabridge/internal/auth"
	"github.com/sysop/ultrabridge/internal/libraryrestore"
	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/processor"
	"github.com/sysop/ultrabridge/internal/rag"
	"github.com/sysop/ultrabridge/internal/readercontract"
	"github.com/sysop/ultrabridge/internal/search"
	"github.com/sysop/ultrabridge/internal/source"
	"github.com/sysop/ultrabridge/internal/syncgeneration"
	"github.com/sysop/ultrabridge/internal/syncidentity"
	"github.com/sysop/ultrabridge/internal/syncstore"
	"github.com/sysop/ultrabridge/internal/syncsvc"
	"golang.org/x/crypto/bcrypt"
)

const sharedSite = "0000000000000000000000000A"
const sharedPeer = "0000000000000000000000000B"

type countOCR struct{ calls atomic.Int32 }

func (o *countOCR) Recognize(context.Context, []byte, string) (string, error) {
	o.calls.Add(1)
	return "unexpected OCR", nil
}

func sharedRequest(h http.Handler, method, path, token string, body any, account bool) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(bounded.Header, "1")
	if account {
		r.SetBasicAuth("owner", "fixture")
	} else if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func newSharedFixture(t *testing.T) (*sql.DB, *auth.Middleware, *rag.Store) {
	t.Helper()
	db, err := notedb.Open(context.Background(), filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	hash, _ := bcrypt.GenerateFromPassword([]byte("fixture"), bcrypt.MinCost)
	return db, auth.New("owner", string(hash)), rag.NewStore(db, nil)
}
func startFixtureSource(t *testing.T, db *sql.DB, account *auth.Middleware, vectors *rag.Store, shared bool, ocr *countOCR) (*Source, *http.ServeMux) {
	t.Helper()
	cfg, _ := json.Marshal(Config{SharedLibrary: shared})
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ocr.calls.Add(1); http.Error(w, "unexpected OCR", 500) }))
	t.Cleanup(endpoint.Close)
	s, err := NewSource(db, source.SourceRow{Name: "fixture", ConfigJSON: string(cfg)}, source.SharedDeps{OCRClient: processor.NewOCRClient(endpoint.URL, "", "fixture", "")}, ForestNoteDeps{Account: account, Indexer: search.New(db), EmbedStore: vectors, ForgetEmbeddings: vectors.ForgetPrefix})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux, account)
	return s, mux
}
func enrollFixture(t *testing.T, h http.Handler, site, letter string) string {
	t.Helper()
	token := syncidentity.TokenPrefix + strings.Repeat(letter, 64)
	hash := sha256.Sum256([]byte(token))
	r := sharedRequest(h, "POST", "/sync/devices/v1/enroll", "", syncidentity.Enrollment{SiteID: site, TokenHash: hex.EncodeToString(hash[:])}, true)
	if r.Code != 204 {
		t.Fatal(r.Code, r.Body.String())
	}
	return token
}
func waitShared(t *testing.T, predicate func() bool) {
	t.Helper()
	until := time.Now().Add(10 * time.Second)
	for !predicate() {
		if time.Now().After(until) {
			t.Fatal("worker did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProductionSourceLegacyOffAndExplicitCutover(t *testing.T) {
	db, account, vectors := newSharedFixture(t)
	ocr := &countOCR{}
	s, mux := startFixtureSource(t, db, account, vectors, false, ocr)
	r := sharedRequest(mux, "POST", "/sync/v1", "", syncsvc.Request{ProtocolVersion: 1, SchemaHash: syncstore.SchemaHash(), SiteID: sharedSite}, true)
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	for _, path := range []string{"/sync/capabilities", "/sync/devices/v1/enroll", "/reader/search", "/sync/restore/v1/state"} {
		if r = sharedRequest(mux, "GET", path, "", nil, true); r.Code != 404 {
			t.Fatal(path, r.Code)
		}
	}
	s.Stop()
	s, mux = startFixtureSource(t, db, account, vectors, true, ocr)
	if s.SyncService() != nil {
		t.Fatal("legacy service exposed after cutover")
	}
	// A previously unbound writer cannot use account auth to bypass enrollment.
	if r = sharedRequest(mux, "POST", "/sync/v1", "", nil, true); r.Code != 401 {
		t.Fatal(r.Code, r.Body.String())
	}
	token := enrollFixture(t, mux, sharedPeer, "b")
	if r = sharedRequest(mux, "GET", "/sync/capabilities", token, nil, false); r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	if _, err := s.CompactNow(context.Background()); err == nil {
		t.Fatal("legacy compactor admitted")
	}
	s.Stop()
	legacy, _ := NewSource(db, source.SourceRow{}, source.SharedDeps{}, ForestNoteDeps{})
	if err := legacy.Start(context.Background()); err == nil {
		legacy.Stop()
		t.Fatal("cutover downgraded to unbound writer")
	}
}

func TestProductionSourceReplacementRebuildAndAdmission(t *testing.T) {
	ctx := context.Background()
	db, account, vectors := newSharedFixture(t)
	ocr := &countOCR{}
	if err := syncstore.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO fn_notebook(id,name,lww_wall_ts,lww_op_seq,lww_site_id) VALUES('00000000000000000000000001','old',1,1,'` + sharedSite + `')`,
		`INSERT INTO fn_page(id,notebook_id,lww_wall_ts,lww_op_seq,lww_site_id) VALUES('00000000000000000000000002','00000000000000000000000001',1,2,'` + sharedSite + `')`,
		`INSERT INTO fn_page_text_from_client(id,text,lww_wall_ts,lww_op_seq,lww_site_id) VALUES('00000000000000000000000002','saved client words',1,3,'` + sharedSite + `')`,
		`INSERT INTO note_content(note_path,page,body_text,indexed_at) VALUES('boox.note',0,'unrelated',1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	oldPath := "forestnote://00000000000000000000000001/00000000000000000000000002"
	for _, path := range []string{oldPath, "boox.note"} {
		if err := vectors.Save(ctx, path, 0, 0, []float32{1}, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	s, mux := startFixtureSource(t, db, account, vectors, true, ocr)
	waitShared(t, func() bool {
		var body string
		return db.QueryRow(`SELECT body_text FROM note_content WHERE note_path=?`, oldPath).Scan(&body) == nil && body == "saved client words"
	})
	if ocr.calls.Load() != 0 {
		t.Fatal("rebuild uploaded handwriting")
	}
	var authored int
	db.QueryRow(`SELECT count(*) FROM sync_ops`).Scan(&authored)
	if authored != 0 {
		t.Fatal("rebuild authored history", authored)
	}
	publisher := enrollFixture(t, mux, sharedPeer, "b")
	peer := enrollFixture(t, mux, "0000000000000000000000000C", "c")
	var archive bytes.Buffer
	z := zip.NewWriter(&archive)
	m, _ := z.Create("manifest.json")
	json.NewEncoder(m).Encode(libraryrestore.Manifest{Version: 1, Schema: readercontract.CandidateCombined().SchemaHash(), Publisher: sharedPeer})
	z.Create("rows.jsonl")
	z.Close()
	data := archive.Bytes()
	id := assets.Digest(data)
	a := &assets.SQLStore{DB: db}
	if _, _, err := a.Stage(ctx, assets.Descriptor{ID: id, ByteLength: int64(len(data)), ChunkBytes: assets.ChunkBytes}); err != nil {
		t.Fatal(err)
	}
	if err := a.WriteChunk(ctx, id, 0, data, id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Complete(ctx, id); err != nil {
		t.Fatal(err)
	}
	generation, _ := syncgeneration.Current(ctx, db)
	requestID := strings.Repeat("1", 64)
	body := map[string]string{"request_id": requestID, "expected_generation": generation, "snapshot": id, "publisher": sharedPeer}
	first := s.writer.Load()
	if _, err := db.Exec(`CREATE TRIGGER reject_replacement BEFORE DELETE ON fn_notebook BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	r := sharedRequest(mux, "POST", "/sync/restore/v1/publish", "", body, true)
	if r.Code != 503 || len(vectors.AllEmbeddings()) != 2 {
		t.Fatal("failed publication lost state", r.Code, r.Body.String())
	}
	if s.writer.Load() == first {
		t.Fatal("rollback did not recreate writer")
	}
	if _, err := db.Exec(`DROP TRIGGER reject_replacement`); err != nil {
		t.Fatal(err)
	}
	// Whole manual/backfill work stays admitted until cleanup is finished.
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_ = s.AdmitEmbedding(ctx, oldPath, func(context.Context) error { close(entered); <-release; return nil })
	}()
	<-entered
	pubDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { pubDone <- sharedRequest(mux, "POST", "/sync/restore/v1/publish", "", body, true) }()
	select {
	case <-pubDone:
		t.Fatal("replacement passed active writer")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-done
	select {
	case r = <-pubDone:
	case <-time.After(10 * time.Second):
		t.Fatal("publication stuck")
	}
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	if len(vectors.AllEmbeddings()) != 1 || vectors.AllEmbeddings()[0].NotePath != "boox.note" {
		t.Fatal("wrong cache scope")
	}
	var count int
	db.QueryRow(`SELECT count(*) FROM note_content WHERE note_path LIKE 'forestnote://%'`).Scan(&count)
	if count != 0 {
		t.Fatal("old index returned")
	}
	if ocr.calls.Load() != 0 {
		t.Fatal("restore invoked OCR")
	}
	s.Stop()
	s, mux = startFixtureSource(t, db, account, vectors, true, ocr)
	for _, path := range []string{"/sync/v1", "/sync/capabilities", "/reader/search", "/sync/assets/v1/" + id} {
		if r = sharedRequest(mux, "GET", path, peer, nil, false); r.Code != 409 {
			t.Fatal("peer bypass", path, r.Code, r.Body.String())
		}
	}
	if r = sharedRequest(mux, "GET", "/sync/restore/v1/publications/"+requestID, publisher, nil, false); r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	s.Stop()
	if err := s.EditTextBox(ctx, "box", "late"); err == nil {
		t.Fatal("stopped writer admitted")
	}
}
