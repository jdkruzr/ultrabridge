package handlers

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sysop/ultrabridge/internal/digeststore"
	"github.com/sysop/ultrabridge/internal/spcserver/auth"
	"github.com/sysop/ultrabridge/internal/spcserver/dto"
	"github.com/sysop/ultrabridge/internal/spcserver/oss"
	"github.com/sysop/ultrabridge/internal/spcserver/staging"
)

const testUID = "42"

func newSummaryHandler(t *testing.T) (*SummaryHandler, string) {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "n.db")
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", dbPath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if err := digeststore.Migrate(ctx, db); err != nil {
		t.Fatalf("digest migrate: %v", err)
	}
	if err := staging.Migrate(ctx, db); err != nil {
		t.Fatalf("staging migrate: %v", err)
	}
	return &SummaryHandler{
		Store:   digeststore.New(db),
		Root:    root,
		Signer:  &oss.Signer{Secret: "test-secret"},
		Staging: &staging.Store{Root: root, DB: db},
	}, root
}

func post(body string) *http.Request {
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	return r.WithContext(auth.WithUserID(r.Context(), testUID))
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %T: %v (body=%s)", v, err, w.Body.String())
	}
	return v
}

func TestAddThenQueryHash(t *testing.T) {
	h, _ := newSummaryHandler(t)

	w := httptest.NewRecorder()
	h.AddSummary(w, post(`{"uniqueIdentifier":"u1","content":"hello","md5Hash":"m1","sourceType":2,"metadata":"{\"note_page\":\"3\"}","lastModifiedTime":500}`))
	add := decode[dto.AddSummaryVO](t, w)
	if !add.Success || add.ID == 0 {
		t.Fatalf("add failed: %+v", add)
	}

	w = httptest.NewRecorder()
	h.QuerySummaryHash(w, post(`{"page":1,"size":20}`))
	hash := decode[dto.QuerySummaryMD5HashVO](t, w)
	if hash.TotalRecords != 1 || len(hash.SummaryInfoVOList) != 1 {
		t.Fatalf("hash query: %+v", hash)
	}
	info := hash.SummaryInfoVOList[0]
	if info.MD5Hash != "m1" || info.ID != add.ID {
		t.Errorf("info mismatch: %+v", info)
	}
	if info.MetadataMap["note_page"] != "3" {
		t.Errorf("metadataMap not parsed: %+v", info.MetadataMap)
	}
}

func TestAddIdempotentOnUniqueIdentifier(t *testing.T) {
	h, _ := newSummaryHandler(t)
	body := `{"uniqueIdentifier":"dup","content":"v1","md5Hash":"a"}`

	w := httptest.NewRecorder()
	h.AddSummary(w, post(body))
	first := decode[dto.AddSummaryVO](t, w)

	w = httptest.NewRecorder()
	h.AddSummary(w, post(`{"uniqueIdentifier":"dup","content":"v2","md5Hash":"b"}`))
	second := decode[dto.AddSummaryVO](t, w)

	if first.ID != second.ID {
		t.Fatalf("re-push should reuse row: %d vs %d", first.ID, second.ID)
	}
	w = httptest.NewRecorder()
	h.QuerySummary(w, post(`{"page":1,"size":20}`))
	q := decode[dto.QuerySummaryVO](t, w)
	if q.TotalRecords != 1 {
		t.Fatalf("want 1 row after re-push, got %d", q.TotalRecords)
	}
	if q.SummaryDOList[0].Content != "v2" || q.SummaryDOList[0].MD5Hash != "b" {
		t.Errorf("re-push didn't update: %+v", q.SummaryDOList[0])
	}
}

func TestItemIdempotentOnMetadataUID(t *testing.T) {
	// Real wire shape: items send empty top-level uniqueIdentifier; the stable
	// identity is metadata.unique_identifier. A re-push must update, not duplicate.
	h, _ := newSummaryHandler(t)
	body := func(content, md5 string) string {
		return fmt.Sprintf(`{"content":%q,"md5Hash":%q,"sourceType":2,"metadata":"{\"author\":\"Bob\",\"unique_identifier\":\"d10ab4ca\"}"}`, content, md5)
	}

	w := httptest.NewRecorder()
	h.AddSummary(w, post(body("Write", "m1")))
	first := decode[dto.AddSummaryVO](t, w)

	w = httptest.NewRecorder()
	h.AddSummary(w, post(body("Write-edited", "m2")))
	second := decode[dto.AddSummaryVO](t, w)

	if first.ID != second.ID {
		t.Fatalf("item re-push must reuse row by metadata.unique_identifier: %d vs %d", first.ID, second.ID)
	}
	w = httptest.NewRecorder()
	h.QuerySummary(w, post(`{"page":0,"size":0}`))
	q := decode[dto.QuerySummaryVO](t, w)
	if q.TotalRecords != 1 {
		t.Fatalf("want 1 row after item re-push, got %d", q.TotalRecords)
	}
	if q.SummaryDOList[0].Content != "Write-edited" || q.SummaryDOList[0].UniqueIdentifier != "d10ab4ca" {
		t.Errorf("item dedup/update wrong (uid should be lifted from metadata): %+v", q.SummaryDOList[0])
	}
}

