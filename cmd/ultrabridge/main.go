package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	gocaldav "github.com/emersion/go-webdav/caldav"
	"golang.org/x/crypto/bcrypt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sysop/ultrabridge/internal/appconfig"
	"github.com/sysop/ultrabridge/internal/auth"
	"github.com/sysop/ultrabridge/internal/booxpipeline"
	ubcaldav "github.com/sysop/ultrabridge/internal/caldav"
	"github.com/sysop/ultrabridge/internal/chat"
	"github.com/sysop/ultrabridge/internal/db"
	"github.com/sysop/ultrabridge/internal/logging"
	"github.com/sysop/ultrabridge/internal/mcpauth"
	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/notestore"
	"github.com/sysop/ultrabridge/internal/processor"
	"github.com/sysop/ultrabridge/internal/rag"
	"github.com/sysop/ultrabridge/internal/search"
	"github.com/sysop/ultrabridge/internal/service"
	"github.com/sysop/ultrabridge/internal/source"
	"github.com/sysop/ultrabridge/internal/source/boox"
	"github.com/sysop/ultrabridge/internal/source/supernote"
	"github.com/sysop/ultrabridge/internal/spcserver"
	spcauth "github.com/sysop/ultrabridge/internal/spcserver/auth"
	"github.com/sysop/ultrabridge/internal/spcserver/fileids"
	"github.com/sysop/ultrabridge/internal/spcserver/notify"
	"github.com/sysop/ultrabridge/internal/sync"
	"github.com/sysop/ultrabridge/internal/taskdb"
	"github.com/sysop/ultrabridge/internal/tasksync"
	snsync "github.com/sysop/ultrabridge/internal/tasksync/supernote"
	"github.com/sysop/ultrabridge/internal/web"
	ubwebdav "github.com/sysop/ultrabridge/internal/webdav"
)

// syncProviderAdapter wraps tasksync.SyncEngine to satisfy web.SyncStatusProvider.
type syncProviderAdapter struct{ engine *tasksync.SyncEngine }

func (a *syncProviderAdapter) Status() service.SyncStatus {
	if a.engine == nil {
		return service.SyncStatus{}
	}
	s := a.engine.Status()
	last := time.UnixMilli(s.LastSyncAt).UTC()
	next := time.UnixMilli(s.NextSyncAt).UTC()
	return service.SyncStatus{
		LastSyncAt:    &last,
		NextSyncAt:    &next,
		InProgress:    s.InProgress,
		LastError:     &s.LastError,
		AdapterID:     s.AdapterID,
		AdapterActive: s.AdapterActive,
	}
}

