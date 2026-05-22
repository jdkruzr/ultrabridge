package appconfig

// Setting key constants. Each maps to a row in the settings KV table.
const (
	// Auth
	KeyUsername     = "auth_username"
	KeyPasswordHash = "auth_password_hash"

	// OCR
	KeyOCREnabled     = "ocr_enabled"
	KeyOCRAPIURL      = "ocr_api_url"
	KeyOCRAPIKey      = "ocr_api_key"
	KeyOCRModel       = "ocr_model"
	KeyOCRConcurrency = "ocr_concurrency"
	KeyOCRMaxFileMB   = "ocr_max_file_mb"
	KeyOCRFormat      = "ocr_format"

	// Embedding / RAG
	KeyEmbedEnabled     = "embed_enabled"
	KeyOllamaURL        = "ollama_url"
	KeyOllamaEmbedModel = "ollama_embed_model"

	// Chat
	KeyChatEnabled = "chat_enabled"
	KeyChatAPIURL  = "chat_api_url"
	KeyChatModel   = "chat_model"

	// Supernote device sync
	KeySNSyncEnabled  = "sn_sync_enabled"
	KeySNSyncInterval = "sn_sync_interval"
	KeySNAPIURL       = "sn_api_url"
	KeySNAccount      = "sn_account"
	KeySNPassword     = "sn_password"

	// Logging
	KeyLogLevel         = "log_level"
	KeyLogFormat        = "log_format"
	KeyLogFile          = "log_file"
	KeyLogFileMaxMB     = "log_file_max_mb"
	KeyLogFileMaxAge    = "log_file_max_age_days"
	KeyLogFileMaxBackup = "log_file_max_backup"
	KeyLogSyslogAddr    = "log_syslog_addr"
	KeyLogVerboseAPI    = "log_verbose_api"

	// CalDAV

	KeyCalDAVCollectionName = "caldav_collection_name"
	KeyDueTimeMode          = "due_time_mode"

	// Server
	KeyWebEnabled  = "web_enabled"
	KeySocketIOURL = "socketio_url"

	// MCP
	// KeyMCPPort is the host-exposed port of the sibling ub-mcp container.
	// Used by the Settings UI to render copy-pasteable client configs
	// (HTTP SSE URL, stdio docker exec command). 0 hides the helper card.
	KeyMCPPort = "mcp_port"

	// MariaDB / SPC connection
	KeyDBHost    = "db_host"
	KeyDBPort    = "db_port"
	KeyDBEnvPath = "dbenv_path"
	KeyUserID    = "user_id"

	// SPC server (UB-as-SPC refactor). Mode defaults to "client" = no listener,
	// so UB behaves exactly as today unless explicitly switched to "server".
	KeySPCMode       = "spc_mode"
	KeySPCListenAddr = "spc_listen_addr"
	KeySPCTLSCert    = "spc_tls_cert"
	KeySPCTLSKey     = "spc_tls_key"

	// SPC auth (1b). KeySPCJWTSecret defaults to Constant.SECRET so UB-minted
	// tokens verify against the same secret the device expects. DeviceAccount/
	// DevicePassword hold the raw account+password UB validates terminal logins
	// against (UB computes md5Hex(raw) internally; see docs/spc-protocol.md §2.1).
	KeySPCJWTSecret      = "spc_jwt_secret"
	KeySPCDeviceAccount  = "spc_device_account"
	KeySPCDevicePassword = "spc_device_password"

	// Runtime-configurable (existing keys, read at job time via closures — NOT loaded into Config struct)
	// These are included here for completeness but are accessed via notedb.GetSetting directly.
	KeySNInjectEnabled     = "sn_inject_enabled"
	KeySNOCRPrompt         = "sn_ocr_prompt"
	KeyBooxOCRPrompt       = "boox_ocr_prompt"
	KeyBooxTodoEnabled     = "boox_todo_enabled"
	KeyBooxTodoPrompt      = "boox_todo_prompt"
	KeyBooxImportPath      = "boox_import_path"
	KeyBooxImportNotes     = "boox_import_notes"
	KeyBooxImportPDFs      = "boox_import_pdfs"
	KeyBooxImportOnyxPaths = "boox_import_onyx_paths"
	// KeyBooxExternalBaseURL is the externally-reachable base URL of this
	// UltraBridge deployment (e.g. https://ub.example.com). Prepended to
	// red-ink-TODO detail links so CalDAV clients render a full clickable
	// URL. Empty string falls back to a relative path (web UI still works,
	// external clients see path-as-text).
	KeyBooxExternalBaseURL = "boox_external_base_url"
)

