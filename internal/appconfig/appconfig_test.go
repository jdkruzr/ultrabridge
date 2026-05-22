package appconfig

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/notedb"
)

// openTestDB opens an in-memory SQLite database for testing.
func openTestDB(t *testing.T) *sql.DB {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("failed to open test DB: %v", err)
	}
	return db
}

// TestLoadReadsFromDB verifies that Load reads config keys from the DB.
// Covers: platform-neutral-config.AC1.1
func TestLoadReadsFromDB(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Pre-populate some settings.
	if err := notedb.SetSetting(ctx, db, KeyUsername, "alice"); err != nil {
		t.Fatalf("SetSetting failed: %v", err)
	}
	if err := notedb.SetSetting(ctx, db, KeyOCREnabled, "true"); err != nil {
		t.Fatalf("SetSetting failed: %v", err)
	}
	if err := notedb.SetSetting(ctx, db, KeyOCRConcurrency, "4"); err != nil {
		t.Fatalf("SetSetting failed: %v", err)
	}
	if err := notedb.SetSetting(ctx, db, KeyLogLevel, "debug"); err != nil {
		t.Fatalf("SetSetting failed: %v", err)
	}

	// Load the config.
	cfg, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Verify typed fields match DB values.
	if cfg.Username != "alice" {
		t.Errorf("expected Username=alice, got %q", cfg.Username)
	}
	if !cfg.OCREnabled {
		t.Errorf("expected OCREnabled=true, got false")
	}
	if cfg.OCRConcurrency != 4 {
		t.Errorf("expected OCRConcurrency=4, got %d", cfg.OCRConcurrency)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("expected LogLevel=debug, got %q", cfg.LogLevel)
	}
}

// TestLoadAppliesDefaults verifies that missing keys get default values.
func TestLoadAppliesDefaults(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Load with empty DB.
	cfg, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Verify defaults are applied.
	if cfg.OCRFormat != "anthropic" {
		t.Errorf("expected OCRFormat=anthropic (default), got %q", cfg.OCRFormat)
	}
	if cfg.OllamaURL != "http://localhost:11434" {
		t.Errorf("expected OllamaURL=http://localhost:11434 (default), got %q", cfg.OllamaURL)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("expected LogLevel=info (default), got %q", cfg.LogLevel)
	}
	if cfg.WebEnabled != true {
		t.Errorf("expected WebEnabled=true (default), got false")
	}
	if cfg.MCPPort != 8081 {
		t.Errorf("expected MCPPort=8081 (default), got %d", cfg.MCPPort)
	}
}

// TestMCPPortRoundtrip verifies MCPPort survives Save → Load via the
// settings DB and that env var override + default still apply.
func TestMCPPortRoundtrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	cfg, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.MCPPort != 8081 {
		t.Fatalf("expected default MCPPort=8081, got %d", cfg.MCPPort)
	}

	cfg.MCPPort = 9091
	if _, err := Save(ctx, db, cfg); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	reloaded, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if reloaded.MCPPort != 9091 {
		t.Errorf("expected MCPPort=9091 after roundtrip, got %d", reloaded.MCPPort)
	}

	// Env var should override the persisted DB value.
	t.Setenv("UB_MCP_PORT", "12345")
	envOverlay, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if envOverlay.MCPPort != 12345 {
		t.Errorf("expected env override MCPPort=12345, got %d", envOverlay.MCPPort)
	}
}

// TestLoadEnvVarOverride verifies that env vars override DB values.
// Covers: platform-neutral-config.AC1.3
func TestLoadEnvVarOverride(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Set a value in DB.
	if err := notedb.SetSetting(ctx, db, KeyOCRFormat, "openai"); err != nil {
		t.Fatalf("SetSetting failed: %v", err)
	}

	// Set an env var to override it.
	t.Cleanup(func() { os.Unsetenv("UB_OCR_FORMAT") })
	os.Setenv("UB_OCR_FORMAT", "anthropic")

	cfg, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Env var should win.
	if cfg.OCRFormat != "anthropic" {
		t.Errorf("expected OCRFormat=anthropic (from env), got %q", cfg.OCRFormat)
	}
}

