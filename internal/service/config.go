package service

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"sync/atomic"

	"github.com/sysop/ultrabridge/internal/appconfig"
	"github.com/sysop/ultrabridge/internal/source"
)

type configService struct {
	noteDB        *sql.DB
	runningConfig *appconfig.Config
	configDirty   atomic.Bool
}

func NewConfigService(
	db *sql.DB,
	runningConfig *appconfig.Config,
) ConfigService {
	return &configService{
		noteDB:        db,
		runningConfig: runningConfig,
	}
}

func (s *configService) GetConfig(ctx context.Context) (interface{}, error) {
	if s.noteDB == nil {
		return nil, fmt.Errorf("database not available")
	}
	return appconfig.Load(ctx, s.noteDB)
}

func (s *configService) UpdateConfig(ctx context.Context, cfg interface{}) error {
	newCfg, ok := cfg.(*appconfig.Config)
	if !ok {
		return fmt.Errorf("invalid config type")
	}

	result, err := appconfig.Save(ctx, s.noteDB, newCfg)
	if err != nil {
		return err
	}

	if result.RestartRequired {
		s.configDirty.Store(true)
	}

	return nil
}

func (s *configService) IsRestartRequired() bool {
	return s.configDirty.Load()
}

func (s *configService) ListSources(ctx context.Context) (interface{}, error) {
	if s.noteDB == nil {
		return nil, fmt.Errorf("database not available")
	}
	return source.ListSources(ctx, s.noteDB)
}

func (s *configService) AddSource(ctx context.Context, src interface{}) error {
	newSrc, ok := src.(*source.SourceRow)
	if !ok {
		return fmt.Errorf("invalid source type")
	}
	if _, err := source.AddSource(ctx, s.noteDB, *newSrc); err != nil {
		return err
	}
	s.configDirty.Store(true)
	return nil
}

func (s *configService) UpdateSource(ctx context.Context, id string, src interface{}) error {
	updatedSrc, ok := src.(*source.SourceRow)
	if !ok {
		return fmt.Errorf("invalid source type")
	}
	rowID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || rowID <= 0 {
		return fmt.Errorf("invalid source id")
	}
	updatedSrc.ID = rowID
	if err := source.UpdateSource(ctx, s.noteDB, *updatedSrc); err != nil {
		return err
	}
	s.configDirty.Store(true)
	return nil
}

func (s *configService) DeleteSource(ctx context.Context, id string) error {
	res, err := s.noteDB.ExecContext(ctx, "DELETE FROM sources WHERE id = ?", id)
	if err != nil {
		return err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return sql.ErrNoRows
	}
	s.configDirty.Store(true)
	return nil
}
