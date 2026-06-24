package appconfig

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"strconv"
	"strings"

	"github.com/sysop/ultrabridge/internal/notedb"
)

// runtimeKeys is the set of config keys that are runtime-configurable
// (not in envVarForKey, read at job time via closures).
var runtimeKeys = []string{
	KeySNInjectEnabled,
	KeySNOCRPrompt,
	KeyForestNoteOCRPrompt,
	KeyBooxOCRPrompt,
	KeyBooxTodoEnabled,
	KeyBooxTodoPrompt,
	KeyBooxImportPath,
	KeyBooxImportNotes,
	KeyBooxImportPDFs,
	KeyBooxImportOnyxPaths,
	KeyBooxExternalBaseURL,
}

// Config represents application configuration. Fields are grouped by concern.
type Config struct {
	// Auth
	Username     string
	PasswordHash string

	// OCR
	OCREnabled     bool
	OCRAPIURL      string
	OCRAPIKey      string
	OCRModel       string
	OCRConcurrency int
	OCRMaxFileMB   int
	OCRFormat      string // "anthropic" or "openai"

	// Embedding / RAG
	EmbedEnabled     bool
	OllamaURL        string
	OllamaEmbedModel string

	// Chat
	ChatEnabled bool
	ChatAPIURL  string
	ChatModel   string

	// Logging
	LogLevel         string
	LogFormat        string
	LogFile          string
	LogFileMaxMB     int
	LogFileMaxAge    int
	LogFileMaxBackup int
	LogSyslogAddr    string
	LogVerboseAPI    bool

	// CalDAV
	CalDAVCollectionName string
	DueTimeMode          string // "preserve" or "date_only"

	// Server
	WebEnabled bool

	// MCP
	MCPPort int // 0 hides the Settings "MCP Connection" helper card

	// SPC server (UB-as-SPC refactor)
	SPCMode       string // "client" (default, no listener) | "server"
	SPCListenAddr string
	SPCTLSCert    string
	SPCTLSKey     string

	// SPC auth (1b)
	SPCJWTSecret      string // defaults to Constant.SECRET
	SPCDeviceAccount  string // expected terminal-login account ("" = accept any)
	SPCDevicePassword string // raw account password; UB computes md5Hex(raw)

	// SPC file listing (Phase 2)
	SPCFileRoot   string // dedicated storage root the device browses ("" = disabled)
	SPCQuotaBytes int64  // fake total capacity reported to the device

	// SPC OSS signing (Phase 3 download)
	SPCOssSecret string // signs/verifies presigned URLs ("" = generate on first boot)

	// ForestNote device sync (roll-our-own SQLite sync)
	SyncEnabled    bool // gates the /sync/v1 route + syncstore migration (default off)
	SyncBatchLimit int  // max relay ops per /sync/v1 response
}

// SaveResult reports the outcome of a Save operation.
type SaveResult struct {
	ChangedKeys     []string
	RestartRequired bool
}