// TestLoadFirstBootFallsBackToEnv verifies that with no DB values, env vars are used.
// Covers: platform-neutral-config.AC1.8
func TestLoadFirstBootFallsBackToEnv(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Set env vars but leave DB empty.
	t.Cleanup(func() {
		os.Unsetenv("UB_USERNAME")
		os.Unsetenv("UB_LOG_LEVEL")
	})
	os.Setenv("UB_USERNAME", "bob")
	os.Setenv("UB_LOG_LEVEL", "warn")

	cfg, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Env vars should be used.
	if cfg.Username != "bob" {
		t.Errorf("expected Username=bob (from env), got %q", cfg.Username)
	}
	if cfg.LogLevel != "warn" {
		t.Errorf("expected LogLevel=warn (from env), got %q", cfg.LogLevel)
	}
}

// TestSaveWritesChangedKeys verifies that Save writes changed keys to DB.
// Covers: platform-neutral-config.AC1.2
func TestSaveWritesChangedKeys(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Set initial values.
	if err := notedb.SetSetting(ctx, db, KeyUsername, "alice"); err != nil {
		t.Fatalf("SetSetting failed: %v", err)
	}
	if err := notedb.SetSetting(ctx, db, KeyLogLevel, "info"); err != nil {
		t.Fatalf("SetSetting failed: %v", err)
	}

	// Load the config.
	cfg, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Modify it.
	cfg.Username = "bob"
	cfg.LogLevel = "debug"

	// Save it.
	result, err := Save(ctx, db, cfg)
	if err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Verify changed keys are reported.
	if len(result.ChangedKeys) != 2 {
		t.Errorf("expected 2 changed keys, got %d: %v", len(result.ChangedKeys), result.ChangedKeys)
	}

	// Verify DB has new values.
	username, err := notedb.GetSetting(ctx, db, KeyUsername)
	if err != nil {
		t.Fatalf("GetSetting failed: %v", err)
	}
	if username != "bob" {
		t.Errorf("expected DB Username=bob, got %q", username)
	}

	logLevel, err := notedb.GetSetting(ctx, db, KeyLogLevel)
	if err != nil {
		t.Fatalf("GetSetting failed: %v", err)
	}
	if logLevel != "debug" {
		t.Errorf("expected DB LogLevel=debug, got %q", logLevel)
	}
}

// TestSaveDetectsRestartRequired verifies that Save detects restart-required keys.
func TestSaveDetectsRestartRequired(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Load initial config.
	cfg, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Modify a restart-required key.
	cfg.OCREnabled = true

	result, err := Save(ctx, db, cfg)
	if err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// RestartRequired should be true.
	if !result.RestartRequired {
		t.Errorf("expected RestartRequired=true, got false")
	}
}

// TestSaveNoChanges verifies that Save with no changes returns empty ChangedKeys.
func TestSaveNoChanges(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Set some initial values.
	if err := notedb.SetSetting(ctx, db, KeyUsername, "alice"); err != nil {
		t.Fatalf("SetSetting failed: %v", err)
	}

	// Load and save without changes.
	cfg, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	result, err := Save(ctx, db, cfg)
	if err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// No changes should be reported.
	if len(result.ChangedKeys) != 0 {
		t.Errorf("expected 0 changed keys, got %d: %v", len(result.ChangedKeys), result.ChangedKeys)
	}
	if result.RestartRequired {
		t.Errorf("expected RestartRequired=false when no changes, got true")
	}
}

// TestBoolParsing verifies that bool fields are parsed correctly.
func TestBoolParsing(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name     string
		dbValue  string
		expected bool
	}{
		{"true", "true", true},
		{"false", "false", false},
		{"1", "1", true},
		{"0", "0", false},
		{"True", "True", true},
		{"FALSE", "FALSE", false},
		{"empty", "", true}, // Empty gets the default: true
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear DB for this test.
			testDB := openTestDB(t)

			if tt.dbValue != "" {
				if err := notedb.SetSetting(ctx, testDB, KeyWebEnabled, tt.dbValue); err != nil {
					t.Fatalf("SetSetting failed: %v", err)
				}
			}

			cfg, err := Load(ctx, testDB)
			if err != nil {
				t.Fatalf("Load failed: %v", err)
			}

			if cfg.WebEnabled != tt.expected {
				t.Errorf("expected WebEnabled=%v for %q, got %v", tt.expected, tt.dbValue, cfg.WebEnabled)
			}
		})
	}
}

