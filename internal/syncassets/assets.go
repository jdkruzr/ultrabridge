// Package syncassets binds Rhizome's asset channel to UB's existing notedb and
// auth. Stage 2 is headless-only: production Source/router wiring is deferred.
package syncassets

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/jdkruzr/rhizome/server-go/assets"
	"github.com/sysop/ultrabridge/internal/auth"
)

func Migrate(ctx context.Context, db *sql.DB) error { return (&assets.SQLStore{DB: db}).Migrate(ctx) }

// Handler cannot be constructed without UB authentication. It advertises no
// capabilities until the bounded-row and reader-registry gates also exist.
func Handler(db *sql.DB, authentication *auth.Middleware) http.Handler {
	if authentication == nil {
		panic("syncassets: authentication required")
	}
	return authentication.Wrap(assets.NewHandler(&assets.SQLStore{DB: db}))
}
