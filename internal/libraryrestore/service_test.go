package libraryrestore

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jdkruzr/rhizome/server-go/assets"
	"github.com/jdkruzr/rhizome/server-go/registry"
	"github.com/sysop/ultrabridge/internal/readercontract"
	"github.com/sysop/ultrabridge/internal/readersearch"
	"github.com/sysop/ultrabridge/internal/readerstore"
	"github.com/sysop/ultrabridge/internal/syncgeneration"
	"github.com/sysop/ultrabridge/internal/syncidentity"
	"github.com/sysop/ultrabridge/internal/syncstore"
	_ "modernc.org/sqlite"
)

const pub = "0000000000000000000000000A"
const peer = "0000000000000000000000000B"
const fresh = "0000000000000000000000000C"

var ctx = context.Background()

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func token(site string) string {
	return syncidentity.TokenPrefix + strings.Repeat(strings.ToLower(site[len(site)-1:]), 64)
}
func tokenHash(site string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(token(site)))) }
func fixture(t *testing.T) *Service {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "live.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	must(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, init := range []func(context.Context, *sql.DB) error{syncstore.Migrate, readerstore.Install, readersearch.Install, syncidentity.Install, Install, func(c context.Context, d *sql.DB) error { return (&assets.SQLStore{DB: d}).Migrate(c) }} {
		must(t, init(ctx, db))
	}
	for _, site := range []string{pub, peer} {
		must(t, (syncidentity.Store{DB: db}).Enroll(ctx, syncidentity.Enrollment{SiteID: site, TokenHash: tokenHash(site)}))
	}
	_, err = db.Exec(`CREATE TABLE unrelated_settings(value TEXT);INSERT INTO unrelated_settings VALUES('keep me');INSERT INTO fn_notebook(id,name,lww_wall_ts,lww_op_seq,lww_site_id) VALUES('0000000000000000000000000Z','discard me',99,1,?)`, peer)
	must(t, err)
	return &Service{DB: db, StageRoot: t.TempDir(), Exclusive: func(c context.Context, fn func() error) error { return fn() }}
}
func row(t *testing.T, table, id, author string, seq int64, overrides map[string]any) []byte {
	cols := map[string]any{}
	for _, c := range readercontract.CandidateCombined().ByName()[table].Columns {
		if c.Nullable {
			cols[c.Name] = nil
		} else {
			switch c.Type {
			case registry.Text:
				cols[c.Name] = ""
			case registry.Blob:
				cols[c.Name] = ""
			default:
				cols[c.Name] = int64(0)
			}
		}
	}
	for k, v := range overrides {
		cols[k] = v
	}
	raw, err := json.Marshal(map[string]any{"table": table, "pk": id, "site_id": author, "op_seq": seq, "op_ts": int64(9007199254740993), "cols": cols})
	must(t, err)
	return raw
}
func archive(t *testing.T, s *Service, rows [][]byte, books map[string][]byte) string {
	return archiveManifest(t, s, rows, books, Manifest{1, readercontract.CandidateCombined().SchemaHash(), pub, 20})
}
func archiveManifest(t *testing.T, s *Service, rows [][]byte, books map[string][]byte, manifest any) string {
	var out bytes.Buffer
	z := zip.NewWriter(&out)
	m, err := z.Create("manifest.json")
	must(t, err)
	must(t, json.NewEncoder(m).Encode(manifest))
	r, err := z.Create("rows.jsonl")
	must(t, err)
	for _, v := range rows {
		_, err = r.Write(append(v, '\n'))
		must(t, err)
	}
	for id, b := range books {
		w, err := z.Create("assets/" + id)
		must(t, err)
		_, err = w.Write(b)
		must(t, err)
	}
	must(t, z.Close())
	return upload(t, s, out.Bytes())
}
func upload(t *testing.T, s *Service, b []byte) string {
	a := &assets.SQLStore{DB: s.DB}
	id := assets.Digest(b)
	d := assets.Descriptor{ID: id, ByteLength: int64(len(b)), ChunkBytes: assets.ChunkBytes}
	_, _, err := a.Stage(ctx, d)
	must(t, err)
	for n := int64(0); n < d.ChunkCount(); n++ {
		l, _ := d.ChunkLength(n)
		chunk := b[n*assets.ChunkBytes : n*assets.ChunkBytes+int64(l)]
		must(t, a.WriteChunk(ctx, id, n, chunk, assets.Digest(chunk)))
	}
	_, err = a.Complete(ctx, id)
	must(t, err)
	return id
}
func request(t *testing.T, s *Service, id string) syncgeneration.Request {
	g, err := syncgeneration.Current(ctx, s.DB)
	must(t, err)
	return syncgeneration.Request{ID: strings.Repeat("1", 64), Expected: g, SnapshotHash: id, Publisher: pub}
}
func scalar(t *testing.T, s *Service, q string) string {
	var v string
	must(t, s.DB.QueryRow(q).Scan(&v))
	return v
}