// TestIntParsing verifies that int fields are parsed correctly.
func TestIntParsing(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Set int values.
	if err := notedb.SetSetting(ctx, db, KeyOCRConcurrency, "42"); err != nil {
		t.Fatalf("SetSetting failed: %v", err)
	}
	if err := notedb.SetSetting(ctx, db, KeyLogFileMaxMB, "123"); err != nil {
		t.Fatalf("SetSetting failed: %v", err)
	}

	cfg, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.OCRConcurrency != 42 {
		t.Errorf("expected OCRConcurrency=42, got %d", cfg.OCRConcurrency)
	}
	if cfg.LogFileMaxMB != 123 {
		t.Errorf("expected LogFileMaxMB=123, got %d", cfg.LogFileMaxMB)
	}
}

// TestIntParsingFallsBackToDefault verifies that invalid ints fall back to default.
func TestIntParsingFallsBackToDefault(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Set an invalid int value.
	if err := notedb.SetSetting(ctx, db, KeyOCRConcurrency, "not_a_number"); err != nil {
		t.Fatalf("SetSetting failed: %v", err)
	}

	cfg, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Should fall back to default (1).
	if cfg.OCRConcurrency != 1 {
		t.Errorf("expected OCRConcurrency=1 (default), got %d", cfg.OCRConcurrency)
	}
}

// TestRoundtrip verifies that Save followed by Load preserves all values.
func TestRoundtrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create a config with non-default values.
	original := &Config{
		Username:             "alice",
		PasswordHash:         "hashed_password",
		OCREnabled:           true,
		OCRAPIURL:            "https://api.anthropic.com",
		OCRAPIKey:            "secret_key",
		OCRModel:             "claude-3",
		OCRConcurrency:       8,
		OCRMaxFileMB:         100,
		OCRFormat:            "openai",
		EmbedEnabled:         true,
		OllamaURL:            "http://custom:11434",
		OllamaEmbedModel:     "custom-model",
		ChatEnabled:          true,
		ChatAPIURL:           "http://custom-chat:8000",
		ChatModel:            "custom-chat-model",
		SNSyncEnabled:        true,
		SNSyncInterval:       600,
		SNAPIURL:             "http://custom-sn:8080",
		SNAccount:            "account123",
		SNPassword:           "password123",
		LogLevel:             "debug",
		LogFormat:            "text",
		LogFile:              "/var/log/app.log",
		LogFileMaxMB:         100,
		LogFileMaxAge:        60,
		LogFileMaxBackup:     10,
		LogSyslogAddr:        "localhost:514",
		CalDAVCollectionName: "My Tasks",
		DueTimeMode:          "date_only",
		WebEnabled:           false,
		SocketIOURL:          "ws://custom:8080/socket.io/",
		DBHost:               "custom-db",
		DBPort:               "5432",
		DBEnvPath:            "/custom/.dbenv",
		UserID:               999,
	}

	// Save it.
	if _, err := Save(ctx, db, original); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Load it back.
	loaded, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Verify all fields match.
	if loaded.Username != original.Username {
		t.Errorf("Username mismatch: expected %q, got %q", original.Username, loaded.Username)
	}
	if loaded.PasswordHash != original.PasswordHash {
		t.Errorf("PasswordHash mismatch: expected %q, got %q", original.PasswordHash, loaded.PasswordHash)
	}
	if loaded.OCREnabled != original.OCREnabled {
		t.Errorf("OCREnabled mismatch: expected %v, got %v", original.OCREnabled, loaded.OCREnabled)
	}
	if loaded.OCRAPIURL != original.OCRAPIURL {
		t.Errorf("OCRAPIURL mismatch: expected %q, got %q", original.OCRAPIURL, loaded.OCRAPIURL)
	}
	if loaded.OCRConcurrency != original.OCRConcurrency {
		t.Errorf("OCRConcurrency mismatch: expected %d, got %d", original.OCRConcurrency, loaded.OCRConcurrency)
	}
	if loaded.ChatEnabled != original.ChatEnabled {
		t.Errorf("ChatEnabled mismatch: expected %v, got %v", original.ChatEnabled, loaded.ChatEnabled)
	}
	if loaded.DBPort != original.DBPort {
		t.Errorf("DBPort mismatch: expected %q, got %q", original.DBPort, loaded.DBPort)
	}
	if loaded.UserID != original.UserID {
		t.Errorf("UserID mismatch: expected %d, got %d", original.UserID, loaded.UserID)
	}
	if loaded.WebEnabled != original.WebEnabled {
		t.Errorf("WebEnabled mismatch: expected %v, got %v", original.WebEnabled, loaded.WebEnabled)
	}
}

