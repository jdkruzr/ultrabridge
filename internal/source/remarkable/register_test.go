package remarkable

import (
	"context"
	"net/http"
	"testing"

	"github.com/sysop/ultrabridge/internal/source"
)

// TestRegisterRoutesNoHealthConflict guards against re-introducing a route that
// collides with one the main mux already owns. main.go registers its own
// "GET /health" on the shared mux; Go 1.22's ServeMux PANICS on a duplicate
// pattern, which crash-looped the whole process the first time a reMarkable
// source existed (RegisterRoutes is a no-op until then). The reMarkable
// protocol must not register /health — the shared health endpoint serves the
// device's liveness probe.
func TestRegisterRoutesNoHealthConflict(t *testing.T) {
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
	defer src.Stop()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(http.ResponseWriter, *http.Request) {}) // simulate main.go

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RegisterRoutes panicked over a route the main mux owns: %v", r)
		}
	}()
	src.RegisterRoutes(mux)
}