func TestUpdatePreservesUniqueIdentifier(t *testing.T) {
	h, _ := newSummaryHandler(t)
	w := httptest.NewRecorder()
	h.AddSummary(w, post(`{"uniqueIdentifier":"keep","content":"orig","md5Hash":"a"}`))
	id := decode[dto.AddSummaryVO](t, w).ID

	w = httptest.NewRecorder()
	h.UpdateSummary(w, post(fmt.Sprintf(`{"id":%d,"content":"edited","md5Hash":"z","lastModifiedTime":900}`, id)))

	w = httptest.NewRecorder()
	h.QuerySummaryByID(w, post(fmt.Sprintf(`{"ids":[%d]}`, id)))
	q := decode[dto.QuerySummaryByIdVO](t, w)
	if len(q.SummaryDOList) != 1 {
		t.Fatalf("want 1, got %d", len(q.SummaryDOList))
	}
	d := q.SummaryDOList[0]
	if d.Content != "edited" || d.MD5Hash != "z" || d.UniqueIdentifier != "keep" {
		t.Errorf("update wrong (uniqueIdentifier must survive): %+v", d)
	}
	if d.IsSummaryGroup != "N" || d.IsDeleted != "N" {
		t.Errorf("Y/N flags wrong: %+v", d)
	}
}

func TestDeleteHidesFromHash(t *testing.T) {
	h, _ := newSummaryHandler(t)
	w := httptest.NewRecorder()
	h.AddSummary(w, post(`{"uniqueIdentifier":"d1","content":"x","md5Hash":"a"}`))
	id := decode[dto.AddSummaryVO](t, w).ID

	w = httptest.NewRecorder()
	h.DeleteSummary(w, post(fmt.Sprintf(`{"id":%d}`, id)))

	w = httptest.NewRecorder()
	h.QuerySummaryHash(w, post(`{"page":1,"size":20}`))
	if decode[dto.QuerySummaryMD5HashVO](t, w).TotalRecords != 0 {
		t.Error("deleted digest still in hash query")
	}
}

func TestGroupsSeparateFromItems(t *testing.T) {
	h, _ := newSummaryHandler(t)
	w := httptest.NewRecorder()
	h.AddSummary(w, post(`{"uniqueIdentifier":"i1","content":"item","md5Hash":"a"}`))
	w = httptest.NewRecorder()
	h.AddSummaryGroup(w, post(`{"uniqueIdentifier":"g1","name":"Lib","md5Hash":"g"}`))
	grpAdd := decode[dto.AddSummaryGroupVO](t, w)
	if grpAdd.ID == 0 {
		t.Fatal("group add failed")
	}

	w = httptest.NewRecorder()
	h.QuerySummary(w, post(`{"page":1,"size":20}`))
	if decode[dto.QuerySummaryVO](t, w).TotalRecords != 1 {
		t.Error("query/summary should return only the item")
	}
	w = httptest.NewRecorder()
	h.QuerySummaryGroup(w, post(`{"page":1,"size":20}`))
	groups := decode[dto.QuerySummaryGroupVO](t, w)
	if groups.TotalRecords != 1 || groups.SummaryDOList[0].IsSummaryGroup != "Y" {
		t.Errorf("group query wrong: %+v", groups)
	}
}

func TestDeleteGroupCascades(t *testing.T) {
	h, _ := newSummaryHandler(t)
	w := httptest.NewRecorder()
	h.AddSummaryGroup(w, post(`{"uniqueIdentifier":"grp","name":"L","md5Hash":"g"}`))
	gid := decode[dto.AddSummaryGroupVO](t, w).ID
	w = httptest.NewRecorder()
	h.AddSummary(w, post(`{"uniqueIdentifier":"child","parentUniqueIdentifier":"grp","content":"c","md5Hash":"a"}`))

	w = httptest.NewRecorder()
	h.DeleteSummaryGroup(w, post(fmt.Sprintf(`{"id":%d}`, gid)))

	w = httptest.NewRecorder()
	h.QuerySummaryHash(w, post(`{"page":1,"size":20}`))
	if decode[dto.QuerySummaryMD5HashVO](t, w).TotalRecords != 0 {
		t.Error("group delete should cascade to member items")
	}
}

