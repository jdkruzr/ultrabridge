// Package libraryhost assembles the shared-library protocol without fixture
// credentials or listener policy. Hosts explicitly opt in; legacy routes do not.
package libraryhost

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

type RouteOptions struct {
	Assets     bool
	Search     http.Handler
	WakeReader func()
	// Called after commit. Must be nonblocking; the host owns the worker lifetime.
	WriterChanged func(context.Context, []syncstore.TablePK)
}
type exchange struct {
	store   *syncstore.Store
	site    string
	options RouteOptions
}

func (s exchange) Sync(context.Context, syncsvc.Request) (syncsvc.Response, error) {
	return syncsvc.Response{}, syncsvc.ErrSchemaMismatch // No unbounded fallback.
}
func (s exchange) SyncBounded(ctx context.Context, req bounded.Request, limits bounded.Limits) ([]byte, error) {
	name := []rune(strings.TrimSpace(req.DeviceName))
	if len(name) > syncsvc.MaxDeviceNameLen {
		name = name[:syncsvc.MaxDeviceNameLen]
	}
	req.DeviceName = string(name)
	body, changed, err := s.store.ExchangeReaderCandidate(ctx, req, limits, s.site)
	if err == nil {
		if s.options.WakeReader != nil {
			s.options.WakeReader()
		}
		if len(changed) > 0 && s.options.WriterChanged != nil {
			s.options.WriterChanged(ctx, changed)
		}
	}
	return body, err
}

// BoundRoutes must ONLY be called after identity verification. Production hosts
// use EnrolledHandler; readerlab wraps this seam in explicit fixture auth.
// Keep the admitted request context for transaction-time generation checks.
func BoundRoutes(ctx context.Context, db *sql.DB, options RouteOptions) (func(string, http.ResponseWriter, *http.Request), error) {
	if err := readerstore.Install(ctx, db); err != nil {
		return nil, err
	}
	caps, err := bounded.CapabilityHandler(bounded.Defaults(), []string{readercontract.CandidateCombined().SchemaHash()}, options.Assets)
	if err != nil {
		return nil, err
	}
	store := syncstore.New(db)
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
			synchttp.New(exchange{store, site, options}, synchttp.DefaultMaxBytes, nil).ServeHTTP(w, r)
		case "/reader/search":
			if options.Search == nil {
				http.NotFound(w, r)
			} else {
				options.Search.ServeHTTP(w, r)
			}
		default:
			if options.Assets && strings.HasPrefix(r.URL.Path, "/sync/assets/v1/") {
				assetHandler.ServeHTTP(w, r)
			} else {
				http.NotFound(w, r)
			}
		}
	}, nil
}

// EnrolledHandler has no shared-account row/asset bypass. Account credentials
// authorize enrollment only; device keys bind every ordinary protocol request.
func EnrolledHandler(ctx context.Context, db *sql.DB, options RouteOptions, account *auth.Middleware) (http.Handler, error) {
	if account == nil {
		return nil, fmt.Errorf("shared library account authentication required")
	}
	if err := syncidentity.Install(ctx, db); err != nil {
		return nil, err
	}
	route, err := BoundRoutes(ctx, db, options)
	if err != nil {
		return nil, err
	}
	identities := syncidentity.Store{DB: db}
	devices := identities.Bind(route)
	management := account.Wrap(identities.AdminHandler())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/sync/devices/v1/") {
			management.ServeHTTP(w, r)
		} else {
			devices.ServeHTTP(w, r)
		}
	}), nil
}
