package web

import (
	"encoding/json"
	"net/http"

	"golang.org/x/crypto/bcrypt"

	"github.com/sysop/ultrabridge/internal/appconfig"
)

// RedactedConfig is like appconfig.Config but with secrets redacted for API responses.
type RedactedConfig struct {
	Username             string `json:"username"`
	PasswordHash         string `json:"password_hash"` // "[set]" or "[not set]"
	OCREnabled           bool   `json:"ocr_enabled"`
	OCRAPIURL            string `json:"ocr_api_url"`
	OCRAPIKey            string `json:"ocr_api_key"` // "[set]" or "[not set]"
	OCRModel             string `json:"ocr_model"`
	OCRConcurrency       int    `json:"ocr_concurrency"`
	OCRMaxFileMB         int    `json:"ocr_max_file_mb"`
	OCRFormat            string `json:"ocr_format"`
	EmbedEnabled         bool   `json:"embed_enabled"`
	OllamaURL            string `json:"ollama_url"`
	OllamaEmbedModel     string `json:"ollama_embed_model"`
	ChatEnabled          bool   `json:"chat_enabled"`
	ChatAPIURL           string `json:"chat_api_url"`
	ChatModel            string `json:"chat_model"`
	LogLevel             string `json:"log_level"`
	LogFormat            string `json:"log_format"`
	LogFile              string `json:"log_file"`
	LogFileMaxMB         int    `json:"log_file_max_mb"`
	LogFileMaxAge        int    `json:"log_file_max_age"`
	LogFileMaxBackup     int    `json:"log_file_max_backup"`
	LogSyslogAddr        string `json:"log_syslog_addr"`
	CalDAVCollectionName string `json:"caldav_collection_name"`
	DueTimeMode          string `json:"due_time_mode"`
	WebEnabled           bool   `json:"web_enabled"`
}

// redactConfig returns a copy of cfg with secrets replaced with "[set]" or "[not set]".
func redactConfig(cfg *appconfig.Config) *RedactedConfig {
	if cfg == nil {
		return nil
	}
	return &RedactedConfig{
		Username:             cfg.Username,
		PasswordHash:         redactSecret(cfg.PasswordHash),
		OCREnabled:           cfg.OCREnabled,
		OCRAPIURL:            cfg.OCRAPIURL,
		OCRAPIKey:            redactSecret(cfg.OCRAPIKey),
		OCRModel:             cfg.OCRModel,
		OCRConcurrency:       cfg.OCRConcurrency,
		OCRMaxFileMB:         cfg.OCRMaxFileMB,
		OCRFormat:            cfg.OCRFormat,
		EmbedEnabled:         cfg.EmbedEnabled,
		OllamaURL:            cfg.OllamaURL,
		OllamaEmbedModel:     cfg.OllamaEmbedModel,
		ChatEnabled:          cfg.ChatEnabled,
		ChatAPIURL:           cfg.ChatAPIURL,
		ChatModel:            cfg.ChatModel,
		LogLevel:             cfg.LogLevel,
		LogFormat:            cfg.LogFormat,
		LogFile:              cfg.LogFile,
		LogFileMaxMB:         cfg.LogFileMaxMB,
		LogFileMaxAge:        cfg.LogFileMaxAge,
		LogFileMaxBackup:     cfg.LogFileMaxBackup,
		LogSyslogAddr:        cfg.LogSyslogAddr,
		CalDAVCollectionName: cfg.CalDAVCollectionName,
		DueTimeMode:          cfg.DueTimeMode,
		WebEnabled:           cfg.WebEnabled,
	}
}

// redactSecret returns "[set]" if secret is non-empty, "[not set]" otherwise.
func redactSecret(s string) string {
	if s != "" {
		return "[set]"
	}
	return "[not set]"
}

// IncomingConfig is the request body format for PUT /api/config.
// It mirrors appconfig.Config but adds a "password" field for plaintext input.
type IncomingConfig struct {
	Username             string `json:"username"`
	Password             string `json:"password"` // plaintext; if non-empty, hashed server-side
	OCREnabled           bool   `json:"ocr_enabled"`
	OCRAPIURL            string `json:"ocr_api_url"`
	OCRAPIKey            string `json:"ocr_api_key"`
	OCRModel             string `json:"ocr_model"`
	OCRConcurrency       int    `json:"ocr_concurrency"`
	OCRMaxFileMB         int    `json:"ocr_max_file_mb"`
	OCRFormat            string `json:"ocr_format"`
	EmbedEnabled         bool   `json:"embed_enabled"`
	OllamaURL            string `json:"ollama_url"`
	OllamaEmbedModel     string `json:"ollama_embed_model"`
	ChatEnabled          bool   `json:"chat_enabled"`
	ChatAPIURL           string `json:"chat_api_url"`
	ChatModel            string `json:"chat_model"`
	LogLevel             string `json:"log_level"`
	LogFormat            string `json:"log_format"`
	LogFile              string `json:"log_file"`
	LogFileMaxMB         int    `json:"log_file_max_mb"`
	LogFileMaxAge        int    `json:"log_file_max_age"`
	LogFileMaxBackup     int    `json:"log_file_max_backup"`
	LogSyslogAddr        string `json:"log_syslog_addr"`
	CalDAVCollectionName string `json:"caldav_collection_name"`
	DueTimeMode          string `json:"due_time_mode"`
	WebEnabled           bool   `json:"web_enabled"`
}