// TestEnvironmentVariableOverride verifies complex env var scenarios.
func TestEnvironmentVariableOverride(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Pre-set DB values.
	if err := notedb.SetSetting(ctx, db, KeyOCREnabled, "false"); err != nil {
		t.Fatalf("SetSetting failed: %v", err)
	}
	if err := notedb.SetSetting(ctx, db, KeyOCRConcurrency, "2"); err != nil {
		t.Fatalf("SetSetting failed: %v", err)
	}

	// Set env vars.
	t.Cleanup(func() {
		os.Unsetenv("UB_OCR_ENABLED")
		os.Unsetenv("UB_OCR_CONCURRENCY")
	})
	os.Setenv("UB_OCR_ENABLED", "true")
	os.Setenv("UB_OCR_CONCURRENCY", "4")

	cfg, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Env vars should override DB values.
	if !cfg.OCREnabled {
		t.Errorf("expected OCREnabled=true (from env), got false")
	}
	if cfg.OCRConcurrency != 4 {
		t.Errorf("expected OCRConcurrency=4 (from env), got %d", cfg.OCRConcurrency)
	}
}

// TestSPCServerConfigDefaults verifies the SPC server keys default to a
// regression-safe state: client mode (no listener) and the :8089 listen addr.
// Covers: spc-phase-1.AC1.2
func TestSPCServerConfigDefaults(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	cfg, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.SPCMode != "client" {
		t.Errorf("expected SPCMode=client (default), got %q", cfg.SPCMode)
	}
	if cfg.SPCListenAddr != ":8089" {
		t.Errorf("expected SPCListenAddr=:8089 (default), got %q", cfg.SPCListenAddr)
	}
	if cfg.SPCTLSCert != "" || cfg.SPCTLSKey != "" {
		t.Errorf("expected empty TLS cert/key by default, got cert=%q key=%q", cfg.SPCTLSCert, cfg.SPCTLSKey)
	}
}

// TestSPCServerConfigRoundtrip verifies all four SPC server keys survive
// Save → Load via the settings DB.
func TestSPCServerConfigRoundtrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	cfg, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	cfg.SPCMode = "server"
	cfg.SPCListenAddr = ":9999"
	cfg.SPCTLSCert = "/etc/ssl/spc.crt"
	cfg.SPCTLSKey = "/etc/ssl/spc.key"

	if _, err := Save(ctx, db, cfg); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	reloaded, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if reloaded.SPCMode != "server" {
		t.Errorf("expected SPCMode=server after roundtrip, got %q", reloaded.SPCMode)
	}
	if reloaded.SPCListenAddr != ":9999" {
		t.Errorf("expected SPCListenAddr=:9999 after roundtrip, got %q", reloaded.SPCListenAddr)
	}
	if reloaded.SPCTLSCert != "/etc/ssl/spc.crt" {
		t.Errorf("expected SPCTLSCert preserved, got %q", reloaded.SPCTLSCert)
	}
	if reloaded.SPCTLSKey != "/etc/ssl/spc.key" {
		t.Errorf("expected SPCTLSKey preserved, got %q", reloaded.SPCTLSKey)
	}
}