func (a *syncProviderAdapter) TriggerSync() {
	if a.engine != nil {
		a.engine.TriggerSync()
	}
}

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

	if len(os.Args) >= 4 && os.Args[1] == "seed-user" {
		username, password := os.Args[2], os.Args[3]
		dbPath := envOrDefault("UB_DB_PATH", "/data/ultrabridge.db")
		db, err := notedb.Open(context.Background(), dbPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to open database: %v\n", err)
			os.Exit(1)
		}
		defer db.Close()
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to hash password: %v\n", err)
			os.Exit(1)
		}
		ctx := context.Background()
		if err := notedb.SetSetting(ctx, db, appconfig.KeyUsername, username); err != nil {
			fmt.Fprintf(os.Stderr, "failed to save username: %v\n", err)
			os.Exit(1)
		}
		if err := notedb.SetSetting(ctx, db, appconfig.KeyPasswordHash, string(hash)); err != nil {
			fmt.Fprintf(os.Stderr, "failed to save password hash: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("User credentials saved.")
		return
	}

	// Stage 1: Bootstrap config (needed before DB opens)
	// Logging and database paths read directly from env vars
	bootstrapCfg := &bootstrapConfig{
		logLevel:         envOrDefault("UB_LOG_LEVEL", "info"),
		logFormat:        envOrDefault("UB_LOG_FORMAT", "json"),
		logFile:          os.Getenv("UB_LOG_FILE"),
		logFileMaxMB:     envIntOrDefault("UB_LOG_FILE_MAX_MB", 50),
		logFileMaxAge:    envIntOrDefault("UB_LOG_FILE_MAX_AGE_DAYS", 30),
		logFileMaxBackup: envIntOrDefault("UB_LOG_FILE_MAX_BACKUPS", 5),
		logSyslogAddr:    os.Getenv("UB_LOG_SYSLOG_ADDR"),
		dbPath:           envOrDefault("UB_DB_PATH", "/data/ultrabridge.db"),
		taskDBPath:       envOrDefault("UB_TASK_DB_PATH", "/data/ultrabridge-tasks.db"),
		listenAddr:       envOrDefault("UB_LISTEN_ADDR", ":8443"),
		dbEnvPath:        envOrDefault("UB_SUPERNOTE_DBENV_PATH", "/run/secrets/dbenv"),
		passwordHashPath: envOrDefault("UB_PASSWORD_HASH_PATH", "/run/secrets/ub_password_hash"),
	}

	logger := logging.Setup(logging.Config{
		Level:         bootstrapCfg.logLevel,
		Format:        bootstrapCfg.logFormat,
		File:          bootstrapCfg.logFile,
		FileMaxMB:     bootstrapCfg.logFileMaxMB,
		FileMaxAge:    bootstrapCfg.logFileMaxAge,
		FileMaxBackup: bootstrapCfg.logFileMaxBackup,
		SyslogAddr:    bootstrapCfg.logSyslogAddr,
	})

	// Load MariaDB credentials from env or .dbenv file
	dbName, dbUser, dbPassword, err := loadDBEnv(bootstrapCfg.dbEnvPath)
	if err != nil {
		logger.Warn("failed to load MariaDB credentials", "error", err)
		// Non-fatal — catalog sync will be skipped
	}

	// Load password hash from env or secrets file
	passwordHash := os.Getenv("UB_PASSWORD_HASH")
	if passwordHash == "" {
		if data, err := os.ReadFile(bootstrapCfg.passwordHashPath); err == nil {
			passwordHash = strings.TrimSpace(string(data))
		}
	}

	// Connect to Supernote MariaDB (optional — required only if sync is enabled)
	// Build DSN from credentials we loaded
	var database *sql.DB
	if dbUser != "" && dbPassword != "" && dbName != "" {
		dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true",
			dbUser, dbPassword,
			envOrDefault("UB_DB_HOST", "mariadb"),
			envOrDefault("UB_DB_PORT", "3306"),
			dbName)
		database, err = db.Connect(dsn)
		if err != nil {
			// Check if sync is enabled
			if envBoolOrDefault("UB_SN_SYNC_ENABLED", false) {
				logger.Error("database connection failed (required for sync)", "error", err)
				os.Exit(1)
			}
			logger.Warn("database connection failed, notes catalog sync disabled", "error", err)
			// database remains nil — catalog updater won't be set, which is nil-guarded below
		}
	} else {
		logger.Warn("MariaDB credentials incomplete, skipping connection")
	}

	if database != nil {
		defer database.Close()
	}

	var userID int64
	if database != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		userIDVal := int64(envIntOrDefault("UB_USER_ID", 0))
		userID, err = db.ResolveUserID(ctx, database, userIDVal)
		if err != nil {
			if envBoolOrDefault("UB_SN_SYNC_ENABLED", false) {
				logger.Error("user resolution failed (required for sync)", "error", err)
				os.Exit(1)
			}
			logger.Warn("user resolution failed", "error", err)
		} else if userIDVal != 0 {
			logger.Info("using configured user_id", "user_id", userID)
		} else {
			logger.Info("discovered user_id", "user_id", userID)
		}
	}

	// Open the task SQLite DB
	taskDB, err := taskdb.Open(context.Background(), bootstrapCfg.taskDBPath)
	if err != nil {
		logger.Error("taskdb open failed", "err", err, "path", bootstrapCfg.taskDBPath)
		os.Exit(1)
	}
	defer taskDB.Close()

	store := taskdb.NewStore(taskDB)

	// Run migration if task DB is empty and SPC sync is enabled
	if envBoolOrDefault("UB_SN_SYNC_ENABLED", false) {
		isEmpty, err := store.IsEmpty(context.Background())
		if err != nil {
			logger.Error("taskdb empty check failed", "err", err)
			os.Exit(1)
		}
		if isEmpty {
			logger.Info("empty task DB detected, attempting migration from SPC")
			snAPIURL := envOrDefault("UB_SN_API_URL", "http://supernote-service:8080")
			snAccount := os.Getenv("UB_SN_ACCOUNT")
			snPassword := os.Getenv("UB_SN_PASSWORD")
			migClient := snsync.NewClient(snAPIURL, snAccount, snPassword, logger)
			if err := migClient.Login(context.Background()); err != nil {
				logger.Warn("SPC login failed for migration, starting with empty store", "error", err)
			} else {
				sm := tasksync.NewSyncMap(taskDB)
				count, err := snsync.MigrateFromSPC(context.Background(), migClient, store, sm, logger)
				if err != nil {
					logger.Warn("migration from SPC failed", "error", err)
				} else {
					logger.Info("migrated tasks from SPC", "count", count)
				}
			}
		} else {
			logger.Info("task DB populated, skipping migration")
		}
	}

	socketIOURL := envOrDefault("UB_SOCKETIO_URL", "ws://supernote-service:8080/socket.io/")
	notifier := sync.NewNotifier(socketIOURL, logger)
	notifier.Connect(context.Background())
	defer notifier.Close()

	// Open the notes SQLite DB (separate from Supernote's MariaDB)
	noteDB, err := notedb.Open(context.Background(), bootstrapCfg.dbPath)
	if err != nil {
		logger.Error("notedb open failed", "err", err, "path", bootstrapCfg.dbPath)
		os.Exit(1)
	}
	defer noteDB.Close()

	// Stage 2: Load application config from DB (after notedb opens)
	cfg, err := appconfig.Load(context.Background(), noteDB)
	if err != nil {
		logger.Error("appconfig load failed", "error", err)
		os.Exit(1)
	}

	// Override auth credentials from bootstrap (env vars take precedence)
	if passwordHash != "" {
		cfg.PasswordHash = passwordHash
	}
	if username := os.Getenv("UB_USERNAME"); username != "" {
		cfg.Username = username
	}

	// Run mcpauth migration to ensure mcp_tokens table exists
	if err := mcpauth.Migrate(context.Background(), noteDB); err != nil {
		logger.Error("mcpauth migrate", "error", err)
		os.Exit(1)
	}

	// Shared infrastructure (not per-source)
	si := search.New(noteDB)

	// Initialize embedding pipeline if enabled
	var embedder rag.Embedder
	var embedStore *rag.Store
	var backfillCancel context.CancelFunc
	if cfg.EmbedEnabled {
		embedder = rag.NewOllamaEmbedder(cfg.OllamaURL, cfg.OllamaEmbedModel, logger)
		embedStore = rag.NewStore(noteDB, logger)

		// Load existing embeddings into memory (AC1.6)
		n, err := embedStore.LoadAll(context.Background())
		if err != nil {
			logger.Warn("failed to load embeddings into cache", "err", err)
		} else {
			logger.Info("loaded embeddings into memory", "count", n)
		}

		// Startup backfill (AC1.4) — runs in background with cancellable context
		var backfillCtx context.Context
		backfillCtx, backfillCancel = context.WithCancel(context.Background())
		go func() {
			n, err := rag.Backfill(backfillCtx, embedStore, embedder, cfg.OllamaEmbedModel, logger)
			if err != nil {
				logger.Warn("startup backfill failed", "err", err)
			} else if n > 0 {
				logger.Info("startup backfill complete", "embedded", n)
			}
		}()
	}

	// Create retriever if embedding is available (also works FTS-only when embedStore is nil)
	var retriever *rag.Retriever
	if embedStore != nil {
		retriever = rag.NewRetriever(noteDB, si, embedStore, embedder, logger)
	} else {
		// FTS-only mode: retriever works without embeddings
		retriever = rag.NewRetriever(noteDB, si, nil, nil, logger)
	}

	// Set up source registry with factory closures
	registry := source.NewRegistry()
	registry.Register("supernote", func(db *sql.DB, row source.SourceRow, deps source.SharedDeps) (source.Source, error) {
		return supernote.NewSource(db, row, deps, database, notifier.Events())
	})
	registry.Register("boox", func(db *sql.DB, row source.SourceRow, deps source.SharedDeps) (source.Source, error) {
		return boox.NewSource(db, row, deps, boox.BooxDeps{
			ContentDeleter: si,
			OnTodosFound: func(ctx context.Context, notePath string, todos []booxpipeline.TodoItem) {
				// Read the external base URL each time so changes from Settings
				// take effect immediately without a restart.
				externalBaseURL, _ := notedb.GetSetting(ctx, noteDB, appconfig.KeyBooxExternalBaseURL)
				created := booxpipeline.CreateTasksFromTodos(ctx, store, notePath, todos, externalBaseURL, logger)
				if created > 0 && notifier != nil {
					notifier.Notify(ctx)
				}
			},
		})
	})

	// Create shared dependencies for sources
	var ocrClient *processor.OCRClient
	if cfg.OCREnabled && cfg.OCRAPIURL != "" {
		ocrClient = processor.NewOCRClient(cfg.OCRAPIURL, cfg.OCRAPIKey, cfg.OCRModel, cfg.OCRFormat)
	}

	deps := source.SharedDeps{
		Indexer:      si,
		Embedder:     embedder,
		EmbedModel:   cfg.OllamaEmbedModel,
		EmbedStore:   embedStore,
		OCRClient:    ocrClient,
		OCRMaxFileMB: cfg.OCRMaxFileMB,
		Logger:       logger,
	}

	// List enabled sources from DB
	rows, err := source.ListEnabledSources(context.Background(), noteDB)
	if err != nil {
		logger.Error("list sources failed", "err", err)
		os.Exit(1)
	}

	// Start sources
	var sources []source.Source
	for _, row := range rows {
		s, err := registry.Create(noteDB, row, deps)
		if err != nil {
			logger.Warn("skipping source", "type", row.Type, "name", row.Name, "err", err)
			continue // AC2.7 + AC2.8: unknown type or bad config → skip, don't crash
		}
		if err := s.Start(context.Background()); err != nil {
			logger.Warn("source start failed", "type", row.Type, "name", row.Name, "err", err)
			continue
		}
		defer s.Stop()
		sources = append(sources, s)
		logger.Info("source started", "type", s.Type(), "name", s.Name())
	}

	var booxNotesPath string
	var snNotesPath string

	// Build a map from source type to source row for extracting config
	sourceRowMap := make(map[string]source.SourceRow)
	for _, row := range rows {
		sourceRowMap[row.Type] = row
	}

	// Extract configs from source rows
	if snRow, hasSupernote := sourceRowMap["supernote"]; hasSupernote {
		var snCfg supernote.Config
		if err := json.Unmarshal([]byte(snRow.ConfigJSON), &snCfg); err == nil {
			snNotesPath = snCfg.NotesPath
		}
	}
	if booxRow, hasBoox := sourceRowMap["boox"]; hasBoox {
		var booxCfg boox.Config
		if err := json.Unmarshal([]byte(booxRow.ConfigJSON), &booxCfg); err == nil {
			booxNotesPath = booxCfg.NotesPath
		}
	}

	// Sync import path from env var to settings DB so the web handler can read it.
	booxImportPath := os.Getenv("UB_BOOX_IMPORT_PATH")
	if booxImportPath != "" {
		notedb.SetSetting(context.Background(), noteDB, "boox_import_path", booxImportPath)
	}

	// Start sync engine if enabled
	var syncEngine *tasksync.SyncEngine
	if cfg.SNSyncEnabled {
		syncEngine = tasksync.NewSyncEngine(
			store, taskDB, logger,
			time.Duration(cfg.SNSyncInterval)*time.Second,
		)
		snAdapter := snsync.NewAdapter(cfg.SNAPIURL, cfg.SNAccount, cfg.SNPassword, notifier, logger)
		syncEngine.RegisterAdapter(snAdapter)
		if err := syncEngine.Start(context.Background()); err != nil {
			logger.Warn("sync engine start failed", "error", err)
		} else {
			defer syncEngine.Stop()
		}
	}

	// In server mode, construct the SPC server now so its Engine.IO registry can
	// back the STARTSYNC notifier the CalDAV backend and task service use; the
	// listener itself is launched later. In client mode the existing
	// sync.Notifier is used and wiring is unchanged (regression-safe).
	var spcSrv *spcserver.Server
	var taskNotifier interface {
		Notify(context.Context) error
	} = notifier
	if cfg.SPCMode == "server" {
		// Phase 2 file listing: migrate the path↔id table (gated to server mode,
		// like mcpauth.Migrate). Best-effort — a failure disables file listing
		// but must not stop the server (task sync still works).
		if err := fileids.Migrate(context.Background(), noteDB); err != nil {
			logger.Error("spc fileids migration failed; file listing disabled", "err", err)
		}
		// Phase 3 download: ensure a persistent OSS signing secret exists
		// (auto-generated on first boot). Best-effort — on failure we fall back
		// to the (empty) configured value, which still works since UB both signs
		// and verifies; persistence just keeps issued URLs valid across restart.
		ossSecret := cfg.SPCOssSecret
		if s, err := appconfig.EnsureSPCOssSecret(context.Background(), noteDB); err != nil {
			logger.Error("spc oss secret generation failed", "err", err)
		} else {
			ossSecret = s
		}
		spcSrv = spcserver.New(spcserver.Config{
			Mode:           cfg.SPCMode,
			ListenAddr:     cfg.SPCListenAddr,
			TLSCert:        cfg.SPCTLSCert,
			TLSKey:         cfg.SPCTLSKey,
			DB:             noteDB,
			JWTSecret:      cfg.SPCJWTSecret,
			DeviceAccount:  cfg.SPCDeviceAccount,
			DevicePassword: cfg.SPCDevicePassword,
			TaskStore:      store,
			CollectionName: cfg.CalDAVCollectionName,
			FileRoot:       cfg.SPCFileRoot,
			QuotaBytes:     cfg.SPCQuotaBytes,
			OssSecret:      ossSecret,
			Logger:         logger,
		})
		taskNotifier = notify.NewSocketNotifier(
			spcSrv.SocketRegistry(),
			func(ctx context.Context) (string, error) {
				return notedb.GetSetting(ctx, noteDB, spcauth.UserIDSettingKey)
			},
			logger,
		)
	}

	backend := ubcaldav.NewBackend(store, "/caldav", cfg.CalDAVCollectionName, cfg.DueTimeMode, taskNotifier)
	caldavHandler := &gocaldav.Handler{
		Backend: backend,
		Prefix:  "/caldav",
	}

	// Generate a persistent internal loopback token for self-calls (MCP -> JSON API).
	// Not stored in DB, strictly in-memory per process lifecycle.
	internalTokenBytes := make([]byte, 32)
	rand.Read(internalTokenBytes)
	internalToken := hex.EncodeToString(internalTokenBytes)

	authMW := auth.NewDynamic(func() (string, string) {
		// Read credentials from DB on each request so changes from
		// seed-user, setup page, or Settings UI take effect immediately.
		// Falls back to bootstrap env var values if DB has no credentials.
		u, _ := notedb.GetSetting(context.Background(), noteDB, appconfig.KeyUsername)
		h, _ := notedb.GetSetting(context.Background(), noteDB, appconfig.KeyPasswordHash)
		if u == "" {
			u = cfg.Username
		}
		if h == "" {
			h = cfg.PasswordHash
		}
		return u, h
	})
	// Enable bearer token auth (MCP tokens from Settings UI + internal loopback)
	authMW.SetTokenValidator(func(token string) (string, error) {
		if token == internalToken {
			return "internal", nil
		}
		return mcpauth.ValidateToken(context.Background(), noteDB, token)
	})

	// Create log broadcaster for web UI
	broadcaster := logging.NewLogBroadcaster()

	// Wire the broadcasting handler to capture logs
	broadcastHandler := logging.NewBroadcastingHandler(logger.Handler(), broadcaster)
	logger = slog.New(broadcastHandler)

	// Set logger for auth middleware to enable verbose failure logging
	authMW.SetLogger(logger, cfg.LogVerboseAPI)

	mux := http.NewServeMux()
	var webHandler *web.Handler // will be set later if web is enabled
	var configSvc service.ConfigService
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		configDirty := false
		if configSvc != nil {
			configDirty = configSvc.IsRestartRequired()
		}
		type healthResp struct {
			Status      string `json:"status"`
			ConfigDirty bool   `json:"config_dirty"`
		}
		json.NewEncoder(w).Encode(healthResp{
			Status:      "ok",
			ConfigDirty: configDirty,
		})
	})
	// Wrap the CalDAV handler with a PROPPATCH stub so clients can rename
	// the collection (DAV:displayname) without hitting the 501 from the
	// go-webdav library. The callback persists the new name to the settings
	// DB and updates the running backend so subsequent PROPFIND responses
	// reflect the change without a container restart.
	caldavWithProppatch := ubcaldav.ProppatchStub(ubcaldav.GetOnCollectionStub(caldavHandler), ubcaldav.ProppatchOptions{
		OnDisplayName: func(name string) error {
			trimmed := strings.TrimSpace(name)
			if trimmed == "" {
				return nil
			}
			backend.SetCollectionName(trimmed)
			if noteDB != nil {
				return notedb.SetSetting(context.Background(), noteDB, appconfig.KeyCalDAVCollectionName, trimmed)
			}
			return nil
		},
		Logger: func(format string, args ...any) {
			logger.Warn(fmt.Sprintf(format, args...))
		},
	})
	mux.Handle("/caldav/", authMW.Wrap(caldavWithProppatch))
	// Register both trailing-slash variants because Go's net/http ServeMux
	// treats "/.well-known/caldav" (exact) and "/.well-known/caldav/" (prefix)
	// as distinct patterns. RFC 6764 uses the no-slash form but some clients
	// probe with a trailing slash; both should redirect to /caldav/.
	wellKnownCalDAV := authMW.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/caldav/", http.StatusMovedPermanently)
	}))
	mux.Handle("/.well-known/caldav", wellKnownCalDAV)
	mux.Handle("/.well-known/caldav/", wellKnownCalDAV)

	// MCP discovery for Claude/OAuth clients
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"mcp_endpoint": "/mcp",
		})
	})

	// General OAuth discovery probes
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"providers": []string{"/mcp"},
		})
	})

	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		// Detect host from request
		host := r.Host
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		baseURL := scheme + "://" + host

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"issuer":                                baseURL,
			"authorization_endpoint":                baseURL + "/authorize",
			"token_endpoint":                        baseURL + "/token",
			"response_types_supported":              []string{"code"},
			"grant_types_supported":                 []string{"authorization_code"},
			"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post"},
		})
	})

	// Wire web UI (always enabled — setup page, settings, and source config depend on it)
	{
		// Create Services
		// 1. Task Service
		taskSvc := service.NewTaskService(store, taskNotifier)

		// 2. Note Service
		// We need to identify Supernote and Boox components from sources
		var ns notestore.NoteStore
		var proc processor.Processor
		var scanner service.FileScanner
		var booxStore service.BooxStore
		var booxImporter service.BooxImporter
		var booxProc service.BooxProcessor

		for _, s := range sources {
			switch s.Type() {
			case "supernote":
				if snSource, ok := s.(*supernote.Source); ok {
					ns = snSource.NoteStore()
					proc = snSource.Processor()
					scanner = snSource.Pipeline()
				}
			case "boox":
				if booxSource, ok := s.(*boox.Source); ok {
					booxStore = booxSource.Processor().Store()
					booxImporter = booxSource.Processor()
					booxProc = booxSource.Processor()
				}
			}
		}

		// Wire Boox WebDAV server if Boox source is active
		if booxImporter != nil && booxNotesPath != "" {
			davHandler := ubwebdav.NewHandler(booxNotesPath, func(absPath string) {
				logger.Info("boox note uploaded", "path", absPath)
				if err := booxImporter.Enqueue(context.Background(), absPath); err != nil {
					logger.Error("enqueue boox job", "error", err, "path", absPath)
				}
			})
			mux.Handle("/webdav/", authMW.Wrap(davHandler))
			logger.Info("boox webdav enabled", "path", booxNotesPath)
		}

		booxCachePath := ""
		if booxNotesPath != "" {
			booxCachePath = filepath.Join(booxNotesPath, ".cache")
		}
		noteSvc := service.NewNoteService(ns, proc, booxStore, booxImporter, booxProc, si, scanner, noteDB, booxCachePath, booxNotesPath, logger)

		// 3. Search Service
		var chatStore *chat.Store
		if cfg.ChatEnabled {
			chatStore = chat.NewStore(noteDB)
		}
		searchSvc := service.NewSearchService(si, retriever, embedder, embedStore, cfg.OllamaEmbedModel, chatStore, cfg.ChatAPIURL, cfg.ChatModel, logger)

		// 4. Config Service
		syncProvider := &syncProviderAdapter{engine: syncEngine}
		configSvc = service.NewConfigService(noteDB, syncProvider, cfg)

		webHandler = web.NewHandler(taskSvc, noteSvc, searchSvc, configSvc, noteDB, snNotesPath, booxNotesPath, logger, broadcaster)

		// OAuth2 flow for Claude.ai
		// /authorize requires user auth (browser login)
		mux.Handle("/authorize", authMW.Wrap(http.HandlerFunc(webHandler.HandleOAuthAuthorize)))
		// /token is called by Claude's backend (no browser/user auth)
		mux.HandleFunc("/token", webHandler.HandleOAuthToken)

		mux.Handle("/", authMW.Wrap(webHandler))
	}

	// Wire MCP server at /mcp/ — speaks MCP protocol for Claude Web and other MCP clients.
	// Tools proxy to the local JSON API using the same auth credentials.
	{
		mcpAPIClient := newMCPAPIClient("http://localhost"+bootstrapCfg.listenAddr, internalToken, logger, cfg.LogVerboseAPI)
		mcpServer := mcp.NewServer(&mcp.Implementation{
			Name:    "ultrabridge-notes",
			Version: "1.0.0",
		}, nil)
		registerMCPTools(mcpServer, mcpAPIClient)
		mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
			return mcpServer
		}, nil)
		// Register on both with and without trailing slash to avoid redirects
		wrappedMCP := authMW.Wrap(http.StripPrefix("/mcp", mcpHandler))
		mux.Handle("/mcp", wrappedMCP)
		mux.Handle("/mcp/", wrappedMCP)
		logger.Info("mcp server enabled", "path", "/mcp")
	}

	// Wire middleware layers: logging -> setup (outermost layer).
	// Setup middleware allows /setup and /setup/save through, redirects other requests to /setup if credentials missing.
	// Individual routes are wrapped with auth middleware at registration time.
	logHandler := logging.RequestID(logger)(mux)
	handler := web.SetupMiddleware(noteDB, logHandler)

	// Launch the device-facing SPC listener (constructed above in server mode).
	// In client mode spcSrv is nil and nothing starts — UB behaves as before.
	if spcSrv != nil {
		go func() {
			logger.Info("spc server starting", "addr", cfg.SPCListenAddr, "tls", cfg.SPCTLSCert != "")
			if err := spcSrv.Run(); err != nil {
				logger.Error("spc server error", "error", err)
			}
		}()
	}

	// Setup graceful shutdown with signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start HTTP server in a goroutine so we can wait for signals
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("ultrabridge starting", "addr", bootstrapCfg.listenAddr)
		serverErr <- http.ListenAndServe(bootstrapCfg.listenAddr, handler)
	}()

	// Wait for shutdown signal or server error
	select {
	case err := <-serverErr:
		if err != nil {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	case sig := <-sigChan:
		logger.Info("shutdown signal received", "signal", sig)

		// Cancel the backfill goroutine
		if backfillCancel != nil {
			backfillCancel()
		}
	}
}

