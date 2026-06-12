# Source Sync-Semantics Surfacing + Settings IA — Implementation Plan

**Goal:** Move every settings card into its correct group template, merge the three MCP cards into one section, dissolve both "General" grab-bag cards, and split their two save mechanisms so each group saves independently — with no setting lost.

**Architecture:** The four group templates from Phase 4 stop being pass-throughs and get their real, filtered cards (cut from the `_settings_all` holding pen). Two save paths split:
1. **`PUT /api/config` (JS):** the monolithic `saveConfig()` becomes three per-group functions (`saveAuthConfig`, `saveOCRConfig`, `saveMCPConfig`), each sending only its subset. Safe because `handlePutConfig` merges by key-presence (verified).
2. **`POST /settings/save` (form):** the single `section=general` form splits into `section=ai`, `section=integrations`, `section=system`, each touching only its own fields (the existing handler sets fields *unconditionally*, so reusing `general` across split forms would blank siblings — new sections are required).

Then the holding pen (`_settings_all.html` / `settings.html`), the dead `saveConfig()`, and the dead `case "general"` are removed.

**Tech Stack:** Go `net/http` form/JSON handlers, `html/template`, vanilla JS `fetch` (HTMX for forms).

**Scope:** Phase 5 of 6. Depends on Phase 4.

**Codebase verified:** 2026-06-12. Card source line-ranges in the original `settings.html` (now verbatim inside `_settings_all.html`):

| Card | Lines | Destination group | Save path |
|---|---|---|---|
| General #1 — Authentication | 14–25 | System | `saveAuthConfig()` PUT |
| General #1 — OCR Configuration | 29–58 | AI & Processing | `saveOCRConfig()` PUT |
| General #1 — MCP Server (port) | 62–72 | Integrations (MCP §Server) | `saveMCPConfig()` PUT |
| Sources | 80–160 | Devices | JS (unchanged) |
| Supernote Settings | 163–187 | Devices | form `section=supernote` |
| UB-as-SPC Device Sync Server | 189–274 | Devices | form `section=ub-spc` |
| ForestNote Device Sync | 276–325 | Devices | form `section=sync` |
| Sync Devices | 327–405 | Devices | sync-device routes |
| Boox Settings | 407–486 | Devices | form `section=boox` |
| Boox Database Maintenance | 488–521 | Devices | maintenance routes |
| MCP Connection | 523–568 | Integrations (MCP §Connection) | read-only |
| MCP Tokens | 570–630 | Integrations (MCP §Tokens) | mcp-token routes |
| General #2 — RAG Search | 638–668 | AI & Processing | form `section=ai` |
| General #2 — AI Chat | 672–695 | AI & Processing | form `section=ai` |
| General #2 — CalDAV | 699–707 | Integrations | form `section=integrations` |
| General #2 — Debugging | 711–720 | System | form `section=system` |
| General #2 — backfill button | 725–733 | AI & Processing | `/settings/backfill-embeddings` |

- `handlePutConfig` overlays only keys present in the request body (`config_api.go:155–230`) → partial PUTs are safe.
- `handleSettingsSave` (handler.go:1289–1401): `case "general"` sets `EmbedEnabled/ChatEnabled/LogVerboseAPI` unconditionally; `caldav_collection_name` guarded by non-empty. `supernote`/`boox` cases early-return with `http.Redirect(.., "/settings", ..)`; others fall to the shared `UpdateConfig` + `http.Redirect(.., "/settings", ..)` tail (1399–1400).
- `handleMCPTokenCreate` HX path (1696) renders `"settings"` with ONLY `{"NewMCPToken": t}` — missing `settingsData`, so it blanks the page; retarget + pass full `settingsData` to fix.
- **Pre-existing bug to NOT replicate:** current `saveConfig()` always includes `ocr_api_key` in the PUT body (layout.html:1103); with the empty-field "leave to keep current" placeholder, that wipes the stored key. `saveOCRConfig()` must omit `ocr_api_key` when the field is blank (mirror the password pattern). Same for `password` in `saveAuthConfig()`.

---

## Acceptance Criteria Coverage

This phase implements and tests:

### sync-model-and-settings-ia.AC4 (completes AC4.2 content)
- **sync-model-and-settings-ia.AC4.2 Success (content):** Each group route now renders ONLY that group's cards.

### sync-model-and-settings-ia.AC5: Regrouping + consolidation
- **sync-model-and-settings-ia.AC5.1 Success:** No card is labeled a duplicate "General"; every former General-card setting appears under its correct group (Auth→System, OCR→AI, MCP-Server→Integrations, RAG/Chat→AI, CalDAV→Integrations, Debugging→System).
- **sync-model-and-settings-ia.AC5.2 Success:** MCP appears as a single section (Server + Connection + Tokens subsections) under Integrations.
- **sync-model-and-settings-ia.AC5.3 Success:** Every setting that existed pre-restructure still loads and saves.

---

**Dependencies:** Phase 4.

<!-- START_TASK_1 -->
### Task 1: System group — Authentication + Debugging

**Files:**
- Modify: `internal/web/templates/settings_system.html` (replace the pass-through)
- Modify: `internal/web/templates/layout.html` (add `saveAuthConfig()`; reuse the existing `<script>` block where `saveConfig` lives)
- Modify: `internal/web/handler.go` (add `case "system"` to `handleSettingsSave`)
- Add: `internal/web/templates/_settings_restart_banner.html` — extract the RestartRequired banner (original lines 3–8) into `{{define "_settings_restart_banner"}}…{{end}}`; include it at the top of all four group templates.

**Implementation:**

`settings_system.html`:
```html
{{template "_settings_restart_banner" .}}
{{if .Config}}
<div class="card">
  <h2>Authentication</h2>
  … Auth fields (original 16–24): #config-username, #config-password …
  <div class="toolbar">
    <button type="button" onclick="saveAuthConfig()">Save Authentication</button>
    <span id="auth-config-status" class="text-small"></span>
  </div>
</div>
<div class="card">
  <h2>System</h2>
  <form hx-post="/settings/save" hx-target="#main-content">
    <input type="hidden" name="section" value="system">
    … Debugging block (original 711–720): #log-verbose-api …
    <button type="submit" class="btn-small">Save System Settings</button>
  </form>
</div>
{{end}}
```

`saveAuthConfig()` (new JS, modeled on `saveConfig`): reads `#config-username` + `#config-password`; builds a body with `username`, and `password` ONLY when non-empty; `PUT /api/config`; writes status to `#auth-config-status`; handles `restart_required`.

`handleSettingsSave` new case:
```go
case "system":
	cfg.LogVerboseAPI = r.FormValue("log_verbose_api") == "true"
```

**Verification:**
Run: `go build -C /home/sysop/src/ultrabridge ./internal/web/`
Expected: builds. Manual save round-trip covered by Task 6.

**Commit:** `feat(web): System settings group (auth + debugging) with scoped saves`
<!-- END_TASK_1 -->

<!-- START_TASK_2 -->
### Task 2: AI & Processing group — OCR + RAG + Chat + backfill

**Files:**
- Modify: `internal/web/templates/settings_ai.html`
- Modify: `internal/web/templates/layout.html` (add `saveOCRConfig()`)
- Modify: `internal/web/handler.go` (`case "ai"`; retarget `handleBackfillEmbeddings` redirect)

**Implementation:**

`settings_ai.html`: restart banner; an **OCR Configuration** card (original 29–58 fields) with `<button onclick="saveOCRConfig()">Save OCR Settings</button>` + `#ocr-config-status`; a **RAG Search + AI Chat** card containing one `<form … section="ai">` wrapping the RAG block (638–668) and the Chat block (672–695) with one submit; and the backfill button block (725–733, gated on `.EmbedEnabled`).

`saveOCRConfig()`: reads `#config-ocr-format/-api-url/-api-key/-model/-concurrency/-max-file-mb`; body includes `ocr_format, ocr_api_url, ocr_model, ocr_concurrency, ocr_max_file_mb`, and `ocr_api_key` ONLY when non-empty (fixes the key-wipe bug); `PUT /api/config`; status → `#ocr-config-status`.