// handleGetConfig handles GET /api/config — returns current config with secrets redacted.
func (h *Handler) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cObj, err := h.config.GetConfig(r.Context())
	if err != nil {
		h.logger.Error("load config", "error", err)
		apiError(w, http.StatusInternalServerError, "failed to load config")
		return
	}
	cfg := cObj.(*appconfig.Config)

	redacted := redactConfig(cfg)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(redacted)
}

// handlePutConfig handles PUT /api/config — update config with password hashing.
func (h *Handler) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Load current config as the base
	cObj, err := h.config.GetConfig(ctx)
	if err != nil {
		h.logger.Error("load config for merge", "error", err)
		apiError(w, http.StatusInternalServerError, "failed to load current config")
		return
	}
	cfg := cObj.(*appconfig.Config)

	// Decode incoming into a map for merge detection
	var rawMap map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&rawMap); err != nil {
		apiError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	var incoming IncomingConfig
	rawBytes, _ := json.Marshal(rawMap)
	json.Unmarshal(rawBytes, &incoming)

	// Overlay fields
	if _, ok := rawMap["username"]; ok {
		cfg.Username = incoming.Username
	}
	if _, ok := rawMap["ocr_enabled"]; ok {
		cfg.OCREnabled = incoming.OCREnabled
	}
	if _, ok := rawMap["ocr_api_url"]; ok {
		cfg.OCRAPIURL = incoming.OCRAPIURL
	}
	if _, ok := rawMap["ocr_api_key"]; ok {
		cfg.OCRAPIKey = incoming.OCRAPIKey
	}
	if _, ok := rawMap["ocr_model"]; ok {
		cfg.OCRModel = incoming.OCRModel
	}
	if _, ok := rawMap["ocr_concurrency"]; ok {
		cfg.OCRConcurrency = incoming.OCRConcurrency
	}
	if _, ok := rawMap["ocr_max_file_mb"]; ok {
		cfg.OCRMaxFileMB = incoming.OCRMaxFileMB
	}
	if _, ok := rawMap["ocr_format"]; ok {
		cfg.OCRFormat = incoming.OCRFormat
	}
	if _, ok := rawMap["embed_enabled"]; ok {
		cfg.EmbedEnabled = incoming.EmbedEnabled
	}
	if _, ok := rawMap["ollama_url"]; ok {
		cfg.OllamaURL = incoming.OllamaURL
	}
	if _, ok := rawMap["ollama_embed_model"]; ok {
		cfg.OllamaEmbedModel = incoming.OllamaEmbedModel
	}
	if _, ok := rawMap["chat_enabled"]; ok {
		cfg.ChatEnabled = incoming.ChatEnabled
	}
	if _, ok := rawMap["chat_api_url"]; ok {
		cfg.ChatAPIURL = incoming.ChatAPIURL
	}
	if _, ok := rawMap["chat_model"]; ok {
		cfg.ChatModel = incoming.ChatModel
	}
	if _, ok := rawMap["log_level"]; ok {
		cfg.LogLevel = incoming.LogLevel
	}
	if _, ok := rawMap["log_format"]; ok {
		cfg.LogFormat = incoming.LogFormat
	}
	if _, ok := rawMap["log_file"]; ok {
		cfg.LogFile = incoming.LogFile
	}
	if _, ok := rawMap["log_file_max_mb"]; ok {
		cfg.LogFileMaxMB = incoming.LogFileMaxMB
	}
	if _, ok := rawMap["log_file_max_age"]; ok {
		cfg.LogFileMaxAge = incoming.LogFileMaxAge
	}
	if _, ok := rawMap["log_file_max_backup"]; ok {
		cfg.LogFileMaxBackup = incoming.LogFileMaxBackup
	}
	if _, ok := rawMap["log_syslog_addr"]; ok {
		cfg.LogSyslogAddr = incoming.LogSyslogAddr
	}
	if _, ok := rawMap["caldav_collection_name"]; ok {
		cfg.CalDAVCollectionName = incoming.CalDAVCollectionName
	}
	if _, ok := rawMap["due_time_mode"]; ok {
		cfg.DueTimeMode = incoming.DueTimeMode
	}
	if _, ok := rawMap["web_enabled"]; ok {
		cfg.WebEnabled = incoming.WebEnabled
	}

	if pw, ok := rawMap["password"]; ok {
		var password string
		json.Unmarshal(pw, &password)
		if password != "" {
			hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
			if err != nil {
				h.logger.Error("bcrypt hash failed", "error", err)
				apiError(w, http.StatusInternalServerError, "failed to hash password")
				return
			}
			cfg.PasswordHash = string(hash)
		}
	}

	if err := h.config.UpdateConfig(ctx, cfg); err != nil {
		h.logger.Error("save config", "error", err)
		apiError(w, http.StatusInternalServerError, "failed to save config")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{
		"restart_required": h.config.IsRestartRequired(),
	})
}