// bootstrapConfig holds the minimal config needed before DB opens.
type bootstrapConfig struct {
	logLevel         string
	logFormat        string
	logFile          string
	logFileMaxMB     int
	logFileMaxAge    int
	logFileMaxBackup int
	logSyslogAddr    string
	dbPath           string
	taskDBPath       string
	listenAddr       string
	dbEnvPath        string
	passwordHashPath string
}

// envOrDefault returns the value of an environment variable or a default.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envIntOrDefault returns the value of an environment variable as an int, or a default.
func envIntOrDefault(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// envBoolOrDefault returns the value of an environment variable as a bool, or a default.
func envBoolOrDefault(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return strings.EqualFold(v, "true") || v == "1"
}

// loadDBEnv loads MariaDB credentials from environment variables or a .dbenv file.
// Returns (dbName, dbUser, dbPassword, error).
// Env vars take precedence over file values.
func loadDBEnv(dbEnvPath string) (string, string, string, error) {
	dbName := os.Getenv("MYSQL_DATABASE")
	dbUser := os.Getenv("MYSQL_USER")
	dbPassword := os.Getenv("MYSQL_PASSWORD")

	// If we got all three from env, we're done
	if dbName != "" && dbUser != "" && dbPassword != "" {
		return dbName, dbUser, dbPassword, nil
	}

	// Try file as fallback
	f, err := os.Open(dbEnvPath)
	if err != nil {
		if dbName != "" || dbUser != "" {
			// Got partial config from env, file is optional
			return dbName, dbUser, dbPassword, nil
		}
		return "", "", "", fmt.Errorf("open %s: %w", dbEnvPath, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "MYSQL_DATABASE":
			if dbName == "" {
				dbName = val
			}
		case "MYSQL_USER":
			if dbUser == "" {
				dbUser = val
			}
		case "MYSQL_PASSWORD":
			if dbPassword == "" {
				dbPassword = val
			}
		}
	}
	return dbName, dbUser, dbPassword, scanner.Err()
}
