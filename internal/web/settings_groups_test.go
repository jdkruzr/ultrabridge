package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/appconfig"
	"github.com/sysop/ultrabridge/internal/logging"
	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/service"
)

// newSettingsGroupsHandler builds a Handler over a real in-memory notedb so
// config saves persist across requests (appconfig.Load/Save round-trips).
func newSettingsGroupsHandler(t *testing.T) (*Handler, service.ConfigService) {
	t.Helper()
	ctx := context.Background()
	testDB, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("notedb open: %v", err)
	}
	t.Cleanup(func() { testDB.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfgService := service.NewConfigService(testDB, &appconfig.Config{})
	h := NewHandler(nil, &mockNoteService{}, nil, cfgService, testDB, "", "", logger, logging.NewLogBroadcaster())
	return h, cfgService
}

func getGroupFragment(t *testing.T, h http.Handler, group string) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/settings/"+group, nil)
	req.Header.Set("HX-Request", "true") // fragment only — keeps layout JS out of the assertions
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /settings/%s = %d, want 200", group, w.Code)
	}
	return w.Body.String()
}

func postSettingsSave(t *testing.T, h http.Handler, form url.Values) {
	t.Helper()
	req := httptest.NewRequest("POST", "/settings/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST /settings/save (%v) = %d, want 303", form.Get("section"), w.Code)
	}
}

func putConfigJSON(t *testing.T, h http.Handler, body string) {
	t.Helper()
	req := httptest.NewRequest("PUT", "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT /api/config %s = %d, want 200 (%s)", body, w.Code, w.Body.String())
	}
}

func loadConfig(t *testing.T, cfgService service.ConfigService) *appconfig.Config {
	t.Helper()
	cObj, err := cfgService.GetConfig(context.Background())
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	return cObj.(*appconfig.Config)
}

// TestSettingsGroupMembership asserts each group route renders only its own
// cards. sync-model-and-settings-ia.AC4.2 (content) + AC5.1.
func TestSettingsGroupMembership(t *testing.T) {
	h, _ := newSettingsGroupsHandler(t)

	groups := map[string]struct {
		want    []string
		wantNot []string
	}{
		"system": {
			want:    []string{"<h2>Authentication</h2>", "<h2>Debugging</h2>", "config-username", "log-verbose-api"},
			wantNot: []string{"OCR Configuration", "RAG Search", "<h2>Sources</h2>", "<h2>MCP</h2>", "CalDAV"},
		},
		"ai": {
			want:    []string{"OCR Configuration", "Source OCR Prompts", "RAG Search", "AI Chat", "config-ocr-model", "embed-enabled", "forestnote-ocr-prompt"},
			wantNot: []string{"<h2>Authentication</h2>", "CalDAV", "<h2>MCP</h2>", "<h2>Sources</h2>", "Debugging"},
		},
		"integrations": {
			want:    []string{"<h2>MCP</h2>", "<h2>CalDAV</h2>", "caldav-collection-name"},
			wantNot: []string{"OCR Configuration", "<h2>Authentication</h2>", "RAG Search", "<h2>Sources</h2>"},
		},
		"devices": {
			want:    []string{"<h2>Sources</h2>", "<h2>Supernote</h2>", "<h2>ForestNote</h2>", "<h2>reMarkable</h2>", "<h2>Boox</h2>"},
			wantNot: []string{"OCR Configuration", "<h2>MCP</h2>", "RAG Search", "<h2>Authentication</h2>", "forestnote-ocr-prompt", "boox-ocr-prompt", "sn-ocr-prompt"},
		},
	}

	for group, expect := range groups {
		body := getGroupFragment(t, h, group)
		for _, want := range expect.want {
			if !strings.Contains(body, want) {
				t.Errorf("/settings/%s missing %q", group, want)
			}
		}
		for _, not := range expect.wantNot {
			if strings.Contains(body, not) {
				t.Errorf("/settings/%s wrongly contains %q", group, not)
			}
		}
		// AC5.1: the General grab-bag cards are dissolved everywhere.
		if strings.Contains(body, "<h2>General") {
			t.Errorf("/settings/%s still renders a General card", group)
		}
	}
}

// TestSettingsMCPConsolidation asserts MCP is one card with Server /
// Connection / Tokens subsections. sync-model-and-settings-ia.AC5.2.
func TestSettingsMCPConsolidation(t *testing.T) {
	h, _ := newSettingsGroupsHandler(t)

	// MCPPort > 0 makes the Connection subsection render.
	putConfigJSON(t, h, `{"mcp_port": 8081}`)

	body := getGroupFragment(t, h, "integrations")
	if n := strings.Count(body, "<h2>MCP</h2>"); n != 1 {
		t.Errorf("MCP <h2> count = %d, want exactly 1", n)
	}
	for _, sub := range []string{">Server</h3>", ">Connection</h3>", ">Tokens</h3>"} {
		if !strings.Contains(body, sub) {
			t.Errorf("MCP card missing subsection marker %q", sub)
		}
	}
	if !strings.Contains(body, "config-mcp-port") {
		t.Error("MCP card missing the Server port field")
	}
}

