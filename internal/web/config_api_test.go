package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sysop/ultrabridge/internal/appconfig"
	"github.com/sysop/ultrabridge/internal/logging"
	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/service"
)

func initTestDB(t *testing.T) *sql.DB {
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	return db
}

func TestGetConfigRedacts(t *testing.T) {
	db := initTestDB(t)
	defer db.Close()

	// Set initial config
	cfg := &appconfig.Config{
		Username:     "testuser",
		PasswordHash: "secret-hash",
		OCRAPIKey:    "secret-key",
	}
	appconfig.Save(context.Background(), db, cfg)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	configSvc := service.NewConfigService(db, cfg)
	h := NewHandler(nil, nil, nil, configSvc, db, "", "", logger, broadcaster)

	req := httptest.NewRequest("GET", "/api/config", nil)
	w := httptest.NewRecorder()
	h.handleGetConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/config returned status %d", w.Code)
	}

	var resp RedactedConfig
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Username != "testuser" {
		t.Errorf("expected username testuser, got %s", resp.Username)
	}
	if resp.PasswordHash != "[set]" {
		t.Errorf("expected redacted password, got %s", resp.PasswordHash)
	}
	if resp.OCRAPIKey != "[set]" {
		t.Errorf("expected redacted ocr key, got %s", resp.OCRAPIKey)
	}
}

func TestPutConfigHashesPassword(t *testing.T) {
	db := initTestDB(t)
	defer db.Close()

	cfg := &appconfig.Config{Username: "user"}
	appconfig.Save(context.Background(), db, cfg)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	configSvc := service.NewConfigService(db, cfg)
	h := NewHandler(nil, nil, nil, configSvc, db, "", "", logger, broadcaster)

	incoming := IncomingConfig{
		Password: "newpassword123",
	}
	body, _ := json.Marshal(incoming)
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.handlePutConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("PUT /api/config returned status %d, want 200", w.Code)
	}

	// Verify password was hashed in DB
	updated, _ := appconfig.Load(context.Background(), db)
	if updated.PasswordHash == "" || updated.PasswordHash == "newpassword123" {
		t.Errorf("password hash is invalid: %q", updated.PasswordHash)
	}
}

func TestPutConfigDetectsRestartRequired(t *testing.T) {
	db := initTestDB(t)
	defer db.Close()

	cfg := &appconfig.Config{Username: "olduser"}
	appconfig.Save(context.Background(), db, cfg)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	configSvc := service.NewConfigService(db, cfg)
	h := NewHandler(nil, nil, nil, configSvc, db, "", "", logger, broadcaster)

	incoming := IncomingConfig{
		Username: "newuser",
	}
	body, _ := json.Marshal(incoming)
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.handlePutConfig(w, req)

	if h.config.IsRestartRequired() != true {
		t.Error("IsRestartRequired = false, want true after username change")
	}
}

func TestHealthEndpointConfigDirty(t *testing.T) {
	configSvc := &mockConfigService{restartRequired: false}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broadcaster := logging.NewLogBroadcaster()
	h := NewHandler(nil, nil, nil, configSvc, nil, "", "", logger, broadcaster)

	// Mock health handler logic as in main.go
	healthHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":       "ok",
			"config_dirty": h.config.IsRestartRequired(),
		})
	}

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	healthHandler(w, req)

	var resp struct {
		ConfigDirty bool `json:"config_dirty"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.ConfigDirty {
		t.Error("config_dirty = true, want false")
	}

	configSvc.restartRequired = true
	w = httptest.NewRecorder()
	healthHandler(w, req)
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.ConfigDirty {
		t.Error("config_dirty = false, want true")
	}
}