// loadConfigFromDB reads all config keys from the database, optionally applies env var overrides,
// applies defaults, and returns a typed Config struct.
// applyEnv controls whether environment variable overrides are applied.
func loadConfigFromDB(ctx context.Context, db *sql.DB, applyEnv bool) (*Config, error) {
	// Layer 1: Read all known keys from DB.
	dbVals := make(map[string]string)
	for key := range envVarForKey {
		val, err := notedb.GetSetting(ctx, db, key)
		if err != nil {
			return nil, err
		}
		dbVals[key] = val
	}

	// Also load the runtime-configurable keys (not in envVarForKey).
	for _, key := range runtimeKeys {
		val, err := notedb.GetSetting(ctx, db, key)
		if err != nil {
			return nil, err
		}
		dbVals[key] = val
	}

	// Layer 2: Optionally apply env var overrides.
	if applyEnv {
		applyEnvOverrides(dbVals)
	}

	// Layer 3: Apply defaults for missing values.
	for key, defaultVal := range defaultValues {
		if dbVals[key] == "" {
			dbVals[key] = defaultVal
		}
	}

	// Parse the map into a typed Config struct.
	cfg := &Config{
		Username:             dbVals[KeyUsername],
		PasswordHash:         dbVals[KeyPasswordHash],
		OCREnabled:           parseBool(dbVals[KeyOCREnabled]),
		OCRAPIURL:            dbVals[KeyOCRAPIURL],
		OCRAPIKey:            dbVals[KeyOCRAPIKey],
		OCRModel:             dbVals[KeyOCRModel],
		OCRConcurrency:       parseIntWithDefault(dbVals[KeyOCRConcurrency], 1),
		OCRMaxFileMB:         parseIntWithDefault(dbVals[KeyOCRMaxFileMB], 0),
		OCRFormat:            dbVals[KeyOCRFormat],
		EmbedEnabled:         parseBool(dbVals[KeyEmbedEnabled]),
		OllamaURL:            dbVals[KeyOllamaURL],
		OllamaEmbedModel:     dbVals[KeyOllamaEmbedModel],
		ChatEnabled:          parseBool(dbVals[KeyChatEnabled]),
		ChatAPIURL:           dbVals[KeyChatAPIURL],
		ChatModel:            dbVals[KeyChatModel],
		LogLevel:             dbVals[KeyLogLevel],
		LogFormat:            dbVals[KeyLogFormat],
		LogFile:              dbVals[KeyLogFile],
		LogFileMaxMB:         parseIntWithDefault(dbVals[KeyLogFileMaxMB], 50),
		LogFileMaxAge:        parseIntWithDefault(dbVals[KeyLogFileMaxAge], 30),
		LogFileMaxBackup:     parseIntWithDefault(dbVals[KeyLogFileMaxBackup], 5),
		LogSyslogAddr:        dbVals[KeyLogSyslogAddr],
		LogVerboseAPI:        parseBool(dbVals[KeyLogVerboseAPI]),
		CalDAVCollectionName: dbVals[KeyCalDAVCollectionName],
		DueTimeMode:          dbVals[KeyDueTimeMode],
		WebEnabled:           parseBool(dbVals[KeyWebEnabled]),
		MCPPort:              parseIntWithDefault(dbVals[KeyMCPPort], 8081),
		SPCMode:              dbVals[KeySPCMode],
		SPCListenAddr:        dbVals[KeySPCListenAddr],
		SPCTLSCert:           dbVals[KeySPCTLSCert],
		SPCTLSKey:            dbVals[KeySPCTLSKey],
		SPCJWTSecret:         dbVals[KeySPCJWTSecret],
		SPCDeviceAccount:     dbVals[KeySPCDeviceAccount],
		SPCDevicePassword:    dbVals[KeySPCDevicePassword],
		SPCFileRoot:          dbVals[KeySPCFileRoot],
		SPCQuotaBytes:        parseInt64WithDefault(dbVals[KeySPCQuotaBytes], 1<<40),
		SPCOssSecret:         dbVals[KeySPCOssSecret],
		SyncEnabled:          parseBool(dbVals[KeySyncEnabled]),
		SyncBatchLimit:       parseIntWithDefault(dbVals[KeySyncBatchLimit], 500),
	}

	return cfg, nil
}

// Load reads all config keys from the database, applies env var overrides,
// and returns a typed Config struct.
func Load(ctx context.Context, db *sql.DB) (*Config, error) {
	return loadConfigFromDB(ctx, db, true)
}

// Save writes changed keys to the database and reports which keys changed
// and whether any restart-required keys were modified.
func Save(ctx context.Context, db *sql.DB, cfg *Config) (*SaveResult, error) {
	// Load the current config from DB (without env overlay).
	current, err := loadDBOnly(ctx, db)
	if err != nil {
		return nil, err
	}

	// Convert both to maps for comparison.
	oldMap := configToMap(current)
	newMap := configToMap(cfg)

	// Find changed keys.
	changedKeys := []string{}
	restartRequiredChanged := false

	for key, newVal := range newMap {
		oldVal, exists := oldMap[key]
		if !exists || oldVal != newVal {
			changedKeys = append(changedKeys, key)
			if restartRequired[key] {
				restartRequiredChanged = true
			}
		}
	}

	// Write changed keys to DB.
	for _, key := range changedKeys {
		if err := notedb.SetSetting(ctx, db, key, newMap[key]); err != nil {
			return nil, err
		}
	}

	return &SaveResult{
		ChangedKeys:     changedKeys,
		RestartRequired: restartRequiredChanged,
	}, nil
}

// IsSetupRequired returns true when no auth credentials exist in either
// the settings DB or environment variables. This indicates first-boot setup
// is needed before the application can enforce authentication.
func IsSetupRequired(ctx context.Context, db *sql.DB) bool {
	// Check DB first
	username, _ := notedb.GetSetting(ctx, db, KeyUsername)
	hash, _ := notedb.GetSetting(ctx, db, KeyPasswordHash)
	if username != "" && hash != "" {
		return false
	}

	// Check env vars (backward compatibility for existing installs)
	if os.Getenv("UB_USERNAME") != "" && os.Getenv("UB_PASSWORD_HASH") != "" {
		return false
	}

	// Also check password hash file
	if os.Getenv("UB_USERNAME") != "" {
		hashPath := os.Getenv("UB_PASSWORD_HASH_PATH")
		if hashPath == "" {
			hashPath = "/run/secrets/ub_password_hash"
		}
		if data, err := os.ReadFile(hashPath); err == nil && strings.TrimSpace(string(data)) != "" {
			return false
		}
	}

	return true
}

// loadDBOnly loads config from DB without env var overlay.
// Used by Save to detect changes.
func loadDBOnly(ctx context.Context, db *sql.DB) (*Config, error) {
	return loadConfigFromDB(ctx, db, false)
}

