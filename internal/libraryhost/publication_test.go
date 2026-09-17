package libraryhost

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jdkruzr/rhizome/server-go/assets"
	"github.com/sysop/ultrabridge/internal/auth"
	"github.com/sysop/ultrabridge/internal/libraryrestore"
	"github.com/sysop/ultrabridge/internal/rag"
	"github.com/sysop/ultrabridge/internal/readercontract"
	"github.com/sysop/ultrabridge/internal/readerstore"
	"github.com/sysop/ultrabridge/internal/syncgeneration"
	"github.com/sysop/ultrabridge/internal/syncidentity"
	"golang.org/x/crypto/bcrypt"
)

func TestPublicationThroughHostPreservesUnrelatedDataAndFencesPeersAcrossRestart(t *testing.T) {
	var starts atomic.Int32
	var cleared atomic.Int32
	var vectors *rag.Store
	h, db, publisher := fixture(t, func(o *Options) {
		o.Restore = true
		o.StageRoot = t.TempDir()
		o.Auxiliary = func(context.Context) func() { starts.Add(1); return func() {} }
		o.ReplaceDerived = func(ctx context.Context, tx *sql.Tx) error {
			for _, q := range []string{`DELETE FROM note_content WHERE note_path LIKE 'forestnote://%'`, `DELETE FROM note_embeddings WHERE note_path LIKE 'forestnote://%'`} {
				if _, err := tx.ExecContext(ctx, q); err != nil {
					return err
				}
			}
			return nil
		}
		o.AfterReplace = func() { vectors.ForgetPrefix("forestnote://"); cleared.Add(1) }
	})
	vectors = rag.NewStore(db, nil)
	for _, path := range []string{"forestnote://old/page", "boox.note"} {
		if _, err := db.Exec(`INSERT INTO note_content(note_path,page,body_text,indexed_at) VALUES(?,0,'keep scope',1)`, path); err != nil {
			t.Fatal(err)
		}
		if err := vectors.Save(ctx, path, 0, 0, []float32{1}, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	peerSite := "0000000000000000000000000B"
	peerToken := syncidentity.TokenPrefix + strings.Repeat("b", 64)
	digest := sha256.Sum256([]byte(peerToken))
	if err := (syncidentity.Store{DB: db}).Enroll(ctx, syncidentity.Enrollment{SiteID: peerSite, TokenHash: hex.EncodeToString(digest[:])}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE unrelated_settings(value TEXT);INSERT INTO unrelated_settings VALUES('preserve');INSERT INTO fn_notebook(id,name,lww_wall_ts,lww_op_seq,lww_site_id) VALUES('00000000000000000000000001','old',1,1,?)`, peerSite); err != nil {
		t.Fatal(err)
	}
	// An empty-library restore clears old notes, never merges them back in.
	var archive bytes.Buffer
	z := zip.NewWriter(&archive)
	m, err := z.Create("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.NewEncoder(m).Encode(libraryrestore.Manifest{Version: 1, Schema: readercontract.CandidateCombined().SchemaHash(), Publisher: site, HighWater: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err = z.Create("rows.jsonl"); err != nil {
		t.Fatal(err)
	}
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	b := archive.Bytes()
	id := assets.Digest(b)
	a := &assets.SQLStore{DB: db}
	if _, _, err = a.Stage(ctx, assets.Descriptor{ID: id, ByteLength: int64(len(b)), ChunkBytes: assets.ChunkBytes}); err != nil {
		t.Fatal(err)
	}
	if err = a.WriteChunk(ctx, id, 0, b, id); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Complete(ctx, id); err != nil {
		t.Fatal(err)
	}
	generation, err := syncgeneration.Current(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	requestID := strings.Repeat("1", 64)
	body, _ := json.Marshal(map[string]string{"request_id": requestID, "expected_generation": generation, "snapshot": id, "publisher": site})
	pub := httptest.NewRequest("POST", "/sync/restore/v1/publish", bytes.NewReader(body))
	pub.SetBasicAuth("owner", "fixture")
	pub.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	// Failure after derived cleanup must roll back both the library and indexes,
	// and must not discard the in-memory vectors.
	if _, err = db.Exec(`CREATE TRIGGER reject_replacement BEFORE DELETE ON fn_notebook BEGIN SELECT RAISE(ABORT,'injected replacement failure'); END`); err != nil {
		t.Fatal(err)
	}
	h.ServeHTTP(out, pub)
	if out.Code != 503 || cleared.Load() != 0 || len(vectors.AllEmbeddings()) != 2 {
		t.Fatal("failed publication changed cache", out.Code, cleared.Load())
	}
	var retained int
	if err = db.QueryRow(`SELECT COUNT(*) FROM note_content`).Scan(&retained); err != nil || retained != 2 {
		t.Fatal("derived rollback failed", retained, err)
	}
	if _, err = db.Exec(`DROP TRIGGER reject_replacement`); err != nil {
		t.Fatal(err)
	}
	pub = httptest.NewRequest("POST", "/sync/restore/v1/publish", bytes.NewReader(body))
	pub.SetBasicAuth("owner", "fixture")
	pub.Header.Set("Content-Type", "application/json")
	out = httptest.NewRecorder()
	h.ServeHTTP(out, pub)
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	var baseline libraryrestore.Baseline
	if err = json.Unmarshal(out.Body.Bytes(), &baseline); err != nil {
		t.Fatal(err)
	}
	if baseline.Generation == generation || starts.Load() != 3 {
		t.Fatal("replacement did not create fresh lifetime", baseline, starts.Load())
	}
	cache := vectors.AllEmbeddings()
	if cleared.Load() != 1 || len(cache) != 1 || cache[0].NotePath != "boox.note" {
		t.Fatal("wrong cache invalidation", cleared.Load(), cache)
	}
	for _, table := range []string{"note_content", "note_embeddings"} {
		var path string
		if err = db.QueryRow("SELECT note_path FROM " + table).Scan(&path); err != nil || path != "boox.note" {
			t.Fatal("wrong derived scope", table, path, err)
		}
	}
	var count int
	var unrelated string
	if err = db.QueryRow("SELECT COUNT(*) FROM fn_notebook").Scan(&count); err != nil || count != 0 {
		t.Fatal("library not replaced", count, err)
	}
	if err = db.QueryRow("SELECT value FROM unrelated_settings").Scan(&unrelated); err != nil || unrelated != "preserve" {
		t.Fatal("unrelated data changed", unrelated, err)
	}
	if out = call(h, "GET", "/sync/restore/v1/publications/"+requestID, publisher, nil); out.Code != 200 {
		t.Fatal("lost-response receipt", out.Code, out.Body.String())
	}
	h.Close()
	hash, _ := bcrypt.GenerateFromPassword([]byte("fixture"), bcrypt.MinCost)
	h, err = New(ctx, db, Options{Account: auth.New("owner", string(hash)), Worker: readerstore.DefaultWorkerOptions(), Restore: true, StageRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, path := range []string{"/sync/capabilities", "/reader/search", "/sync/assets/v1/" + id, "/sync/v1"} {
		if out = call(h, "GET", path, peerToken, nil); out.Code != 409 {
			t.Fatal("old peer bypass", path, out.Code, out.Body.String())
		}
	}
	if out = call(h, "GET", "/sync/restore/v1/state", peerToken, nil); out.Code != 200 || !strings.Contains(out.Body.String(), `"needs_adoption":true`) {
		t.Fatal(out.Code, out.Body.String())
	}
	freshToken := syncidentity.TokenPrefix + strings.Repeat("c", 64)
	freshHash := sha256.Sum256([]byte(freshToken))
	adopt, _ := json.Marshal(libraryrestore.Adoption{Generation: baseline.Generation, Snapshot: id, Site: "0000000000000000000000000C", TokenHash: hex.EncodeToString(freshHash[:])})
	if out = call(h, "POST", "/sync/restore/v1/adopt", peerToken, adopt); out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	if out = call(h, "GET", "/sync/capabilities", freshToken, nil); out.Code != 200 {
		t.Fatal("new peer denied", out.Code)
	}
}
