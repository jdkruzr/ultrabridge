package boox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/sysop/ultrabridge/internal/booxpipeline"
	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/source"
)

// BooxDeps holds Boox-specific dependencies not in source.SharedDeps.
type BooxDeps struct {
	ContentDeleter booxpipeline.ContentDeleter
	OnTodosFound   func(ctx context.Context, notePath string, todos []booxpipeline.TodoItem)
}

// Source implements source.Source for the Boox notes pipeline.
type Source struct {
	name     string
	cfg      Config
	db       *sql.DB
	deps     source.SharedDeps
	booxDeps BooxDeps
	proc     *booxpipeline.Processor
}

// NewSource constructs a Boox source from a source row and dependencies.
func NewSource(db *sql.DB, row source.SourceRow, deps source.SharedDeps, booxDeps BooxDeps) (*Source, error) {
	var cfg Config
	if err := json.Unmarshal([]byte(row.ConfigJSON), &cfg); err != nil {
		return nil, fmt.Errorf("parse boox config: %w", err)
	}
	return &Source{
		name:     row.Name,
		cfg:      cfg,
		db:       db,
		deps:     deps,
		booxDeps: booxDeps,
	}, nil
}

func (s *Source) Type() string { return "boox" }
func (s *Source) Name() string { return s.name }

func (s *Source) Start(ctx context.Context) error {
	workerCfg := booxpipeline.WorkerConfig{
		Indexer:        s.deps.Indexer,
		ContentDeleter: s.booxDeps.ContentDeleter,
		CachePath:      filepath.Join(s.cfg.NotesPath, ".cache"),
		Embedder:       s.deps.Embedder,
		EmbedModel:     s.deps.EmbedModel,
		EmbedStore:     s.deps.EmbedStore,
		OCRPrompt: func() string {
			v, _ := notedb.GetSetting(context.Background(), s.db, "boox_ocr_prompt")
			return v
		},
		TodoEnabled: func() bool {
			v, _ := notedb.GetSetting(context.Background(), s.db, "boox_todo_enabled")
			return v == "true"
		},
		TodoPrompt: func() string {
			v, _ := notedb.GetSetting(context.Background(), s.db, "boox_todo_prompt")
			return v
		},
		TodoInkColors: func() []uint32 {
			v, _ := notedb.GetSetting(context.Background(), s.db, "boox_todo_ink_colors")
			return booxpipeline.ParseInkColors(v)
		},
		OnTodosFound: s.booxDeps.OnTodosFound,
	}
	if s.deps.OCRClient != nil {
		workerCfg.OCR = s.deps.OCRClient
	}

	logger := s.deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	s.proc = booxpipeline.New(s.db, s.cfg.NotesPath, workerCfg, logger)
	if err := s.proc.Start(ctx); err != nil {
		return fmt.Errorf("start boox processor: %w", err)
	}
	return nil
}

func (s *Source) Stop() {
	if s.proc != nil {
		s.proc.Stop()
	}
}

// Processor returns the internal processor for backward compatibility (Phase 5).
// This allows main.go to extract the processor for use in the web handler.
// TODO: Phase 6 will refactor the web handler to work directly with sources.
func (s *Source) Processor() *booxpipeline.Processor {
	return s.proc
}