// configToMap converts a Config struct to a map for comparison.
func configToMap(cfg *Config) map[string]string {
	m := map[string]string{
		KeyUsername:             cfg.Username,
		KeyPasswordHash:         cfg.PasswordHash,
		KeyOCREnabled:           boolToString(cfg.OCREnabled),
		KeyOCRAPIURL:            cfg.OCRAPIURL,
		KeyOCRAPIKey:            cfg.OCRAPIKey,
		KeyOCRModel:             cfg.OCRModel,
		KeyOCRConcurrency:       strconv.Itoa(cfg.OCRConcurrency),
		KeyOCRMaxFileMB:         strconv.Itoa(cfg.OCRMaxFileMB),
		KeyOCRFormat:            cfg.OCRFormat,
		KeyEmbedEnabled:         boolToString(cfg.EmbedEnabled),
		KeyOllamaURL:            cfg.OllamaURL,
		KeyOllamaEmbedModel:     cfg.OllamaEmbedModel,
		KeyChatEnabled:          boolToString(cfg.ChatEnabled),
		KeyChatAPIURL:           cfg.ChatAPIURL,
		KeyChatModel:            cfg.ChatModel,
		KeyLogLevel:             cfg.LogLevel,
		KeyLogFormat:            cfg.LogFormat,
		KeyLogFile:              cfg.LogFile,
		KeyLogFileMaxMB:         strconv.Itoa(cfg.LogFileMaxMB),
		KeyLogFileMaxAge:        strconv.Itoa(cfg.LogFileMaxAge),
		KeyLogFileMaxBackup:     strconv.Itoa(cfg.LogFileMaxBackup),
		KeyLogSyslogAddr:        cfg.LogSyslogAddr,
		KeyLogVerboseAPI:        boolToString(cfg.LogVerboseAPI),
		KeyCalDAVCollectionName: cfg.CalDAVCollectionName,
		KeyDueTimeMode:          cfg.DueTimeMode,
		KeyWebEnabled:           boolToString(cfg.WebEnabled),
		KeyMCPPort:              strconv.Itoa(cfg.MCPPort),
		KeySPCMode:              cfg.SPCMode,
		KeySPCListenAddr:        cfg.SPCListenAddr,
		KeySPCTLSCert:           cfg.SPCTLSCert,
		KeySPCTLSKey:            cfg.SPCTLSKey,
		KeySPCJWTSecret:         cfg.SPCJWTSecret,
		KeySPCDeviceAccount:     cfg.SPCDeviceAccount,
		KeySPCDevicePassword:    cfg.SPCDevicePassword,
		KeySPCFileRoot:          cfg.SPCFileRoot,
		KeySPCQuotaBytes:        strconv.FormatInt(cfg.SPCQuotaBytes, 10),
		KeySPCOssSecret:         cfg.SPCOssSecret,
		KeySyncEnabled:          boolToString(cfg.SyncEnabled),
		KeySyncBatchLimit:       strconv.Itoa(cfg.SyncBatchLimit),
	}
	return m
}

// Parsing helpers.

func parseBool(v string) bool {
	return strings.EqualFold(v, "true") || v == "1"
}

func boolToString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func parseIntWithDefault(v string, def int) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func parseInt64(v string) int64 {
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func parseInt64WithDefault(v string, def int64) int64 {
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// EnsureSPCOssSecret returns the persisted SPC OSS signing secret, generating
// and storing a fresh 32-byte (64 hex char) random value on first boot if none
// is set. Used by the UB-as-SPC server mode (Phase 3 download) to sign the
// presigned URLs it issues to itself; the device never computes a signature, so
// any stable per-install secret works. An already-set value (env- or
// DB-configured) is returned untouched.
func EnsureSPCOssSecret(ctx context.Context, db *sql.DB) (string, error) {
	existing, err := notedb.GetSetting(ctx, db, KeySPCOssSecret)
	if err != nil {
		return "", err
	}
	if existing != "" {
		return existing, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(b)
	if err := notedb.SetSetting(ctx, db, KeySPCOssSecret, secret); err != nil {
		return "", err
	}
	return secret, nil
}

// EnsureTaskAttachSecret returns the persisted CalDAV task-ATTACH signing
// secret, generating and storing a fresh 32-byte (64 hex char) random value on
// first boot if none is set. Used to sign the public (no-auth) attachment
// download + page-render URLs UB embeds in VTODO ATTACH properties; the secret
// is stable (the signed URLs never expire), so an already-set value (env- or
// DB-configured) is returned untouched.
func EnsureTaskAttachSecret(ctx context.Context, db *sql.DB) (string, error) {
	// An explicit env override wins and is NOT persisted, letting an operator
	// pin or rotate the secret without touching the DB. notedb.GetSetting reads
	// the raw table and does not apply env overrides, so we must check here.
	if env := os.Getenv(envVarForKey[KeyTaskAttachSecret]); env != "" {
		return env, nil
	}
	existing, err := notedb.GetSetting(ctx, db, KeyTaskAttachSecret)
	if err != nil {
		return "", err
	}
	if existing != "" {
		return existing, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(b)
	if err := notedb.SetSetting(ctx, db, KeyTaskAttachSecret, secret); err != nil {
		return "", err
	}
	return secret, nil
}
