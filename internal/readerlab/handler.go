// Package readerlab provides only disposable-database fixture admission.
// Protocol implementation is shared with the enrolled library host.
package readerlab

import (
	"context"
	"database/sql"
	"github.com/sysop/ultrabridge/internal/auth"
	"github.com/sysop/ultrabridge/internal/libraryhost"
	"net/http"
)

const SiteA = "0000000000000000000000000A"
const SiteB = "0000000000000000000000000B"

func Handler(ctx context.Context, db *sql.DB) (http.Handler, error) {
	return HandlerWithWake(ctx, db, nil)
}
func HandlerWithWake(ctx context.Context, db *sql.DB, wake func()) (http.Handler, error) {
	return HandlerWithSearch(ctx, db, wake, nil)
}
func HandlerWithSearch(ctx context.Context, db *sql.DB, wake func(), search http.Handler) (http.Handler, error) {
	return HandlerWithAssets(ctx, db, wake, search, false)
}

// Fixed credentials exist only in this fixture adapter, never libraryhost.
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
func HandlerWithEnrollment(ctx context.Context, db *sql.DB, wake func(), search http.Handler, account *auth.Middleware) (http.Handler, error) {
	return libraryhost.EnrolledHandler(ctx, db, libraryhost.RouteOptions{Assets: true, Search: search, WakeReader: wake}, account)
}
func candidateRoutes(ctx context.Context, db *sql.DB, wake func(), search http.Handler, enableAssets bool) (func(string, http.ResponseWriter, *http.Request), error) {
	return libraryhost.BoundRoutes(ctx, db, libraryhost.RouteOptions{Assets: enableAssets, Search: search, WakeReader: wake})
}
