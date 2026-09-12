package readerlab

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jdkruzr/rhizome/server-go/assets"
	"github.com/jdkruzr/rhizome/server-go/bounded"
	"github.com/sysop/ultrabridge/internal/auth"
	"github.com/sysop/ultrabridge/internal/syncidentity"
	"github.com/sysop/ultrabridge/internal/syncstore"
	"golang.org/x/crypto/bcrypt"
)

func TestEnrolledIdentityProtectsMixedRowsAssetsSearchAndManagement(t *testing.T) {
	db, _ := open(t, filepath.Join(t.TempDir(), "enrolled.db"))
	if err := (&assets.SQLStore{DB: db}).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.MinCost)
	admin := auth.New("admin", string(hash))
	admin.SetTokenValidator(func(string) (string, error) { return SiteA, nil }) // Even an MCP label equal to a site is NOT identity/admin authority.
	h, err := HandlerWithEnrollment(ctx, db, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }), admin)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	defer s.Close()
	call := func(method, path, credential string, body any, headers map[string]string, want int) []byte {
		t.Helper()
		var data []byte
		if body != nil {
			data, _ = json.Marshal(body)
		}
		r, _ := http.NewRequest(method, s.URL+path, bytes.NewReader(data))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(bounded.Header, "1")
		if credential == "admin" {
			r.SetBasicAuth("admin", "admin")
		} else if credential != "" {
			r.Header.Set("Authorization", "Bearer "+credential)
		}
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		res, e := s.Client().Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		data, _ = io.ReadAll(res.Body)
		if res.StatusCode != want {
			t.Fatalf("%s %s: %d want %d: %s", method, path, res.StatusCode, want, data)
		}
		return data
	}
	var secret [32]byte
	rand.Read(secret[:])
	token := syncidentity.TokenPrefix + hex.EncodeToString(secret[:])
	digest := sha256.Sum256([]byte(token))
	e := syncidentity.Enrollment{SiteID: SiteA, TokenHash: hex.EncodeToString(digest[:])}
	before := state(t, db)
	call("POST", "/sync/devices/v1/enroll", "", e, nil, 401)
	call("POST", "/sync/devices/v1/enroll", "mcp", e, nil, 403)
	call("POST", "/sync/devices/v1/enroll", "admin", e, map[string]string{"Origin": "https://untrusted.invalid"}, 403)
	call("POST", "/sync/devices/v1/enroll", "admin", e, map[string]string{"Content-Type": "text/plain"}, 415)
	call("GET", "/sync/devices/v1/enroll", "admin", nil, nil, 405)
	call("POST", "/sync/devices/v1/enroll", "admin", map[string]any{"site_id": SiteA, "token_hash": e.TokenHash, "raw_token": token}, nil, 400)
	call("POST", "/sync/devices/v1/enroll", "admin", map[string]any{"site_id": strings.Repeat("x", 4096)}, nil, 400)
	call("POST", "/sync/devices/v1/enroll", "admin", e, nil, 204)
	call("POST", "/sync/devices/v1/enroll", "admin", e, nil, 204)
	if !reflect.DeepEqual(before, state(t, db)) {
		t.Fatal("Enrollment mutated sync state")
	}
	for _, path := range []string{"/sync/v1", "/sync/capabilities", "/reader/search", "/sync/assets/v1/" + bookID, "/sync/assets/v1/" + bookID + "/chunks/0"} {
		for _, cred := range []string{"", "admin", "mcp", e.TokenHash} {
			call("GET", path, cred, nil, nil, 401)
		}
	}
	call("GET", "/sync/capabilities", token, nil, nil, 200)
	call("GET", "/reader/search", token, nil, nil, 200)
	call("POST", "/sync/devices/v1/revoke", token, map[string]string{"site_id": SiteA}, nil, 403)
	// Both a lying envelope and lying operations fail before any receipt/ACK.
	call("POST", "/sync/v1", token, request(SiteB), nil, 403)
	call("POST", "/sync/v1", token, request(SiteA, title(SiteB, 1, "spoof")), nil, 403)
	call("POST", "/sync/v1", token, request(SiteA, notebook(SiteB, 1)), nil, 403)
	if !reflect.DeepEqual(before, state(t, db)) {
		t.Fatal("Spoof mutated sync state")
	}
	call("POST", "/sync/v1", token, request(SiteA, notebook(SiteA, 1), title(SiteA, 2, "real")), nil, 200)
	call("PUT", "/sync/assets/v1/"+bookID, token, map[string]any{"asset_id": bookID, "byte_length": "10", "chunk_bytes": 262144}, nil, 201)
	// Cursor pruning is cleanup, not credential revocation or a new identity.
	if _, err := syncstore.New(db).DeleteDevice(ctx, SiteA); err != nil {
		t.Fatal(err)
	}
	call("POST", "/sync/v1", token, request(SiteA), nil, 200)
	before = state(t, db)
	call("POST", "/sync/devices/v1/revoke", "admin", map[string]string{"site_id": SiteA}, nil, 204)
	for _, path := range []string{"/sync/v1", "/sync/capabilities", "/reader/search", "/sync/assets/v1/" + bookID, "/sync/assets/v1/" + bookID + "/chunks/0"} {
		call("GET", path, token, nil, nil, 401)
	}
	call("POST", "/sync/devices/v1/enroll", "admin", e, nil, 409)
	if !reflect.DeepEqual(before, state(t, db)) {
		t.Fatal("Revocation deleted history or changed ACK")
	}
}