// envVarForKey maps each setting key to its UB_ env var name.
// Only keys that have a corresponding env var are listed.
// Note: Per-source env vars (UB_NOTES_PATH, UB_BACKUP_PATH, UB_BOOX_ENABLED, UB_BOOX_NOTES_PATH) are no longer recognized.
// Configure sources via the Settings UI instead.
var envVarForKey = map[string]string{
	KeyUsername:             "UB_USERNAME",
	KeyPasswordHash:         "UB_PASSWORD_HASH",
	KeyOCREnabled:           "UB_OCR_ENABLED",
	KeyOCRAPIURL:            "UB_OCR_API_URL",
	KeyOCRAPIKey:            "UB_OCR_API_KEY",
	KeyOCRModel:             "UB_OCR_MODEL",
	KeyOCRConcurrency:       "UB_OCR_CONCURRENCY",
	KeyOCRMaxFileMB:         "UB_OCR_MAX_FILE_MB",
	KeyOCRFormat:            "UB_OCR_FORMAT",
	KeyEmbedEnabled:         "UB_EMBED_ENABLED",
	KeyOllamaURL:            "UB_OLLAMA_URL",
	KeyOllamaEmbedModel:     "UB_OLLAMA_EMBED_MODEL",
	KeyChatEnabled:          "UB_CHAT_ENABLED",
	KeyChatAPIURL:           "UB_CHAT_API_URL",
	KeyChatModel:            "UB_CHAT_MODEL",
	KeySNSyncEnabled:        "UB_SN_SYNC_ENABLED",
	KeySNSyncInterval:       "UB_SN_SYNC_INTERVAL",
	KeySNAPIURL:             "UB_SN_API_URL",
	KeySNAccount:            "UB_SN_ACCOUNT",
	KeySNPassword:           "UB_SN_PASSWORD",
	KeyLogLevel:             "UB_LOG_LEVEL",
	KeyLogFormat:            "UB_LOG_FORMAT",
	KeyLogFile:              "UB_LOG_FILE",
	KeyLogFileMaxMB:         "UB_LOG_FILE_MAX_MB",
	KeyLogFileMaxAge:        "UB_LOG_FILE_MAX_AGE_DAYS",
	KeyLogFileMaxBackup:     "UB_LOG_FILE_MAX_BACKUPS",
	KeyLogSyslogAddr:        "UB_LOG_SYSLOG_ADDR",
	KeyLogVerboseAPI:        "UB_LOG_VERBOSE_API",
	KeyCalDAVCollectionName: "UB_CALDAV_COLLECTION_NAME",
	KeyDueTimeMode:          "UB_DUE_TIME_MODE",
	KeyWebEnabled:           "UB_WEB_ENABLED",
	KeySocketIOURL:          "UB_SOCKETIO_URL",
	KeyDBHost:               "UB_DB_HOST",
	KeyDBPort:               "UB_DB_PORT",
	KeyDBEnvPath:            "UB_SUPERNOTE_DBENV_PATH",
	KeyUserID:               "UB_USER_ID",
	KeyMCPPort:              "UB_MCP_PORT",
	KeySPCMode:              "UB_SPC_MODE",
	KeySPCListenAddr:        "UB_SPC_LISTEN_ADDR",
	KeySPCTLSCert:           "UB_SPC_TLS_CERT",
	KeySPCTLSKey:            "UB_SPC_TLS_KEY",
	KeySPCJWTSecret:         "UB_SPC_JWT_SECRET",
	KeySPCDeviceAccount:     "UB_SPC_DEVICE_ACCOUNT",
	KeySPCDevicePassword:    "UB_SPC_DEVICE_PASSWORD",
}

