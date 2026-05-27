// Package forestnote is the source adapter for ForestNote, UB's own roll-our-own
// device-sync client. Unlike Supernote/Boox (which wrap a vendor's files), UB
// owns the ForestNote schema end to end, so this source manages the full sync
// stack: the syncstore mirror, the render→OCR→index→embed bridge, and the
// syncsvc relay behind /sync/v1. The device endpoint itself is mounted in
// main.go (consistent with how Boox's WebDAV endpoint is wired); this source
// owns the processing and exposes the relay service + store for main and the
// web layer to consume.
package forestnote

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/sysop/ultrabridge/internal/source"
	"github.com/sysop/ultrabridge/internal/syncbridge"
	"github.com/sysop/ultrabridge/internal/syncstore"
	"github.com/sysop/ultrabridge/internal/syncsvc"
)

// ForestNoteDeps holds ForestNote-specific dependencies not in source.SharedDeps.
// Indexer and EmbedStore are the Delete-capable concrete services (the search
// store and embedding store): the syncbridge interfaces require a Delete method
// that the narrower SharedDeps types omit, so main captures the concretes in the
// factory closure (mirroring how boox.BooxDeps carries its ContentDeleter).
type ForestNoteDeps struct {
	Indexer    syncbridge.Indexer    // never nil
	EmbedStore syncbridge.EmbedStore // nil when embedding is disabled
	// OCRPrompt is read per page so a Settings change applies without a restart
	// (empty → the bridge's built-in default prompt).
	OCRPrompt func() string
}

// Source implements source.Source for the ForestNote device-sync pipeline.
type Source struct {
	name   string
	cfg    Config
	db     *sql.DB
	deps   source.SharedDeps
	fnDeps ForestNoteDeps

	store   *syncstore.Store
	bridge  *syncbridge.Bridge
	syncSvc *syncsvc.Service
}

// NewSource constructs a ForestNote source from a source row and dependencies.
func NewSource(db *sql.DB, row source.SourceRow, deps source.SharedDeps, fnDeps ForestNoteDeps) (*Source, error) {
	var cfg Config
	if row.ConfigJSON != "" {
		if err := json.Unmarshal([]byte(row.ConfigJSON), &cfg); err != nil {
			return nil, fmt.Errorf("parse forestnote config: %w", err)
		}
	}
	if cfg.BatchLimit <= 0 {
		cfg.BatchLimit = defaultBatchLimit
	}
	return &Source{name: row.Name, cfg: cfg, db: db, deps: deps, fnDeps: fnDeps}, nil
}

func (s *Source) Type() string { return "forestnote" }
func (s *Source) Name() string { return s.name }

// Start migrates the mirror, builds the bridge, and starts the relay service.
// Idempotent within a process lifecycle (Migrate is idempotent).
func (s *Source) Start(ctx context.Context) error {
	if err := syncstore.Migrate(ctx, s.db); err != nil {
		return fmt.Errorf("syncstore migrate: %w", err)
	}
	s.store = syncstore.New(s.db)

	// Guard concrete-pointer deps so a nil *OCRClient/Embedder/EmbedStore isn't
	// boxed into a non-nil interface (would panic on call) — same discipline as
	// the previous inline wiring in main.go.
	bdeps := syncbridge.Deps{
		Indexer:    s.fnDeps.Indexer,
		EmbedModel: s.deps.EmbedModel,
		OCRPrompt:  s.fnDeps.OCRPrompt,
	}
	if s.deps.OCRClient != nil {
		bdeps.OCR = s.deps.OCRClient
	}
	if s.deps.Embedder != nil {
		bdeps.Embedder = s.deps.Embedder
	}
	if s.fnDeps.EmbedStore != nil {
		bdeps.EmbedStore = s.fnDeps.EmbedStore
	}

	logger := s.deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	s.bridge = syncbridge.New(s.store, bdeps, logger)
	s.bridge.Start(ctx)
	s.syncSvc = syncsvc.New(s.store, s.cfg.BatchLimit, s.bridge, logger)
	return nil
}

func (s *Source) Stop() {
	if s.bridge != nil {
		s.bridge.Stop()
	}
}

// SyncService returns the relay service main.go mounts at /sync/v1 (nil until
// Start). Mirrors the boox.Source.Processor() accessor pattern.
func (s *Source) SyncService() *syncsvc.Service { return s.syncSvc }

// Store returns the syncstore mirror, consumed by the note service for the
// Files tab inventory and on-the-fly page rendering (nil until Start).
func (s *Source) Store() *syncstore.Store { return s.store }