func TestTagLifecycle(t *testing.T) {
	h, _ := newSummaryHandler(t)
	w := httptest.NewRecorder()
	h.AddSummaryTag(w, post(`{"name":"work"}`))
	tagID := decode[dto.AddSummaryTagVO](t, w).ID

	// Idempotent on name.
	w = httptest.NewRecorder()
	h.AddSummaryTag(w, post(`{"name":"work"}`))
	if decode[dto.AddSummaryTagVO](t, w).ID != tagID {
		t.Error("duplicate tag name should reuse id")
	}

	w = httptest.NewRecorder()
	h.UpdateSummaryTag(w, post(fmt.Sprintf(`{"id":%d,"name":"home"}`, tagID)))

	w = httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	h.QuerySummaryTag(w, r.WithContext(auth.WithUserID(r.Context(), testUID)))
	tags := decode[dto.QuerySummaryTagVO](t, w)
	if len(tags.SummaryTagDOList) != 1 || tags.SummaryTagDOList[0].Name != "home" {
		t.Errorf("tag list wrong: %+v", tags.SummaryTagDOList)
	}

	w = httptest.NewRecorder()
	h.DeleteSummaryTag(w, post(fmt.Sprintf(`{"id":%d}`, tagID)))
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/", nil)
	h.QuerySummaryTag(w, r.WithContext(auth.WithUserID(r.Context(), testUID)))
	if len(decode[dto.QuerySummaryTagVO](t, w).SummaryTagDOList) != 0 {
		t.Error("tag not deleted")
	}
}

func TestMarkBlobRoundTrip(t *testing.T) {
	h, root := newSummaryHandler(t)

	// 1. apply → innerName + signed upload URL.
	w := httptest.NewRecorder()
	h.UploadApplySummary(w, post(`{"fileName":"draw.mark"}`))
	apply := decode[dto.UploadSummaryApplyVO](t, w)
	if apply.InnerName == "" || !strings.HasSuffix(apply.InnerName, ".mark") {
		t.Fatalf("bad innerName: %q", apply.InnerName)
	}
	if !strings.Contains(apply.FullUploadURL, "/api/oss/upload") || !strings.Contains(apply.FullUploadURL, "signature=") {
		t.Fatalf("bad upload url: %q", apply.FullUploadURL)
	}

	// 2. simulate the device's POST to /api/oss/upload landing in .staging.
	blob := []byte("handwriting-strokes")
	if _, err := h.Staging.Stage(apply.InnerName, strings.NewReader(string(blob))); err != nil {
		t.Fatalf("stage: %v", err)
	}
	sum := md5.Sum(blob)
	markMD5 := hex.EncodeToString(sum[:])

	// 3. add/summary referencing the handwriting promotes it to .digests.
	w = httptest.NewRecorder()
	h.AddSummary(w, post(fmt.Sprintf(
		`{"uniqueIdentifier":"hw","content":"c","md5Hash":"a","commentHandwriteName":"draw.mark","handwriteInnerName":%q,"handwriteMD5":%q}`,
		apply.InnerName, markMD5)))
	id := decode[dto.AddSummaryVO](t, w).ID

	promoted := filepath.Join(root, ".digests", apply.InnerName)
	got, err := os.ReadFile(promoted)
	if err != nil {
		t.Fatalf("promoted blob missing: %v", err)
	}
	if string(got) != string(blob) {
		t.Errorf("promoted blob bytes differ: %q", got)
	}

	// 4. download/summary mints a signed GET for the blob.
	w = httptest.NewRecorder()
	h.DownloadSummary(w, post(fmt.Sprintf(`{"id":%d}`, id)))
	dl := decode[dto.DownloadSummaryVO](t, w)
	if !dl.Success || !strings.Contains(dl.URL, "/api/oss/download") || !strings.Contains(dl.URL, "signature=") {
		t.Fatalf("bad download VO: %+v", dl)
	}
}

func TestParseMetadataMapPreservesNumbers(t *testing.T) {
	// Device-confirmed: numeric metadata (e.g. source_size 18992668) must not be
	// coerced through float64 into "1.8992668e+07".
	m := parseMetadataMap(`{"source_size":18992668,"note_page":12,"author":"greg","unique_identifier":"u1"}`)
	if m["source_size"] != "18992668" {
		t.Errorf("source_size corrupted: %q", m["source_size"])
	}
	if m["note_page"] != "12" {
		t.Errorf("note_page corrupted: %q", m["note_page"])
	}
	if m["author"] != "greg" || m["unique_identifier"] != "u1" {
		t.Errorf("string fields wrong: %+v", m)
	}
	if parseMetadataMap("") != nil || parseMetadataMap("not json") != nil {
		t.Error("empty/invalid metadata should yield nil")
	}
}

func TestDownloadSummaryNoHandwrite(t *testing.T) {
	h, _ := newSummaryHandler(t)
	w := httptest.NewRecorder()
	h.AddSummary(w, post(`{"uniqueIdentifier":"nohw","content":"c","md5Hash":"a"}`))
	id := decode[dto.AddSummaryVO](t, w).ID

	w = httptest.NewRecorder()
	h.DownloadSummary(w, post(fmt.Sprintf(`{"id":%d}`, id)))
	dl := decode[dto.DownloadSummaryVO](t, w)
	if dl.Success || dl.ErrorCode != errFileNotExistCode {
		t.Errorf("want E0321 for digest without handwriting, got %+v", dl)
	}
}
