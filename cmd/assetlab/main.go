// assetlab is a LOCAL headless interoperability harness, not the UB service.
// It only binds loopback; use a disposable DB. EOF on stdin shuts it down.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jdkruzr/rhizome/server-go/bounded"
	"github.com/sysop/ultrabridge/internal/auth"
	"github.com/sysop/ultrabridge/internal/readerlab"
	"github.com/sysop/ultrabridge/internal/readersearch"
	"github.com/sysop/ultrabridge/internal/readerstore"
	"github.com/sysop/ultrabridge/internal/syncassets"
	"github.com/sysop/ultrabridge/internal/synchttp"
	"github.com/sysop/ultrabridge/internal/syncstore"
	"github.com/sysop/ultrabridge/internal/syncsvc"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

func main() {
	path := flag.String("db", "", "disposable SQLite database path (required)")
	reader := flag.Bool("reader", false, "enable candidate reader row fixture with per-site fixture credentials")
	readerAssets := flag.Bool("reader-assets", false, "with --reader: enable combined asset fixture")
	enrollment := flag.Bool("reader-enrollment", false, "with --reader-assets: require enrolled device credentials; fixture admin assetlab:assetlab")
	checkpoint := flag.String("checkpoint", "", "with --reader-assets: gate a successful METHOD:path response before delivery")
	inspect := flag.Bool("reader-inspect", false, "with --reader-assets: inspect disposable DB and annotation n, then exit")
	backup := flag.String("reader-backup", "", "with --reader-assets: consistent snapshot into a NEW disposable file, then exit")
	metadataOnly := flag.Bool("metadata-only", false, "with --reader-backup: omit assets from the newly created snapshot ONLY")
	inventory := flag.Bool("reader-inventory", false, "with --reader-assets: hash disposable table state and verify book assets, then exit")
	projection := flag.String("reader-project", "", "with --reader: drain disposable DB, print annotation projection, exit without HTTP")
	restore := flag.String("reader-restore", "", "offline: restore snapshot into a NEW operation-owned directory")
	restoreID := flag.String("restore-id", "", "stable restore attempt identifier")
	flag.Parse()
	if (*readerAssets && !*reader) || ((*enrollment || *checkpoint != "" || *inspect || *backup != "" || *inventory) && !*readerAssets) || (*metadataOnly && *backup == "") {
		log.Fatal("reader-assets requires reader; checkpoint requires reader-assets")
	}
	if *path == "" {
		log.Fatal("--db is required; never use your production notedb")
	}
	if *restore != "" {
		if !*readerAssets || !*enrollment {
			log.Fatal("restore requires enrolled reader-assets fixture")
		}
		result, err := prepareRestore(context.Background(), *path, *restore, *restoreID, restoreGate(*checkpoint))
		if err != nil {
			log.Fatal(err)
		}
		if err = json.NewEncoder(os.Stdout).Encode(result); err != nil {
			log.Fatal(err)
		}
		return
	}
	if !*inventory && *backup == "" && !*inspect {
		if err := validateRestoreStartup(*path, *enrollment); err != nil {
			log.Fatal(err)
		}
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
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := syncstore.Migrate(ctx, db); err != nil {
		log.Fatal(err)
	}
	if err := syncassets.Migrate(ctx, db); err != nil {
		log.Fatal(err)
	}
	if *backup != "" || *inventory {
		var result map[string]any
		if *backup != "" {
			result, err = backupFixture(ctx, db, *backup, *metadataOnly)
		} else {
			result, err = inventoryFixture(ctx, db)
		}
		if err != nil {
			log.Fatal(err)
		}
		if err = json.NewEncoder(os.Stdout).Encode(result); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *inspect {
		result, err := inspectFixture(ctx, db)
		if err != nil {
			log.Fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *projection != "" {
		if !*reader {
			log.Fatal("--reader-project requires --reader")
		}
		result, err := readerlab.ProjectFixture(ctx, db, *projection)
		if err != nil {
			log.Fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			log.Fatal(err)
		}
		return
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
	var handler http.Handler = mux
	workerDone := make(chan struct{})
	if *reader {
		options := readerstore.DefaultWorkerOptions()
		options.OnError = func(err error) { log.Printf("reader worker: %v", err) }
		if err := readerstore.Install(ctx, db); err != nil {
			log.Fatal(err)
		}
		if err := readersearch.Install(ctx, db); err != nil {
			log.Fatal(err)
		}
		search := readersearch.New(db)
		worker, e := readerstore.NewWorker(readerstore.New(db), search.Schedule, options)
		if e != nil {
			log.Fatal(e)
		}
		if *enrollment {
			handler, err = readerlab.HandlerWithEnrollment(ctx, db, worker.Wake, search.Handler(), a)
		} else {
			handler, err = readerlab.HandlerWithAssets(ctx, db, worker.Wake, search.Handler(), *readerAssets)
		}
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			defer close(workerDone)
			searchDone := make(chan struct{})
			go func() { defer close(searchDone); _ = search.Run(ctx, options.OnError) }()
			_ = worker.Run(ctx)
			<-searchDone
		}()
	} else {
		close(workerDone)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	if *checkpoint != "" {
		handler = gateResponse(handler, *checkpoint)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); _ = server.Close() }()
	go func() { <-ctx.Done(); _ = server.Close() }()
	fmt.Printf("http://%s\n", listener.Addr())
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	cancel()
	<-workerDone // Join before closing the shared database.
}
