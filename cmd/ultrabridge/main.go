package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	gocaldav "github.com/emersion/go-webdav/caldav"
	"golang.org/x/crypto/bcrypt"

	"github.com/sysop/ultrabridge/internal/auth"
	ubcaldav "github.com/sysop/ultrabridge/internal/caldav"
	"github.com/sysop/ultrabridge/internal/config"
	"github.com/sysop/ultrabridge/internal/db"
	"github.com/sysop/ultrabridge/internal/logging"
	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/notestore"
	"github.com/sysop/ultrabridge/internal/pipeline"
	"github.com/sysop/ultrabridge/internal/processor"
	"github.com/sysop/ultrabridge/internal/search"
	"github.com/sysop/ultrabridge/internal/sync"
	"github.com/sysop/ultrabridge/internal/taskstore"
	"github.com/sysop/ultrabridge/internal/web"
)

func main() {
	if len(os.Args) >= 3 && os.Args[1] == "hash-password" {
		hash, err := bcrypt.GenerateFromPassword([]byte(os.Args[2]), 10)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ultrabridge: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(hash))
		return
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ultrabridge: %v\n", err)
		os.Exit(1)
	}

	logger := logging.Setup(logging.Config{
		Level:         cfg.LogLevel,
		Format:        cfg.LogFormat,
		File:          cfg.LogFile,
		FileMaxMB:     cfg.LogFileMaxMB,
		FileMaxAge:    cfg.LogFileMaxAge,
		FileMaxBackup: cfg.LogFileMaxBackup,
		SyslogAddr:    cfg.LogSyslogAddr,
	})

	database, err := db.Connect(cfg.DSN())
	if err != nil {
		logger.Error("database connection failed", "error", err)
		os.Exit(1)
	}
	defer database.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID, err := db.ResolveUserID(ctx, database, cfg.UserID)
	if err != nil {
		logger.Error("user resolution failed", "error", err)
		os.Exit(1)
	}
	if cfg.UserID != 0 {
		logger.Info("using configured user_id", "user_id", userID)
	} else {
		logger.Info("discovered user_id", "user_id", userID)
	}

	store := taskstore.New(database, userID)

	notifier := sync.NewNotifier(cfg.SocketIOURL, logger)
	notifier.Connect(context.Background())
	defer notifier.Close()

	// Open the notes SQLite DB (separate from Supernote's MariaDB)
	noteDB, err := notedb.Open(context.Background(), cfg.DBPath)
	if err != nil {
		logger.Error("notedb open failed", "err", err, "path", cfg.DBPath)
		os.Exit(1)
	}
	defer noteDB.Close()

	// Notes pipeline components
	ns := notestore.New(noteDB, cfg.NotesPath)
	si := search.New(noteDB)
	workerCfg := processor.WorkerConfig{
		OCREnabled: cfg.OCREnabled,
		BackupPath: cfg.BackupPath,
		MaxFileMB:  cfg.OCRMaxFileMB,
		Indexer:    si,
	}
	if database != nil {
		workerCfg.CatalogUpdater = processor.NewSPCCatalog(database)
	}
	if cfg.OCREnabled && cfg.OCRAPIURL != "" {
		workerCfg.OCRClient = processor.NewOCRClient(cfg.OCRAPIURL, cfg.OCRAPIKey, cfg.OCRModel, cfg.OCRFormat)
	}
	proc := processor.New(noteDB, workerCfg)
	if cfg.OCREnabled {
		if err := proc.Start(context.Background()); err != nil {
			logger.Warn("processor start failed", "err", err)
		}
		defer proc.Stop()
	}

	pl := pipeline.New(pipeline.Config{
		NotesPath: cfg.NotesPath,
		Store:     ns,
		Proc:      proc,
		Events:    notifier.Events(),
		Logger:    logger,
	})
	pl.Start(context.Background())
	defer pl.Close()

	backend := ubcaldav.NewBackend(store, "/caldav", cfg.CalDAVCollectionName, cfg.DueTimeMode, notifier)
	caldavHandler := &gocaldav.Handler{
		Backend: backend,
		Prefix:  "/caldav",
	}

	authMW := auth.New(cfg.Username, cfg.PasswordHash)

	// Create log broadcaster for web UI
	broadcaster := logging.NewLogBroadcaster()

	// Wire the broadcasting handler to capture logs
	broadcastHandler := logging.NewBroadcastingHandler(logger.Handler(), broadcaster)
	logger = slog.New(broadcastHandler)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.Handle("/caldav/", authMW.Wrap(caldavHandler))
	mux.HandleFunc("/.well-known/caldav", func(w http.ResponseWriter, r *http.Request) {
		authMW.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/caldav/", http.StatusMovedPermanently)
		})).ServeHTTP(w, r)
	})

	// Wire web UI if enabled
	if cfg.WebEnabled {
		webHandler := web.NewHandler(store, notifier, ns, si, proc, pl, logger, broadcaster)
		mux.Handle("/", authMW.Wrap(webHandler))
	}

	handler := logging.RequestID(logger)(mux)
	logger.Info("ultrabridge starting", "addr", cfg.ListenAddr)
	if err := http.ListenAndServe(cfg.ListenAddr, handler); err != nil {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
}