`handleSettingsSave` new case:
```go
case "ai":
	cfg.EmbedEnabled = r.FormValue("embed_enabled") == "true"
	cfg.OllamaURL, cfg.OllamaEmbedModel = r.FormValue("ollama_url"), r.FormValue("ollama_embed_model")
	cfg.ChatEnabled = r.FormValue("chat_enabled") == "true"
	cfg.ChatAPIURL, cfg.ChatModel = r.FormValue("chat_api_url"), r.FormValue("chat_model")
```

`handleBackfillEmbeddings`: change its redirect from `/settings` to `/settings/ai`.

**Verification:**
Run: `go build -C /home/sysop/src/ultrabridge ./internal/web/`
Expected: builds.

**Commit:** `feat(web): AI & Processing settings group (OCR + RAG + Chat)`
<!-- END_TASK_2 -->

<!-- START_TASK_3 -->
### Task 3: Integrations group — unified MCP section + CalDAV

**Files:**
- Modify: `internal/web/templates/settings_integrations.html`
- Modify: `internal/web/templates/layout.html` (add `saveMCPConfig()`)
- Modify: `internal/web/handler.go` (`case "integrations"`; retarget `handleMCPTokenCreate`/`handleMCPTokenRevoke`)

**Implementation:**

`settings_integrations.html`: restart banner; a single **MCP** card with three `<h3>` subsections — **Server** (the port field from original 62–72, with `<button onclick="saveMCPConfig()">Save MCP Server</button>` + `#mcp-config-status`), **Connection** (original 523–568, gated `.MCPEnabled`, read-only), **Tokens** (original 570–630, gated `.MCPTokensEnabled`, including the create/revoke forms and the `.NewMCPToken` flash) — satisfying AC5.2's "one section". Then a **CalDAV** card: `<form … section="integrations">` wrapping the CalDAV block (699–707).

`saveMCPConfig()`: reads `#config-mcp-port`; body `{ mcp_port: parseInt(...)||0 }`; `PUT /api/config`; status → `#mcp-config-status`.

`handleSettingsSave` new case:
```go
case "integrations":
	if v := strings.TrimSpace(r.FormValue("caldav_collection_name")); v != "" {
		cfg.CalDAVCollectionName = v
	}
```

Retarget MCP-token handlers to the integrations group AND pass full `settingsData` on HX (fixes the page-blanking bug):
```go
// handleMCPTokenCreate, HX branch:
data := h.settingsData(r)
data["activeTab"], data["SettingsGroup"], data["NewMCPToken"] = "settings-integrations", "integrations", t
h.renderTemplate(w, r, "settings_integrations", data)
// non-HX: http.Redirect(w, r, "/settings/integrations?new_token="+url.QueryEscape(t), http.StatusSeeOther)

// handleMCPTokenRevoke, HX branch:
data := h.settingsData(r)
data["activeTab"], data["SettingsGroup"] = "settings-integrations", "integrations"
h.renderTemplate(w, r, "settings_integrations", data)
// non-HX: http.Redirect(w, r, "/settings/integrations", http.StatusSeeOther)
```

**Verification:**
Run: `go build -C /home/sysop/src/ultrabridge ./internal/web/`
Expected: builds.

**Commit:** `feat(web): Integrations settings group (unified MCP + CalDAV)`
<!-- END_TASK_3 -->

<!-- START_TASK_4 -->
### Task 4: Devices group — stack the device cards + group-aware save redirects

**Files:**
- Modify: `internal/web/templates/settings_devices.html`
- Modify: `internal/web/handler.go` (`handleSettingsSave` redirects)

**Implementation:**

`settings_devices.html` (Phase 6 will refactor this into uniform per-source sections; here it just lands the cards under the devices route): restart banner, then the Sources card (80–160), Supernote Settings (163–187), UB-as-SPC (189–274), ForestNote Device Sync (276–325), Sync Devices (327–405), Boox Settings (407–486), Boox Database Maintenance (488–521) — moved verbatim.

`handleSettingsSave` redirects → owning group. Add a helper and use it:
```go
func settingsGroupForSection(section string) string {
	switch section {
	case "supernote", "ub-spc", "sync", "boox":
		return "devices"
	case "ai", "integrations", "system":
		return section
	default:
		return "devices"
	}
}
```
- `case "supernote"` and `case "boox"` early-returns: change `"/settings"` → `"/settings/devices"`.
- Shared tail (1399–1400): replace `http.Redirect(w, r, "/settings", ..)` with `http.Redirect(w, r, "/settings/"+settingsGroupForSection(r.FormValue("section")), http.StatusSeeOther)`.

