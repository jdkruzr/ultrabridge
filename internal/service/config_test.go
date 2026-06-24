package service

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sysop/ultrabridge/internal/appconfig"
	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/source"
)

func openConfigServiceDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestConfigServiceConfigLifecycle(t *testing.T) {
	ctx := context.Background()
	db := openConfigServiceDB(t)
	svc := NewConfigService(db, &appconfig.Config{}).(*configService)

	cfgAny, err := svc.GetConfig(ctx)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	cfg := cfgAny.(*appconfig.Config)
	if cfg.LogLevel != "info" {
		t.Fatalf("default LogLevel = %q, want info", cfg.LogLevel)
	}

	if err := svc.UpdateConfig(ctx, "not a config"); err == nil {
		t.Fatal("UpdateConfig with wrong type should error")
	}
	cfg.LogLevel = "debug" // restart-required key
	if err := svc.UpdateConfig(ctx, cfg); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if !svc.IsRestartRequired() {
		t.Fatal("restart-required config change did not set dirty flag")
	}
	reloaded, err := appconfig.Load(ctx, db)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if reloaded.LogLevel != "debug" {
		t.Fatalf("persisted LogLevel = %q, want debug", reloaded.LogLevel)
	}
}

func TestConfigServiceSourcesLifecycle(t *testing.T) {
	ctx := context.Background()
	db := openConfigServiceDB(t)
	svc := NewConfigService(db, &appconfig.Config{}).(*configService)

	if err := svc.AddSource(ctx, "not a source"); err == nil {
		t.Fatal("AddSource with wrong type should error")
	}
	if err := svc.AddSource(ctx, &source.SourceRow{Type: "forestnote", Name: "FN", Enabled: true, ConfigJSON: `{"batch_limit":42}`}); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	if !svc.IsRestartRequired() {
		t.Fatal("AddSource should mark config dirty")
	}

	rowsAny, err := svc.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	rows := rowsAny.([]source.SourceRow)
	if len(rows) != 1 || rows[0].Name != "FN" || !rows[0].Enabled {
		t.Fatalf("sources after add = %+v", rows)
	}

	rows[0].Name = "ForestNote"
	rows[0].Enabled = false
	if err := svc.UpdateSource(ctx, "", &rows[0]); err != nil {
		t.Fatalf("UpdateSource: %v", err)
	}
	rowsAny, err = svc.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources after update: %v", err)
	}
	rows = rowsAny.([]source.SourceRow)
	if rows[0].Name != "ForestNote" || rows[0].Enabled {
		t.Fatalf("sources after update = %+v", rows)
	}

	if err := svc.DeleteSource(ctx, "999999"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("DeleteSource missing: got %v, want sql.ErrNoRows", err)
	}
	if err := svc.DeleteSource(ctx, "1"); err != nil {
		t.Fatalf("DeleteSource existing: %v", err)
	}
	rowsAny, err = svc.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources after delete: %v", err)
	}
	if rows := rowsAny.([]source.SourceRow); len(rows) != 0 {
		t.Fatalf("sources after delete = %+v, want none", rows)
	}
}
