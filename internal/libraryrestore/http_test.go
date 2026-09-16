package libraryrestore

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/auth"
	"github.com/sysop/ultrabridge/internal/syncidentity"
	"golang.org/x/crypto/bcrypt"
)

func TestNativeRestoreAuthorizationAndBehindGenerationReadBoundary(t *testing.T) {
	s := fixture(t)
	id := archive(t, s, [][]byte{row(t, "notebook", pub, peer, 1, map[string]any{"name": "restored"})}, nil)
	pw, err := bcrypt.GenerateFromPassword([]byte("fixture"), bcrypt.MinCost)
	must(t, err)
	h := s.Handler(auth.New("owner", string(pw)))
	call := func(method, path, body, key string, admin, origin bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		if admin {
			r.SetBasicAuth("owner", "fixture")
		}
		if origin {
			r.Header.Set("Origin", "https://untrusted.invalid")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	r := request(t, s, id)
	body := `{"request_id":"` + r.ID + `","expected_generation":"` + r.Expected + `","snapshot":"` + id + `","publisher":"` + pub + `"}`
	if w := call("POST", "/sync/restore/v1/publish", body, token(peer), false, false); w.Code == 200 {
		t.Fatal("device key published")
	}
	if w := call("POST", "/sync/restore/v1/publish", body, "", true, true); w.Code != 403 {
		t.Fatal("browser published")
	}
	if w := call("POST", "/sync/restore/v1/publish", body, "", true, false); w.Code != 200 {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	b, err := s.Baseline(ctx)
	must(t, err)
	if w := call("GET", "/sync/restore/v1/publications/"+r.ID, "", token(pub), false, false); w.Code != 200 {
		t.Fatal("publisher cannot recover receipt")
	}
	if w := call("GET", "/sync/restore/v1/publications/"+r.ID, "", token(peer), false, false); w.Code != 404 {
		t.Fatal("peer can claim publisher receipt")
	}
	if w := call("GET", "/sync/restore/v1/publications/"+strings.Repeat("0", 64), "", token(pub), false, false); w.Code != 404 {
		t.Fatal("absent receipt not distinguished")
	}
	if w := call("GET", "/sync/restore/v1/state", "", token(peer), false, false); w.Code != 200 || !strings.Contains(w.Body.String(), `"needs_adoption":true`) {
		t.Fatal("old key cannot discover replacement")
	}
	path := "/sync/restore/v1/assets/" + id + "?generation=" + b.Generation
	if w := call("GET", path, "", token(peer), false, false); w.Code != 200 {
		t.Fatal("old key cannot download baseline")
	}
	if w := call("PUT", path, "{}", token(peer), false, false); w.Code != 405 {
		t.Fatal("old key can modify baseline")
	}
	if w := call("GET", "/sync/restore/v1/assets/"+strings.Repeat("a", 64)+"?generation="+b.Generation, "", token(peer), false, false); w.Code != 404 {
		t.Fatal("old key can read arbitrary assets")
	}
	if w := call("GET", path, "", "", false, false); w.Code != 401 {
		t.Fatal("baseline public")
	}
	if w := call("GET", "/sync/restore/v1/assets/"+id+"?generation="+r.Expected, "", token(peer), false, false); w.Code != 409 {
		t.Fatal("wrong baseline generation accepted")
	}
	normal := (syncidentity.Store{DB: s.DB}).Bind(func(string, http.ResponseWriter, *http.Request) { t.Fatal("old key reached row/asset router") })
	q := httptest.NewRequest("POST", "/sync/v1", nil)
	q.Header.Set("Authorization", "Bearer "+token(peer))
	w := httptest.NewRecorder()
	normal.ServeHTTP(w, q)
	if w.Code != 409 {
		t.Fatal("old key admitted")
	}
}

func TestPublicationJoinsOldWorkerBeforeReplacingAndStartsFreshWorker(t *testing.T) {
	s := fixture(t)
	id := archive(t, s, [][]byte{row(t, "notebook", pub, peer, 1, map[string]any{"name": "restored"})}, nil)
	cancelled, release := make(chan struct{}), make(chan struct{})
	var starts atomic.Int32
	w := NewWorkers(ctx, func(c context.Context) func() {
		n := starts.Add(1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			<-c.Done()
			if n == 1 {
				close(cancelled)
				<-release
				_, err := s.DB.Exec(`UPDATE fn_notebook SET name='late old worker'`)
				if err != nil {
					t.Error(err)
				}
			}
		}()
		return func() { <-done }
	})
	defer w.Close()
	s.Exclusive = w.Exclusive
	done := make(chan error, 1)
	r := request(t, s, id)
	go func() { _, err := s.Publish(ctx, r); done <- err }()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("worker not cancelled")
	}
	select {
	case err := <-done:
		close(release)
		t.Fatalf("publication didn't join: %v", err)
	default:
	}
	close(release)
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("publication stuck")
	}
	if starts.Load() != 2 || scalar(t, s, "SELECT name FROM fn_notebook") != "restored" {
		t.Fatal("worker crossed publication")
	}
}