**Verification:**
Run: `go build -C /home/sysop/src/ultrabridge ./internal/web/`
Expected: builds. All four group templates are now real; the holding pen is unused.

**Commit:** `feat(web): Devices settings group + group-aware save redirects`
<!-- END_TASK_4 -->

<!-- START_TASK_5 -->
### Task 5: Remove the holding pen and dead save code

**Files:**
- Delete: `internal/web/templates/settings.html`
- Delete: `internal/web/templates/_settings_all.html`
- Modify: `internal/web/handler.go` (remove `case "general"` from `handleSettingsSave`)
- Modify: `internal/web/templates/layout.html` (remove the now-dead `saveConfig()` function)

**Implementation:**

Grep first to prove nothing still references them:
```bash
grep -rn '"settings"\|_settings_all\|saveConfig(\|section.*general\|"general"' internal/web/ --include=*.go --include=*.html
```
Expected after Tasks 1–4: no `renderTemplate(..,"settings",..)` calls remain, no `onclick="saveConfig()"` remains (all replaced by the three scoped functions), and no template posts `section=general`. Remove `case "general":` and the dead `saveConfig()` body. Delete the two template files.

**Verification:**
Run: `go build -C /home/sysop/src/ultrabridge ./internal/web/ && go vet -C /home/sysop/src/ultrabridge ./internal/web/`
Expected: builds clean, no references to deleted templates.

**Commit:** `chore(web): drop the legacy settings holding pen and saveConfig`
<!-- END_TASK_5 -->

<!-- START_TASK_6 -->
### Task 6: Group membership + save round-trip tests

**Verifies:** sync-model-and-settings-ia.AC4.2 (content), AC5.1, AC5.2, AC5.3

**Files:**
- Add: `internal/web/settings_groups_test.go`

**Testing:**

Use a `Handler` over an in-memory notedb seeded with a real `appconfig.Config` (so `Config`, MCP tokens, etc. populate), mirroring `setupTestHandler` but with config values set.

- **AC4.2 (content) / AC5.1:** For each group route, render (non-HX) and assert the response contains that group's cards and NOT others. Concretely: `/settings/system` contains "Authentication" and "Debugging" and does NOT contain "OCR Configuration", "RAG Search", "Sources", "MCP". `/settings/ai` contains "OCR", "RAG Search", "AI Chat" and not "Authentication"/"CalDAV"/"MCP". `/settings/integrations` contains "MCP" and "CalDAV" and not "OCR"/"Authentication". `/settings/devices` contains "Sources"/"Supernote"/"Boox"/"ForestNote" and not "OCR"/"MCP Tokens"/"RAG". Assert NO group renders a card titled exactly "General".
- **AC5.2:** `/settings/integrations` renders a single element whose heading is "MCP" containing the substrings for Server (the `config-mcp-port` field), Connection, and Tokens — assert all three subsection markers appear within the one card (e.g. the three `<h3>` labels Server/Connection/Tokens) and that there is exactly one top-level MCP `<h2>`.
- **AC5.3:** Round-trip each save path against the handler:
  - `POST /settings/save` with `section=ai` (embed/chat fields) → `GetConfig` reflects them; with `section=integrations` (`caldav_collection_name`) → reflected; with `section=system` (`log_verbose_api=true`) → reflected. Assert a `section=ai` POST does NOT change `LogVerboseAPI` or `CalDAVCollectionName` (proves the split doesn't blank siblings).
  - `PUT /api/config` with only `{username}` then only `{mcp_port}` then only `{ocr_model}` — assert each updates just its field and leaves the others intact (partial-merge proof for the split JS savers).
  - A `PUT /api/config` with `ocr_api_key` omitted leaves a previously-set key intact (guards the key-wipe fix).

**Verification:**
Run: `go test -C /home/sysop/src/ultrabridge ./internal/web/`
Expected: all pass.

**Commit:** `test(web): settings group membership, MCP consolidation, scoped saves`
<!-- END_TASK_6 -->