// TestSPCModeEnvOverride verifies UB_SPC_MODE=server overrides a DB value of
// client (env beats DB), so an operator can force server mode without touching
// the settings table.
func TestSPCModeEnvOverride(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := notedb.SetSetting(ctx, db, KeySPCMode, "client"); err != nil {
		t.Fatalf("SetSetting failed: %v", err)
	}

	t.Setenv("UB_SPC_MODE", "server")
	cfg, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.SPCMode != "server" {
		t.Errorf("expected SPCMode=server (from env), got %q", cfg.SPCMode)
	}
}

// constantSECRET is the verbatim com/ratta/constants/Constant.java:46 SECRET —
// the SPC JWT signing secret. UB's default for KeySPCJWTSecret must equal it so
// UB-minted tokens verify against the same secret the device's tokens were
// signed with. A drift here silently breaks device auth, so we lock it exactly.
const constantSECRET = "suernotea1hK52bgkf9N7PQ5E3KDqKeCIT719a6kh04eSTSBLv7e9tPtw2L8S6pEDMy7lAIv2CYjg5Ncy7ep5zDS7hH9CDAZnLieo66g7F8iZmClK9a1xEEPewXLhkM4KTKI7pz2Lkl7Cds4MpClNvNCVHPbfWKNyiFSGUztbnmqDWgNAinPBNamwDUQpT8RwCO1wc9vYTTQsmXm8ByioHC3QkRMZtHZnIWWCkIWECPzSJGOowNliAavzVCMsKadYnsH322n"

// TestSPCAuthConfigDefaults verifies the 1b auth keys default correctly: the
// JWT secret defaults to Constant.SECRET, account/password default empty.
func TestSPCAuthConfigDefaults(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	cfg, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.SPCJWTSecret != constantSECRET {
		t.Errorf("SPCJWTSecret default must equal Constant.SECRET\n got  %q\n want %q", cfg.SPCJWTSecret, constantSECRET)
	}
	if cfg.SPCDeviceAccount != "" || cfg.SPCDevicePassword != "" {
		t.Errorf("expected empty account/password by default, got account=%q password=%q", cfg.SPCDeviceAccount, cfg.SPCDevicePassword)
	}
}

// TestSPCAuthConfigRoundtrip verifies the three 1b auth keys survive Save→Load.
func TestSPCAuthConfigRoundtrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	cfg, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	cfg.SPCJWTSecret = "custom-secret"
	cfg.SPCDeviceAccount = "starkruzr@gmail.com"
	cfg.SPCDevicePassword = "ehh1701jqb"

	if _, err := Save(ctx, db, cfg); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	reloaded, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if reloaded.SPCJWTSecret != "custom-secret" {
		t.Errorf("SPCJWTSecret not preserved: got %q", reloaded.SPCJWTSecret)
	}
	if reloaded.SPCDeviceAccount != "starkruzr@gmail.com" {
		t.Errorf("SPCDeviceAccount not preserved: got %q", reloaded.SPCDeviceAccount)
	}
	if reloaded.SPCDevicePassword != "ehh1701jqb" {
		t.Errorf("SPCDevicePassword not preserved: got %q", reloaded.SPCDevicePassword)
	}
}

// TestSPCJWTSecretEnvOverride verifies UB_SPC_JWT_SECRET overrides the DB value.
func TestSPCJWTSecretEnvOverride(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := notedb.SetSetting(ctx, db, KeySPCJWTSecret, "db-secret"); err != nil {
		t.Fatalf("SetSetting failed: %v", err)
	}

	t.Setenv("UB_SPC_JWT_SECRET", "env-secret")
	cfg, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.SPCJWTSecret != "env-secret" {
		t.Errorf("expected SPCJWTSecret=env-secret (from env), got %q", cfg.SPCJWTSecret)
	}
}

// TestIsSetupRequiredWithEmptyDB verifies that IsSetupRequired returns true when
// DB is empty and no env vars are set.
// Covers: platform-neutral-config.AC3.3
func TestIsSetupRequiredWithEmptyDB(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Clear any env vars that might interfere
	t.Cleanup(func() {
		os.Unsetenv("UB_USERNAME")
		os.Unsetenv("UB_PASSWORD_HASH")
		os.Unsetenv("UB_PASSWORD_HASH_PATH")
	})
	os.Unsetenv("UB_USERNAME")
	os.Unsetenv("UB_PASSWORD_HASH")
	os.Unsetenv("UB_PASSWORD_HASH_PATH")

	// With empty DB and no env vars, setup is required.
	if !IsSetupRequired(ctx, db) {
		t.Errorf("expected IsSetupRequired=true for empty DB, got false")
	}
}

