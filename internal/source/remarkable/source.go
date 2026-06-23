package remarkable

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sysop/ultrabridge/internal/source"
)

// Source implements source.Source for the reMarkable sync server.
type Source struct {
	name     string
	cfg      Config
	db       *sql.DB
	deps     source.SharedDeps
	store    *store
	protocol *protocol
	hub      *hub
}

// NewSource constructs a reMarkable source from a source row and dependencies.
func NewSource(db *sql.DB, row source.SourceRow, deps source.SharedDeps) (*Source, error) {
	var cfg Config
	if err := json.Unmarshal([]byte(row.ConfigJSON), &cfg); err != nil {
		return nil, fmt.Errorf("parse remarkable config: %w", err)
	}
	if strings.TrimSpace(cfg.DataPath) == "" {
		return nil, fmt.Errorf("parse remarkable config: data_path is required")
	}
	return &Source{name: row.Name, cfg: cfg, db: db, deps: deps}, nil
}

func (s *Source) Type() string { return "remarkable" }
func (s *Source) Name() string { return s.name }

func (s *Source) Start(ctx context.Context) error {
	if err := migrate(ctx, s.db); err != nil {
		return err
	}
	s.store = newStore(s.db, s.cfg.DataPath)
	if err := s.store.ensurePaths(); err != nil {
		return fmt.Errorf("remarkable ensure paths: %w", err)
	}
	logger := s.deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s.hub = newHub(logger)
	s.protocol = newProtocol(s.cfg, s.store, logger, s.hub)
	return nil
}

func (s *Source) Stop() {
	if s.hub != nil {
		s.hub.close()
	}
}

// RegisterRoutes mounts the device-facing reMarkable protocol surface.
func (s *Source) RegisterRoutes(mux *http.ServeMux) {
	if s.protocol != nil {
		s.protocol.register(mux)
	}
}

// Devices lists the known paired devices for the single shared account.
func (s *Source) Devices(ctx context.Context) ([]DeviceRow, error) {
	if s.store == nil {
		return nil, fmt.Errorf("remarkable source not started")
	}
	return s.store.listDevices(ctx)
}

// ListDocuments returns the synced document/folder tree (read-only). It reads
// the modern sync-v3 blob hashtree when present and falls back to the legacy
// document-storage v2 metadata table otherwise.
func (s *Source) ListDocuments(ctx context.Context) ([]Document, error) {
	if s.store == nil {
		return nil, fmt.Errorf("remarkable source not started")
	}
	return s.store.listDocumentTree(ctx)
}
