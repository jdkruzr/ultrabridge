package readerlab

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jdkruzr/rhizome/server-go/bounded"
	"github.com/sysop/ultrabridge/internal/syncgeneration"
	"github.com/sysop/ultrabridge/internal/syncidentity"
)

func TestAdmittedMixedPushCannotResurrectPreviousGeneration(t *testing.T) {
	db, _ := open(t, filepath.Join(t.TempDir(), "restore.db"))
	if err := syncidentity.Install(ctx, db); err != nil {
		t.Fatal(err)
	}
	identities := syncidentity.Store{DB: db}
	tokens := map[string]string{}
	for _, site := range []string{SiteA, SiteB} {
		token := syncidentity.TokenPrefix + strings.Repeat(strings.ToLower(site[len(site)-1:]), 64)
		h := sha256.Sum256([]byte(token))
		tokens[site] = token
		if err := identities.Enroll(ctx, syncidentity.Enrollment{SiteID: site, TokenHash: hex.EncodeToString(h[:])}); err != nil {
			t.Fatal(err)
		}
	}
	route, err := candidateRoutes(ctx, db, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	admitted, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(identities.Bind(func(site string, w http.ResponseWriter, r *http.Request) {
		close(admitted)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		route(site, w, r)
	}))
	defer server.Close()
	// Both halves in one request; neither a writer mirror nor a reader receipt
	// may survive the generation mismatch. Admission occurs BEFORE publication.
	body, _ := json.Marshal(request(SiteB, notebook(SiteB, 1), title(SiteB, 2, "discarded title")))
	r, _ := http.NewRequest("POST", server.URL+"/sync/v1", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+tokens[SiteB])
	r.Header.Set(bounded.Header, "1")
	type response struct {
		status int
		body   string
		err    error
	}
	done := make(chan response, 1)
	go func() {
		res, err := server.Client().Do(r)
		if err != nil {
			done <- response{err: err}
			return
		}
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		done <- response{res.StatusCode, string(b), err}
	}()
	select {
	case <-admitted:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("request was not admitted")
	}
	generation, err := syncgeneration.Current(ctx, db)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	receipt, err := syncgeneration.Publish(ctx, db, syncgeneration.Request{ID: strings.Repeat("1", 64), Expected: generation, SnapshotHash: strings.Repeat("2", 64), Publisher: SiteA}, func(context.Context, *sql.Tx) error { return nil })
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	before := state(t, db)
	close(release)
	var result response
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("exchange did not finish")
	}
	if result.err != nil || result.status != 409 || !strings.Contains(result.body, "library_replaced") {
		t.Fatalf("stale push: %+v", result)
	}
	if !reflect.DeepEqual(before, state(t, db)) {
		t.Fatal("stale push altered relay, ACK, cursor, clock or rows")
	}
	// Fresh requests are stopped at admission, before capabilities/assets/search
	// or any body is delegated. Supplying the new generation header cannot adopt.
	blocked := httptest.NewServer(identities.Bind(func(string, http.ResponseWriter, *http.Request) { t.Error("old binding reached route") }))
	defer blocked.Close()
	for _, path := range []string{"/sync/v1", "/sync/capabilities", "/sync/assets/v1/" + bookID, "/reader/search"} {
		req, _ := http.NewRequest("GET", blocked.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+tokens[SiteB])
		req.Header.Set(syncgeneration.Header, receipt.Generation)
		res, err := blocked.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != 409 || res.Header.Get(syncgeneration.Header) != receipt.Generation {
			t.Fatal("old peer was not told to replace its library")
		}
	}
}
