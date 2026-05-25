package supernote

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/notestore"
	"github.com/sysop/ultrabridge/internal/pipeline"
	"github.com/sysop/ultrabridge/internal/processor"
	"github.com/sysop/ultrabridge/internal/source"
)

// Source implements source.Source for the Supernote notes pipeline.
type Source struct {
	name string
	cfg  Config
	db   *sql.DB
	deps source.SharedDeps

	ns   *notestore.Store // created in Start()
	proc *processor.Store
	pl   *pipeline.Pipeline
}

// NewSource constructs a Supernote source from a source row and dependencies.
func NewSource(db *sql.DB, row source.SourceRow, deps source.SharedDeps) (*Source, error) {
	var cfg Config
	if err := json.Unmarshal([]byte(row.ConfigJSON), &cfg); err != nil {
		return nil, fmt.Errorf("parse supernote config: %w", err)
	}
	return &Source{
		name: row.Name,
		cfg:  cfg,
		db:   db,
		deps: deps,
	}, nil
}

func (s *Source) Type() string { return "supernote" }
func (s *Source) Name() string { return s.name }

func (s *Source) Start(ctx context.Context) error {
	s.ns = notestore.New(s.db, s.cfg.NotesPath)

	workerCfg := processor.WorkerConfig{
		OCREnabled:  s.deps.OCRClient != nil,
		BackupPath:  s.cfg.BackupPath,
		MaxFileMB:   s.deps.OCRMaxFileMB,
		Indexer:     s.deps.Indexer,
		Embedder:    s.deps.Embedder,
		EmbedModel:  s.deps.EmbedModel,
		EmbedStore:  s.deps.EmbedStore,
		OCRPrompt: func() string {
			v, _ := notedb.GetSetting(context.Background(), s.db, "sn_ocr_prompt")
			return v
		},
		InjectEnabled: func() bool {
			v, _ := notedb.GetSetting(context.Background(), s.db, "sn_inject_enabled")
			return v != "false"
		},
	}
	if s.deps.OCRClient != nil {
		workerCfg.OCRClient = s.deps.OCRClient
	}

	s.proc = processor.New(s.db, workerCfg)
	if err := s.proc.Start(ctx); err != nil {
		return fmt.Errorf("start processor: %w", err)
	}

	s.pl = pipeline.New(pipeline.Config{
		NotesPath: s.cfg.NotesPath,
		Store:     s.ns,
		Proc:      s.proc,
		Logger:    s.deps.Logger,
	})
	s.pl.Start(ctx)
	return nil
}

func (s *Source) Stop() {
	if s.pl != nil {
		s.pl.Close()
	}
	if s.proc != nil {
		s.proc.Stop() // error logged internally
	}
}

// Processor returns the internal processor for backward compatibility (Phase 5).
// This allows main.go to extract the processor for use in the web handler.
// TODO: Phase 6 will refactor the web handler to work directly with sources.
func (s *Source) Processor() *processor.Store {
	return s.proc
}

// Pipeline returns the internal pipeline for backward compatibility (Phase 5).
// This allows main.go to extract the pipeline (FileScanner) for use in the web handler.
// TODO: Phase 6 will refactor the web handler to work directly with sources.
func (s *Source) Pipeline() *pipeline.Pipeline {
	return s.pl
}

// NoteStore returns the internal note store for backward compatibility (Phase 5).
// This allows main.go to extract the note store for use in the web handler.
// TODO: Phase 6 will refactor the web handler to work directly with sources.
func (s *Source) NoteStore() *notestore.Store {
	return s.ns
}