func TestPublicationReplacesOnlyLibraryAndAdoptionFencesOldWork(t *testing.T) {
	s := fixture(t)
	book := bytes.Repeat([]byte("NO PANCAKES."), 100000)
	id := assets.Digest(book)
	rows := [][]byte{row(t, "notebook", pub, peer, 7, map[string]any{"name": "restored", "page_width": int64(10000), "page_height": int64(15000)}), row(t, "reader_book", id, peer, 8, map[string]any{"asset_id": id, "byte_length": len(book), "media_type": "application/epub+zip", "metadata_json": `{"version":1,"title":"A book"}`})}
	r := request(t, s, archive(t, s, rows, map[string][]byte{id: book}))
	b, err := s.Publish(ctx, r)
	must(t, err)
	if b.Generation == r.Expected || b.Cursor != 2 || b.HighWater != 20 {
		t.Fatalf("bad baseline: %+v", b)
	}
	if scalar(t, s, "SELECT name FROM fn_notebook") != "restored" || scalar(t, s, "SELECT count(*) FROM fn_notebook") != "1" || scalar(t, s, "SELECT value FROM unrelated_settings") != "keep me" {
		t.Fatal("wrong replacement scope")
	}
	if scalar(t, s, "SELECT lww_wall_ts FROM fn_notebook") != "9007199254740993" || scalar(t, s, "SELECT lww_site_id FROM fn_notebook") != peer {
		t.Fatal("provenance lost")
	}
	asset, err := (&assets.SQLStore{DB: s.DB}).Describe(ctx, id)
	must(t, err)
	if asset.State != "ready" || asset.ByteLength != int64(len(book)) {
		t.Fatal("book lost")
	}
	if _, _, err = syncgeneration.Admit(ctx, s.DB, peer); !errors.Is(err, syncgeneration.ErrReplaced) {
		t.Fatal("old work admitted")
	}
	// Lost publication response doesn't invoke workers or publish twice.
	s.Exclusive = func(context.Context, func() error) error { t.Fatal("retry stopped workers"); return nil }
	retry, err := s.Publish(ctx, r)
	must(t, err)
	if retry != b {
		t.Fatal("retry changed baseline")
	}
	a := Adoption{Generation: b.Generation, Snapshot: b.Snapshot, Site: fresh, TokenHash: tokenHash(fresh)}
	adopted, err := s.Adopt(ctx, peer, a)
	must(t, err)
	if adopted != b {
		t.Fatal("wrong baseline")
	}
	_, err = s.Adopt(ctx, peer, a)
	must(t, err)
	_, _, err = syncgeneration.Admit(ctx, s.DB, fresh)
	must(t, err)
	if scalar(t, s, "SELECT acked_op_seq FROM sync_cursors WHERE site_id='"+pub+"'") != "20" || scalar(t, s, "SELECT last_pull_seq FROM sync_cursors WHERE site_id='"+fresh+"'") != "2" {
		t.Fatal("wrong resume checkpoint")
	}
	a.Site = "0000000000000000000000000D"
	if _, err = s.Adopt(ctx, peer, a); !errors.Is(err, syncgeneration.ErrConflict) {
		t.Fatal("changed adoption retry accepted")
	}
	if _, _, err = syncgeneration.Admit(ctx, s.DB, peer); !errors.Is(err, syncgeneration.ErrReplaced) {
		t.Fatal("old actor was promoted")
	}
}

