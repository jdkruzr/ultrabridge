// Package readerlab is ONLY the loopback, disposable-db integration harness.
// Public fixture credentials are not device enrollment or production auth.
package readerlab

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/jdkruzr/rhizome/server-go/assets"
	"github.com/jdkruzr/rhizome/server-go/bounded"
	"github.com/sysop/ultrabridge/internal/auth"
	"github.com/sysop/ultrabridge/internal/readercontract"
	"github.com/sysop/ultrabridge/internal/readerstore"
	"github.com/sysop/ultrabridge/internal/syncgeneration"
	"github.com/sysop/ultrabridge/internal/synchttp"
	"github.com/sysop/ultrabridge/internal/syncidentity"
	"github.com/sysop/ultrabridge/internal/syncstore"
	"github.com/sysop/ultrabridge/internal/syncsvc"
)

const SiteA = "0000000000000000000000000A"
const SiteB = "0000000000000000000000000B"

type service struct {
	store *syncstore.Store
	site  string
	wake  func()
}

func (s service) Sync(context.Context, syncsvc.Request) (syncsvc.Response, error) {
	return syncsvc.Response{}, syncsvc.ErrSchemaMismatch // No unbounded/legacy fallback.
}
func (s service) SyncBounded(ctx context.Context, req bounded.Request, limits bounded.Limits) ([]byte, error) {
	name := []rune(strings.TrimSpace(req.DeviceName))
	if len(name) > syncsvc.MaxDeviceNameLen {
		name = name[:syncsvc.MaxDeviceNameLen]
	}
	req.DeviceName = string(name)
	body, _, err := s.store.ExchangeReaderCandidate(ctx, req, limits, s.site)
	if err == nil && s.wake != nil {
		s.wake()
	}
	return body, err
}

// Handler explicitly installs reader storage in an already migrated fixture DB.
// No worker runs inside the HTTP transaction. Tests drain after receipt/restart.
func Handler(ctx context.Context, db *sql.DB) (http.Handler, error) {
	return HandlerWithWake(ctx, db, nil)
}

// HandlerWithWake connects only a post-commit hint. The host owns worker
// lifecycle; restart recovery never depends on the hint being delivered.
func HandlerWithWake(ctx context.Context, db *sql.DB, wake func()) (http.Handler, error) {
	return HandlerWithSearch(ctx, db, wake, nil)
}

// HandlerWithSearch adds only the authenticated fixture reader-search surface.
func HandlerWithSearch(ctx context.Context, db *sql.DB, wake func(), search http.Handler) (http.Handler, error) {
	return HandlerWithAssets(ctx, db, wake, search, false)
}

// HandlerWithAssets is explicit, fixture-only combined row/asset admission.
// The outer site-authenticated router protects EVERY asset request.
func HandlerWithAssets(ctx context.Context, db *sql.DB, wake func(), search http.Handler, enableAssets bool) (http.Handler, error) {
	route, err := candidateRoutes(ctx, db, wake, search, enableAssets)
	if err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		site, known := map[string]string{"reader-a": SiteA, "reader-b": SiteB}[user]
		if !ok || !known || password != "readerlab" {
			w.Header().Set("WWW-Authenticate", `Basic realm="Disposable Reader Lab"`)
			http.Error(w, "fixture credentials required", 401)
			return
		}
		route(site, w, r)
	}), nil
}

// HandlerWithEnrollment exercises persistent credentials instead of the fixed
// A/B mapping. ADMIN credentials remain loopback fixtures, not production setup.
func HandlerWithEnrollment(ctx context.Context, db *sql.DB, wake func(), search http.Handler, admin *auth.Middleware) (http.Handler, error) {
	if err := syncidentity.Install(ctx, db); err != nil {
		return nil, err
	}
	route, err := candidateRoutes(ctx, db, wake, search, true)
	if err != nil {
		return nil, err
	}
	identities := syncidentity.Store{DB: db}
	devices := identities.Bind(route)
	management := admin.Wrap(identities.AdminHandler())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/sync/devices/v1/") {
			management.ServeHTTP(w, r)
			return
		}
		devices.ServeHTTP(w, r)
	}), nil
}

func candidateRoutes(ctx context.Context, db *sql.DB, wake func(), search http.Handler, enableAssets bool) (func(string, http.ResponseWriter, *http.Request), error) {
	if err := readerstore.Install(ctx, db); err != nil {
		return nil, err
	}
	caps, err := bounded.CapabilityHandler(bounded.Defaults(), []string{readercontract.CandidateCombined().SchemaHash()}, enableAssets)
	if err != nil {
		return nil, err
	}
	assetHandler := assets.NewHandler(&assets.SQLStore{DB: db, BeforeWrite: func(ctx context.Context, tx *sql.Tx) error {
		err := syncgeneration.CheckRequestTx(ctx, tx)
		if errors.Is(err, syncgeneration.ErrReplaced) {
			return assets.Fail(409, "library_replaced")
		}
		return err
	}})
	return func(site string, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync/capabilities":
			caps.ServeHTTP(w, r)
		case "/sync/v1":
			synchttp.New(service{syncstore.New(db), site, wake}, synchttp.DefaultMaxBytes, nil).ServeHTTP(w, r)
		case "/reader/search":
			if search == nil {
				http.NotFound(w, r)
			} else {
				search.ServeHTTP(w, r)
			}
		default:
			if enableAssets && strings.HasPrefix(r.URL.Path, "/sync/assets/v1/") {
				assetHandler.ServeHTTP(w, r)
				return
			}
			http.NotFound(w, r)
		}
	}, nil
}
