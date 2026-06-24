package remarkable

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sysop/ultrabridge/internal/source"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "remarkable.db") + "?_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestNewSource_RequiresDataPath(t *testing.T) {
	db := testDB(t)
	if _, err := NewSource(db, source.SourceRow{ConfigJSON: `{"pairing_code":"123456"}`}, source.SharedDeps{}); err == nil {
		t.Fatal("NewSource succeeded without data_path, want error")
	}
}

func TestProtocol_BetaSettingsProbeIsUnauthenticated(t *testing.T) {
	db := testDB(t)
	row := source.SourceRow{
		Type:       "remarkable",
		Name:       "RM",
		ConfigJSON: `{"data_path":"` + t.TempDir() + `","pairing_code":"123456"}`,
	}
	src, err := NewSource(db, row, source.SharedDeps{})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if err := src.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(src.Stop)

	mux := http.NewServeMux()
	src.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/settings/v1/beta", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET beta settings = %d body=%s", w.Code, w.Body.String())
	}
	var got map[string]bool
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode beta settings: %v", err)
	}
	if got["enrolled"] || !got["available"] {
		t.Fatalf("beta settings = %+v", got)
	}

	req = httptest.NewRequest(http.MethodPost, "/settings/v1/beta", strings.NewReader(`{"enrolled":true}`))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST beta settings = %d body=%s", w.Code, w.Body.String())
	}
}

func TestProtocol_MultiDeviceRootConverges(t *testing.T) {
	db := testDB(t)
	row := source.SourceRow{
		Type:       "remarkable",
		Name:       "RM",
		ConfigJSON: `{"data_path":"` + t.TempDir() + `","pairing_code":"123456"}`,
	}
	src, err := NewSource(db, row, source.SharedDeps{})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if err := src.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	mux := http.NewServeMux()
	src.RegisterRoutes(mux)

	pair := func(deviceID, desc string) string {
		body, _ := json.Marshal(map[string]string{
			"code":       "123456",
			"deviceDesc": desc,
			"deviceID":   deviceID,
		})
		req := httptest.NewRequest(http.MethodPost, "/token/json/2/device/new", bytes.NewReader(body))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("pair %s = %d body=%s", deviceID, w.Code, w.Body.String())
		}
		deviceToken := w.Body.String()

		req = httptest.NewRequest(http.MethodPost, "/token/json/2/user/new", nil)
		req.Header.Set("Authorization", "Bearer "+deviceToken)
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("user token %s = %d body=%s", deviceID, w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	userA := pair("rm-device-a", "reMarkable 2")
	userB := pair("rm-device-b", "reMarkable Paper Pro")

	putRootBody, _ := json.Marshal(map[string]any{
		"generation": 0,
		"hash":       "root-hash-a",
		"broadcast":  true,
	})
	req := httptest.NewRequest(http.MethodPut, "/sync/v3/root", bytes.NewReader(putRootBody))
	req.Header.Set("Authorization", "Bearer "+userA)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT root = %d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/sync/v3/root", nil)
	req.Header.Set("Authorization", "Bearer "+userB)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET root = %d body=%s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode root response: %v", err)
	}
	if got["hash"] != "root-hash-a" {
		t.Fatalf("root hash = %v, want root-hash-a", got["hash"])
	}

	devices, err := src.Devices(context.Background())
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("device count = %d, want 2", len(devices))
	}
}