func TestBadSnapshotNeverTouchesLiveLibrary(t *testing.T) {
	for _, kind := range []string{"duplicate-row", "duplicate-op", "future-publisher-op", "missing-columns", "bad-book"} {
		t.Run(kind, func(t *testing.T) {
			s := fixture(t)
			rows := [][]byte{row(t, "notebook", pub, peer, 1, map[string]any{"name": "restored"})}
			books := map[string][]byte{}
			switch kind {
			case "duplicate-row":
				rows = append(rows, rows[0])
			case "duplicate-op":
				rows = append(rows, row(t, "notebook", fresh, peer, 1, nil))
			case "future-publisher-op":
				rows = append(rows, row(t, "notebook", fresh, pub, 21, nil))
			case "missing-columns":
				rows = append(rows, []byte(`{"table":"notebook","pk":"`+fresh+`","site_id":"`+peer+`","op_seq":2,"op_ts":1,"cols":{}}`))
			case "bad-book":
				books[strings.Repeat("a", 64)] = []byte("bad")
			}
			r := request(t, s, archive(t, s, rows, books))
			if _, err := s.Publish(ctx, r); err == nil {
				t.Fatal("bad snapshot published")
			}
			g, err := syncgeneration.Current(ctx, s.DB)
			must(t, err)
			if g != r.Expected || scalar(t, s, "SELECT name FROM fn_notebook") != "discard me" {
				t.Fatal("validation altered live state")
			}
		})
	}
}

func TestMissingOrNullPublicationHighWaterIsNotAnEmptySnapshot(t *testing.T) {
	for _, null := range []bool{false, true} {
		s := fixture(t)
		m := map[string]any{"version": 1, "schema": readercontract.CandidateCombined().SchemaHash(), "publisher": pub}
		if null {
			m["high_water"] = nil
		}
		r := request(t, s, archiveManifest(t, s, nil, nil, m))
		if _, err := s.Publish(ctx, r); !errors.Is(err, ErrSnapshot) {
			t.Fatalf("missing high water accepted: %v", err)
		}
		g, err := syncgeneration.Current(ctx, s.DB)
		must(t, err)
		if g != r.Expected || scalar(t, s, "SELECT name FROM fn_notebook") != "discard me" {
			t.Fatal("invalid snapshot altered library")
		}
	}
}

func TestFailedPublicationRollsBackActualContentAndBaseline(t *testing.T) {
	s := fixture(t)
	id := archive(t, s, [][]byte{row(t, "notebook", pub, peer, 1, map[string]any{"name": "restore"})}, nil)
	r := request(t, s, id)
	// Fail after mirrors were copied, while clearing derived state.
	_, err := s.DB.Exec(`CREATE TRIGGER fail_restore BEFORE DELETE ON reader_search_documents BEGIN SELECT RAISE(ABORT,'injected'); END;INSERT INTO reader_search_documents VALUES(1,'a','b','t','q','r','x','h','[]',1)`)
	must(t, err)
	if _, err = s.Publish(ctx, r); err == nil {
		t.Fatal("failure published")
	}
	g, err := syncgeneration.Current(ctx, s.DB)
	must(t, err)
	if g != r.Expected || scalar(t, s, "SELECT name FROM fn_notebook") != "discard me" || scalar(t, s, "SELECT count(*) FROM sync_restore_baseline") != "0" {
		t.Fatal("partial publication")
	}
	_, err = s.DB.Exec(`DROP TRIGGER fail_restore`)
	must(t, err)
	_, err = s.Publish(ctx, r)
	must(t, err)
}