// TestIsSetupRequiredWithDBCredentials verifies that IsSetupRequired returns false
// when credentials are in the database.
func TestIsSetupRequiredWithDBCredentials(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Set credentials in DB
	if err := notedb.SetSetting(ctx, db, KeyUsername, "alice"); err != nil {
		t.Fatalf("SetSetting failed: %v", err)
	}
	if err := notedb.SetSetting(ctx, db, KeyPasswordHash, "hashed_password"); err != nil {
		t.Fatalf("SetSetting failed: %v", err)
	}

	// With credentials in DB, setup is not required.
	if IsSetupRequired(ctx, db) {
		t.Errorf("expected IsSetupRequired=false when DB has credentials, got true")
	}
}

// TestIsSetupRequiredWithEnvVars verifies that IsSetupRequired returns false
// when env vars are set.
// Covers: platform-neutral-config.AC3.5
func TestIsSetupRequiredWithEnvVars(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Clear any env vars first
	t.Cleanup(func() {
		os.Unsetenv("UB_USERNAME")
		os.Unsetenv("UB_PASSWORD_HASH")
		os.Unsetenv("UB_PASSWORD_HASH_PATH")
	})

	// Set env vars (but leave DB empty)
	os.Setenv("UB_USERNAME", "bob")
	os.Setenv("UB_PASSWORD_HASH", "env_hashed_password")

	// With env vars set, setup is not required even if DB is empty.
	if IsSetupRequired(ctx, db) {
		t.Errorf("expected IsSetupRequired=false with env vars set, got true")
	}
}

// TestIsSetupRequiredWithPasswordHashFile verifies that IsSetupRequired returns false
// when a password hash file exists.
func TestIsSetupRequiredWithPasswordHashFile(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create a temporary password hash file
	tmpFile, err := os.CreateTemp("", "ub_password_hash")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	// Write a hash to it
	if _, err := tmpFile.WriteString("hashed_password_from_file"); err != nil {
		t.Fatalf("WriteString failed: %v", err)
	}
	tmpFile.Close()

	// Clear env vars first
	t.Cleanup(func() {
		os.Unsetenv("UB_USERNAME")
		os.Unsetenv("UB_PASSWORD_HASH")
		os.Unsetenv("UB_PASSWORD_HASH_PATH")
	})

	// Set username and password hash path (but leave DB empty)
	os.Setenv("UB_USERNAME", "bob")
	os.Setenv("UB_PASSWORD_HASH_PATH", tmpFile.Name())

	// With password hash file, setup is not required.
	if IsSetupRequired(ctx, db) {
		t.Errorf("expected IsSetupRequired=false with password hash file, got true")
	}
}

// TestIsSetupRequiredWithPartialDBCredentials verifies that setup is still required
// if only username or only password hash is set.
func TestIsSetupRequiredWithPartialDBCredentials(t *testing.T) {
	tests := []struct {
		name    string
		setupDB func(*sql.DB, context.Context) error
	}{
		{
			name: "only_username",
			setupDB: func(db *sql.DB, ctx context.Context) error {
				return notedb.SetSetting(ctx, db, KeyUsername, "alice")
			},
		},
		{
			name: "only_password_hash",
			setupDB: func(db *sql.DB, ctx context.Context) error {
				return notedb.SetSetting(ctx, db, KeyPasswordHash, "hashed_password")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()

			// Clear env vars
			t.Cleanup(func() {
				os.Unsetenv("UB_USERNAME")
				os.Unsetenv("UB_PASSWORD_HASH")
				os.Unsetenv("UB_PASSWORD_HASH_PATH")
			})

			if err := tt.setupDB(db, ctx); err != nil {
				t.Fatalf("setupDB failed: %v", err)
			}

			// With partial credentials, setup is still required.
			if !IsSetupRequired(ctx, db) {
				t.Errorf("expected IsSetupRequired=true with partial credentials, got false")
			}
		})
	}
}