// TestSettingsSaveRoundTrips asserts every split save path persists its own
// fields and does not blank its former General-card siblings.
// sync-model-and-settings-ia.AC5.3.
func TestSettingsSaveRoundTrips(t *testing.T) {
	h, cfgService := newSettingsGroupsHandler(t)

	// section=system and section=integrations persist their fields.
	postSettingsSave(t, h, url.Values{"section": {"system"}, "log_verbose_api": {"true"}})
	postSettingsSave(t, h, url.Values{"section": {"integrations"}, "caldav_collection_name": {"My Tasks"}})
	cfg := loadConfig(t, cfgService)
	if !cfg.LogVerboseAPI {
		t.Error("section=system save did not persist LogVerboseAPI")
	}
	if cfg.CalDAVCollectionName != "My Tasks" {
		t.Errorf("section=integrations save: CalDAVCollectionName = %q, want My Tasks", cfg.CalDAVCollectionName)
	}

	// section=ai persists its fields and leaves the other sections' alone.
	postSettingsSave(t, h, url.Values{
		"section":            {"ai"},
		"embed_enabled":      {"true"},
		"ollama_url":         {"http://ollama:11434"},
		"ollama_embed_model": {"nomic-embed-text:v1.5"},
		"chat_enabled":       {"true"},
		"chat_api_url":       {"http://vllm:8000"},
		"chat_model":         {"qwen3:8b"},
	})
	cfg = loadConfig(t, cfgService)
	if !cfg.EmbedEnabled || cfg.OllamaURL != "http://ollama:11434" || cfg.OllamaEmbedModel != "nomic-embed-text:v1.5" {
		t.Errorf("section=ai save did not persist RAG fields: %+v", cfg)
	}
	if !cfg.ChatEnabled || cfg.ChatAPIURL != "http://vllm:8000" || cfg.ChatModel != "qwen3:8b" {
		t.Errorf("section=ai save did not persist chat fields: %+v", cfg)
	}
	if !cfg.LogVerboseAPI || cfg.CalDAVCollectionName != "My Tasks" {
		t.Error("section=ai save blanked sibling sections (LogVerboseAPI / CalDAVCollectionName)")
	}

	postSettingsSave(t, h, url.Values{
		"section":               {"ocr-prompts"},
		"sn_ocr_prompt":         {"supernote prompt"},
		"forestnote_ocr_prompt": {"forestnote prompt"},
		"boox_ocr_prompt":       {"boox prompt"},
	})
	for key, want := range map[string]string{
		appconfig.KeySNOCRPrompt:         "supernote prompt",
		appconfig.KeyForestNoteOCRPrompt: "forestnote prompt",
		appconfig.KeyBooxOCRPrompt:       "boox prompt",
	} {
		got, _ := notedb.GetSetting(context.Background(), h.noteDB, key)
		if got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	// Partial PUT /api/config bodies update only their own field (the merge
	// the split JS savers rely on).
	putConfigJSON(t, h, `{"username": "alice"}`)
	putConfigJSON(t, h, `{"mcp_port": 9999}`)
	putConfigJSON(t, h, `{"ocr_model": "qwen3-vl"}`)
	cfg = loadConfig(t, cfgService)
	if cfg.Username != "alice" || cfg.MCPPort != 9999 || cfg.OCRModel != "qwen3-vl" {
		t.Errorf("partial PUTs did not land: username=%q mcp_port=%d ocr_model=%q",
			cfg.Username, cfg.MCPPort, cfg.OCRModel)
	}
	if !cfg.EmbedEnabled || cfg.CalDAVCollectionName != "My Tasks" {
		t.Error("partial PUT clobbered fields outside its body")
	}

	// A PUT omitting ocr_api_key leaves a previously-set key intact (the
	// key-wipe fix: blank field => key omitted from the body).
	putConfigJSON(t, h, `{"ocr_api_key": "sekrit-key"}`)
	putConfigJSON(t, h, `{"ocr_model": "qwen3-vl-8b"}`)
	cfg = loadConfig(t, cfgService)
	if cfg.OCRAPIKey != "sekrit-key" {
		t.Errorf("ocr_api_key = %q after key-less PUT, want sekrit-key preserved", cfg.OCRAPIKey)
	}
}
