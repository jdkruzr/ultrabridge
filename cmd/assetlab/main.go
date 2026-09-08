// assetlab is a LOCAL headless interoperability harness, not the UB service.
// It only binds loopback; use a disposable DB. EOF on stdin shuts it down.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/jdkruzr/rhizome/server-go/bounded"
	"github.com/sysop/ultrabridge/internal/auth"
	"github.com/sysop/ultrabridge/internal/syncassets"
	"github.com/sysop/ultrabridge/internal/synchttp"
	"github.com/sysop/ultrabridge/internal/syncstore"
	"github.com/sysop/ultrabridge/internal/syncsvc"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

func main() {
	path := flag.String("db", "", "disposable SQLite database path (required)")
	flag.Parse()
	if *path == "" {
		log.Fatal("--db is required; never use your production notedb")
	}
	db, err := sql.Open("sqlite", *path)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA busy_timeout=5000"} {
		if _, err := db.Exec(stmt); err != nil {
			log.Fatal(err)
		}
	}
	ctx := context.Background()
	if err := syncstore.Migrate(ctx, db); err != nil {
		log.Fatal(err)
	}
	if err := syncassets.Migrate(ctx, db); err != nil {
		log.Fatal(err)
	}
	// Deliberately public fixture credentials; never used outside this loopback harness.
	hash, err := bcrypt.GenerateFromPassword([]byte("assetlab"), bcrypt.MinCost)
	if err != nil {
		log.Fatal(err)
	}
	a := auth.New("assetlab", string(hash))
	mux := http.NewServeMux()
	mux.Handle("/sync/assets/v1/", syncassets.Handler(db, a))
	mux.Handle("/sync/v1", a.Wrap(synchttp.New(syncsvc.New(syncstore.New(db), 500, nil, nil), synchttp.DefaultMaxBytes, nil)))
	capabilities, err := bounded.CapabilityHandler(bounded.Defaults(), syncstore.AcceptedSchemaHashes(), true)
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/sync/capabilities", a.Wrap(capabilities))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); _ = server.Close() }()
	fmt.Printf("http://%s\n", listener.Addr())
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