func TestProtocol_LegacyDocumentFlow(t *testing.T) {
	db := testDB(t)
	dataDir := t.TempDir()
	row := source.SourceRow{
		Type:       "remarkable",
		Name:       "RM",
		ConfigJSON: `{"data_path":"` + dataDir + `","pairing_code":"123456"}`,
	}
	src, err := NewSource(db, row, source.SharedDeps{})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if err := src.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	mux := http.NewServeMux()
	src.RegisterRoutes(mux)

	userToken := pairUserToken(t, mux, "rm-device-docs", "reMarkable 2")

	meta := []map[string]any{{
		"ID":             "doc-1",
		"Version":        3,
		"ModifiedClient": "2026-06-20T12:00:00Z",
		"Type":           "DocumentType",
		"VissibleName":   "Project Plan",
		"CurrentPage":    4,
		"Bookmarked":     true,
		"Parent":         "",
	}}
	metaBody, _ := json.Marshal(meta)
	req := httptest.NewRequest(http.MethodPut, "/document-storage/json/2/upload/update-status", bytes.NewReader(metaBody))
	req.Header.Set("Authorization", "Bearer "+userToken)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update-status = %d body=%s", w.Code, w.Body.String())
	}

	uploadReq, _ := json.Marshal([]map[string]any{{"ID": "doc-1", "Version": 3}})
	req = httptest.NewRequest(http.MethodPut, "/document-storage/json/2/upload/request", bytes.NewReader(uploadReq))
	req.Header.Set("Authorization", "Bearer "+userToken)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload-request = %d body=%s", w.Code, w.Body.String())
	}
	var uploads []map[string]any
	if err := json.NewDecoder(w.Body).Decode(&uploads); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	putURL, ok := uploads[0]["BlobURLPut"].(string)
	if !ok || putURL == "" {
		t.Fatalf("BlobURLPut missing: %+v", uploads)
	}
	req = httptest.NewRequest(http.MethodPut, putURL, strings.NewReader("pdf-bytes"))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("storage put = %d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/document-storage/json/2/docs?withBlob=true", nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list docs = %d body=%s", w.Code, w.Body.String())
	}
	var docs []map[string]any
	if err := json.NewDecoder(w.Body).Decode(&docs); err != nil {
		t.Fatalf("decode docs: %v", err)
	}
	if len(docs) != 1 || docs[0]["VissibleName"] != "Project Plan" {
		t.Fatalf("docs = %+v", docs)
	}
	getURL, ok := docs[0]["BlobURLGet"].(string)
	if !ok || getURL == "" {
		t.Fatalf("BlobURLGet missing: %+v", docs)
	}
	req = httptest.NewRequest(http.MethodGet, getURL, nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("storage get = %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "pdf-bytes" {
		t.Fatalf("document bytes = %q, want pdf-bytes", got)
	}

	deleteBody, _ := json.Marshal([]map[string]string{{"ID": "doc-1"}})
	req = httptest.NewRequest(http.MethodPut, "/document-storage/json/2/delete", bytes.NewReader(deleteBody))
	req.Header.Set("Authorization", "Bearer "+userToken)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete doc = %d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/document-storage/json/2/docs", nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list docs after delete = %d body=%s", w.Code, w.Body.String())
	}
	docs = nil
	if err := json.NewDecoder(w.Body).Decode(&docs); err != nil {
		t.Fatalf("decode docs after delete: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("docs after delete = %+v, want empty", docs)
	}
}

func TestProtocol_LegacyMetadataRefreshesSearchIndex(t *testing.T) {
	db := testDB(t)
	dataDir := t.TempDir()
	idx := &recordingMetadataIndex{}
	row := source.SourceRow{
		Type:       "remarkable",
		Name:       "RM",
		ConfigJSON: `{"data_path":"` + dataDir + `","pairing_code":"123456"}`,
	}
	src, err := NewSource(db, row, source.SharedDeps{Indexer: idx})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if err := src.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	mux := http.NewServeMux()
	src.RegisterRoutes(mux)
	userToken := pairUserToken(t, mux, "rm-device-index", "reMarkable 2")

	metaBody, _ := json.Marshal([]map[string]any{{
		"ID":             "doc-indexed",
		"Version":        1,
		"ModifiedClient": "2026-06-20T12:00:00Z",
		"Type":           "DocumentType",
		"VissibleName":   "Indexed Plan",
	}})
	req := httptest.NewRequest(http.MethodPut, "/document-storage/json/2/upload/update-status", bytes.NewReader(metaBody))
	req.Header.Set("Authorization", "Bearer "+userToken)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update-status = %d body=%s", w.Code, w.Body.String())
	}
	if len(idx.calls) != 1 || idx.calls[0].path != "remarkable://doc-indexed" || idx.calls[0].titleText != "Indexed Plan" {
		t.Fatalf("index calls = %+v", idx.calls)
	}

	deleteBody, _ := json.Marshal([]map[string]string{{"ID": "doc-indexed"}})
	req = httptest.NewRequest(http.MethodPut, "/document-storage/json/2/delete", bytes.NewReader(deleteBody))
	req.Header.Set("Authorization", "Bearer "+userToken)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete = %d body=%s", w.Code, w.Body.String())
	}
	if len(idx.deleted) != 1 || idx.deleted[0] != "remarkable://doc-indexed" {
		t.Fatalf("deleted = %+v", idx.deleted)
	}
}