// defaultValues provides the default for each setting key when neither DB nor env var is set.
// Keys not in this map default to empty string.
var defaultValues = map[string]string{
	KeyOCRFormat:            "anthropic",
	KeyOCRConcurrency:       "1",
	KeyOCRMaxFileMB:         "0",
	KeyOllamaURL:            "http://localhost:11434",
	KeyOllamaEmbedModel:     "nomic-embed-text:v1.5",
	KeyChatAPIURL:           "http://localhost:8000",
	KeyChatModel:            "Qwen/Qwen3-8B",
	KeySNSyncInterval:       "300",
	KeySNAPIURL:             "http://supernote-service:8080",
	KeyLogLevel:             "info",
	KeyLogFormat:            "json",
	KeyLogFileMaxMB:         "50",
	KeyLogFileMaxAge:        "30",
	KeyLogFileMaxBackup:     "5",
	KeyCalDAVCollectionName: "Tasks",
	KeyDueTimeMode:          "preserve",
	KeyWebEnabled:           "true",
	KeySocketIOURL:          "ws://supernote-service:8080/socket.io/",
	KeyDBHost:               "mariadb",
	KeyDBPort:               "3306",
	KeyDBEnvPath:            "/run/secrets/dbenv",
	KeyMCPPort:              "8081",
	KeySPCMode:              "client",
	KeySPCListenAddr:        ":8089",
	// Constant.SECRET (com/ratta/constants/Constant.java:46) — the SPC JWT
	// signing secret (NOT the 32-char JWT_SECRET). Load-bearing for device auth.
	KeySPCJWTSecret: "suernotea1hK52bgkf9N7PQ5E3KDqKeCIT719a6kh04eSTSBLv7e9tPtw2L8S6pEDMy7lAIv2CYjg5Ncy7ep5zDS7hH9CDAZnLieo66g7F8iZmClK9a1xEEPewXLhkM4KTKI7pz2Lkl7Cds4MpClNvNCVHPbfWKNyiFSGUztbnmqDWgNAinPBNamwDUQpT8RwCO1wc9vYTTQsmXm8ByioHC3QkRMZtHZnIWWCkIWECPzSJGOowNliAavzVCMsKadYnsH322n",
}

// restartRequired is the set of keys whose changes require a restart to take effect.
// Changes to these keys are detected by Save() and reported so the UI can show a banner.
var restartRequired = map[string]bool{
	KeyUsername:             true,
	KeyPasswordHash:         true,
	KeyOCREnabled:           true,
	KeyOCRAPIURL:            true,
	KeyOCRAPIKey:            true,
	KeyOCRModel:             true,
	KeyOCRConcurrency:       true,
	KeyOCRMaxFileMB:         true,
	KeyOCRFormat:            true,
	KeyEmbedEnabled:         true,
	KeyOllamaURL:            true,
	KeyOllamaEmbedModel:     true,
	KeyChatEnabled:          true,
	KeyChatAPIURL:           true,
	KeyChatModel:            true,
	KeySNSyncEnabled:        true,
	KeySNSyncInterval:       true,
	KeySNAPIURL:             true,
	KeySNAccount:            true,
	KeySNPassword:           true,
	KeyLogLevel:             true,
	KeyLogFormat:            true,
	KeyLogFile:              true,
	KeyLogSyslogAddr:        true,
	KeyWebEnabled:           true,
	KeySocketIOURL:          true,
	KeyDBHost:               true,
	KeyDBPort:               true,
	KeyDBEnvPath:            true,
	KeyUserID:               true,
	KeyCalDAVCollectionName: true,
	KeySPCMode:              true,
	KeySPCListenAddr:        true,
	KeySPCTLSCert:           true,
	KeySPCTLSKey:            true,
	KeySPCJWTSecret:         true,
	KeySPCDeviceAccount:     true,
	KeySPCDevicePassword:    true,
}