func TestProtocol_BlobSignedURLAndDirectRead(t *testing.T) {
	db := testDB(t)
	row := source.SourceRow{
		Type:       "remarkable",
		Name:       "RM",
		ConfigJSON: `{"data_path":"` + t.TempDir() + `","pairing_code":"123456"}`,
	}
	src, err := NewSource(db, row, source.SharedDeps{})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if err := src.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	mux := http.NewServeMux()
	src.RegisterRoutes(mux)

	userToken := pairUserToken(t, mux, "rm-device-blobs", "reMarkable Paper Pro")
	reqBody, _ := json.Marshal(map[string]any{
		"http_method":   "PUT",
		"initial_sync":  false,
		"relative_path": "blob-a",
	})
	req := httptest.NewRequest(http.MethodPost, "/sync/v2/signed-urls/uploads", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+userToken)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("signed-url upload = %d body=%s", w.Code, w.Body.String())
	}
	var signed map[string]any
	if err := json.NewDecoder(w.Body).Decode(&signed); err != nil {
		t.Fatalf("decode signed upload: %v", err)
	}
	putURL := signed["url"].(string)
	req = httptest.NewRequest(http.MethodPut, putURL, strings.NewReader("blob-contents"))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("blob put = %d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/sync/v3/files/blob-a", nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("direct blob get = %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "blob-contents" {
		t.Fatalf("blob body = %q, want blob-contents", got)
	}

	reqBody, _ = json.Marshal(map[string]any{
		"http_method":   "GET",
		"initial_sync":  false,
		"relative_path": "blob-a",
	})
	req = httptest.NewRequest(http.MethodPost, "/sync/v2/signed-urls/downloads", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+userToken)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("signed-url download = %d body=%s", w.Code, w.Body.String())
	}
	signed = nil
	if err := json.NewDecoder(w.Body).Decode(&signed); err != nil {
		t.Fatalf("decode signed download: %v", err)
	}
	getURL := signed["url"].(string)
	req = httptest.NewRequest(http.MethodGet, getURL, nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("blob get = %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "blob-contents" {
		t.Fatalf("signed blob body = %q, want blob-contents", got)
	}
}

func TestProtocol_HWRProxy(t *testing.T) {
	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != hwrAPIPath {
			t.Fatalf("upstream path = %q, want %q", r.URL.Path, hwrAPIPath)
		}
		data, _ := io.ReadAll(r.Body)
		upstreamBody = string(data)
		w.Header().Set("Content-Type", hwrJIIX)
		_, _ = w.Write([]byte(`{"type":"Text","label":"recognized"}`))
	}))
	defer upstream.Close()

	db := testDB(t)
	row := source.SourceRow{
		Type: "remarkable",
		Name: "RM",
		ConfigJSON: `{"data_path":"` + t.TempDir() + `","pairing_code":"123456",` +
			`"hwr_application_key":"app-key","hwr_hmac":"hmac-secret","hwr_host":"` + upstream.URL + `"}`,
	}
	src, err := NewSource(db, row, source.SharedDeps{})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if err := src.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	mux := http.NewServeMux()
	src.RegisterRoutes(mux)
	userToken := pairUserToken(t, mux, "rm-device-hwr", "reMarkable 2")

	body := `{"configuration":{"lang":"en_US"},"content":"ink"}`
	req := httptest.NewRequest(http.MethodPost, "/convert/v1/handwriting", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+userToken)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("hwr proxy = %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != hwrJIIX {
		t.Fatalf("content type = %q, want %q", got, hwrJIIX)
	}
	if upstreamBody != body {
		t.Fatalf("upstream body = %s, want %s", upstreamBody, body)
	}
	if got := w.Body.String(); got != `{"type":"Text","label":"recognized"}` {
		t.Fatalf("response = %s", got)
	}
}

func TestProtocol_HWRRequiresAuthAndConfig(t *testing.T) {
	db := testDB(t)
	row := source.SourceRow{
		Type:       "remarkable",
		Name:       "RM",
		ConfigJSON: `{"data_path":"` + t.TempDir() + `","pairing_code":"123456"}`,
	}
	src, err := NewSource(db, row, source.SharedDeps{})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if err := src.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	mux := http.NewServeMux()
	src.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/page", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauth hwr = %d body=%s", w.Code, w.Body.String())
	}

	userToken := pairUserToken(t, mux, "rm-device-hwr-unconfigured", "reMarkable 2")
	req = httptest.NewRequest(http.MethodPost, "/api/v1/page", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+userToken)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("unconfigured hwr = %d body=%s", w.Code, w.Body.String())
	}
}

func pairUserToken(t *testing.T, mux *http.ServeMux, deviceID, desc string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"code":       "123456",
		"deviceDesc": desc,
		"deviceID":   deviceID,
	})
	req := httptest.NewRequest(http.MethodPost, "/token/json/2/device/new", bytes.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("pair %s = %d body=%s", deviceID, w.Code, w.Body.String())
	}
	deviceToken := w.Body.String()

	req = httptest.NewRequest(http.MethodPost, "/token/json/2/user/new", nil)
	req.Header.Set("Authorization", "Bearer "+deviceToken)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("user token %s = %d body=%s", deviceID, w.Code, w.Body.String())
	}
	return w.Body.String()
}

var _ source.Source = (*Source)(nil)
