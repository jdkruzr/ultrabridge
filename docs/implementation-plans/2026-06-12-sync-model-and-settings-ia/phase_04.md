# Source Sync-Semantics Surfacing + Settings IA — Implementation Plan

**Goal:** Turn the single Settings page into four deep-linkable, navigable group routes (`/settings/{devices,ai,integrations,system}`) using the app's existing HTMX sidebar idiom — without yet moving any card (that's Phase 5).

**Architecture:** `GET /settings` 303-redirects to `/settings/devices`. A new `GET /settings/{group}` handler validates the group, sets `activeTab=settings-<group>`, and renders a per-group template. For this phase the four group templates and the legacy `settings.html` all render one shared `_settings_all` block (the existing body, verbatim) so every setting stays visible and saveable through the transition. The sidebar gains a "Settings" group label + four `nav-sub` items, exactly like the existing Supernote/Boox/ForestNote nav groups. Phase 5 swaps the group templates' bodies for their real, filtered cards.

**Tech Stack:** Go 1.22 `net/http.ServeMux` path patterns (`{group}` wildcard, `r.PathValue`), `html/template`, HTMX 1.9.10 (`hx-get`→`#main-content`, `hx-push-url`, `activeTab`).

**Scope:** Phase 4 of 6. Independent of Phases 1–3.

**Codebase verified:** 2026-06-12.
- `GET /settings` → `handleSettings` (handler.go:299, :1033) → `renderTemplate(w,r,"settings", h.settingsData(r))`. `settingsData(r)` (1040–1105) is reused by mutation re-renders.
- Sync-device mutations redirect to `/settings#sync-devices` (handler.go:1134 prune, :1156 compact); prune's HX path calls `h.handleSettings` inline (:1131).
- `handleSettingsSave` (:1399–1400), `handleBackfillEmbeddings` (:1409), `handleMCPTokenCreate/Revoke` (~:1696) redirect to / render `"settings"`.
- Sidebar single "Settings" link: `layout.html:516–517` (`activeTab "settings"`). Nav-group idiom: `nav-group-label` + `nav-item nav-sub` + `hx-get`/`hx-target="#main-content"`/`hx-push-url="true"` (layout.html:476–501).
- `renderTemplate` reads `templates/<name>.html` as the per-request `"content"` template and Clones `h.tmpl` (which has every `{{define}}` from startup `ParseFS`), so a group template can `{{template "_settings_all" .}}`.
- `TestRoutes` (routes_test.go:24) and `TestSectionVisibility` (:54) assert `GET /settings` → 200 — both must change. Check `sync_devices_test.go` for any assertion of the `/settings#sync-devices` redirect target and update it to `/settings/devices`.

---

## Acceptance Criteria Coverage

This phase implements and tests:

### sync-model-and-settings-ia.AC4: Settings split into deep-linkable groups
- **sync-model-and-settings-ia.AC4.1 Success:** `GET /settings` redirects to `/settings/devices`.
- **sync-model-and-settings-ia.AC4.2 Success (mechanism; content completed in Phase 5):** `GET /settings/{devices|ai|integrations|system}` renders into `#main-content`, with the URL pushed (bookmarkable). The "only that group's cards" filtering lands in Phase 5.
- **sync-model-and-settings-ia.AC4.3 Success:** The sidebar shows a "Settings" group with four sub-items; the active group is highlighted via `activeTab`.
- **sync-model-and-settings-ia.AC4.4 Success:** Settings mutations that previously redirected to `/settings#sync-devices` now redirect to `/settings/devices` (no hash hack).
- **sync-model-and-settings-ia.AC4.5 Failure:** An unknown group path (e.g. `/settings/bogus`) does not 500 — it falls back to the default group.

---

**Dependencies:** None.

<!-- START_TASK_1 -->
### Task 1: Extract the existing settings body into a shared `_settings_all` block

**Files:**
- Create: `internal/web/templates/_settings_all.html`
- Modify: `internal/web/templates/settings.html`

**Implementation:**

Mechanical move — no markup changes. Wrap the *entire current* `settings.html` content (the `<div id="settings">…</div>` and the trailing `<script>…</script>`) in a define inside the new partial:

```html
{{define "_settings_all"}}
… verbatim current contents of settings.html …
{{end}}
```

Then replace `settings.html`'s contents with a single line so the legacy `"settings"` template name (still referenced by `handleMCPTokenCreate/Revoke`, `handleSettingsSave`, etc. until Phase 5) keeps working:

```html
{{template "_settings_all" .}}
```

`_settings_all.html` is `_`-prefixed, so the `//go:embed all:templates` directive already covers it.

**Verification:**

Run: `go build -C /home/sysop/src/ultrabridge ./internal/web/`
Expected: builds. (Content parity is asserted in Task 5.)

**Commit:** `refactor(web): extract settings body into shared _settings_all block`
<!-- END_TASK_1 -->

<!-- START_TASK_2 -->
### Task 2: Four group templates (transitional pass-throughs)

**Files:**
- Create: `internal/web/templates/settings_devices.html`
- Create: `internal/web/templates/settings_ai.html`
- Create: `internal/web/templates/settings_integrations.html`
- Create: `internal/web/templates/settings_system.html`

**Implementation:**

For this phase each group template renders the full body so nothing is lost or unreachable mid-sequence. Each file's entire content is:

```html
{{template "_settings_all" .}}
```

Phase 5 replaces each with its real, filtered cards.

**Verification:** covered by Task 5 routing test.

**Commit:** `feat(web): add settings group templates (transitional pass-throughs)`
<!-- END_TASK_2 -->

<!-- START_TASK_3 -->
### Task 3: Routing — redirect + group dispatch + retargeted mutation redirects

**Files:**
- Modify: `internal/web/handler.go`

**Implementation:**

1. Replace `handleSettings` so `GET /settings` redirects to the default group:

```go
func (h *Handler) handleSettings(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/settings/devices", http.StatusSeeOther)
}
```

2. Add a group validator and dispatch handler:

```go
// settingsGroups is the canonical ordered set of Settings sub-pages.
var settingsGroups = []string{"devices", "ai", "integrations", "system"}

func validSettingsGroup(g string) bool {
	for _, s := range settingsGroups {
		if s == g {
			return true
		}
	}
	return false
}

// handleSettingsGroup renders one Settings group. Unknown groups fall back to
// the default (a clean 303, never a 500) — AC4.5.
func (h *Handler) handleSettingsGroup(w http.ResponseWriter, r *http.Request) {
	group := r.PathValue("group")
	if !validSettingsGroup(group) {
		http.Redirect(w, r, "/settings/devices", http.StatusSeeOther)
		return
	}
	data := h.settingsData(r)
	data["activeTab"] = "settings-" + group
	data["SettingsGroup"] = group
	h.renderTemplate(w, r, "settings_"+group, data)
}
```

3. Register the wildcard route (GET only — the existing `POST /settings/save`, `/settings/mcp-tokens/*`, `/settings/sync-devices/*` are different methods/paths and do not conflict):

```go
h.mux.HandleFunc("GET /settings", h.handleSettings)
h.mux.HandleFunc("GET /settings/{group}", h.handleSettingsGroup)
```

4. Retarget the sync-device mutation redirects from `/settings#sync-devices` to `/settings/devices`, and fix prune's inline HX re-render (it currently calls `h.handleSettings`, which is now a redirect — render the devices group directly instead):

- `handleSyncDevicePrune`: HX path → `data := h.settingsData(r); data["activeTab"]="settings-devices"; data["SettingsGroup"]="devices"; h.renderTemplate(w,r,"settings_devices", data)`. Non-HX → `http.Redirect(w,r,"/settings/devices", http.StatusSeeOther)`.
- `handleSyncDeviceCompact`: HX path → same render of `settings_devices` with `data["SyncCompactResult"]=result` added. Non-HX → redirect `/settings/devices`.

Leave `handleSettingsSave`, `handleBackfillEmbeddings`, and the MCP-token handlers redirecting to / rendering `"settings"` for now — `"settings"` still resolves (Task 1) and `/settings` 303s onward. Phase 5 retargets these to their owning groups.

**Verification:**

Run: `go build -C /home/sysop/src/ultrabridge ./internal/web/`
Expected: builds.

**Commit:** `feat(web): route /settings into deep-linkable group pages`
<!-- END_TASK_3 -->

<!-- START_TASK_4 -->
### Task 4: Sidebar Settings sub-nav

**Files:**
- Modify: `internal/web/templates/layout.html` (replace the single Settings `nav-item` at :516–517)

**Implementation:**

Replace the lone Settings link with a group label + four `nav-sub` items, mirroring the existing device nav groups (each `hx-get` → `#main-content`, `hx-push-url="true"`, highlighted by `activeTab`):

```html
<div class="nav-group-label">Settings</div>
<a class="nav-item nav-sub {{if eq .activeTab "settings-devices"}}active{{end}}"
   href="/settings/devices" hx-get="/settings/devices" hx-target="#main-content" hx-push-url="true">Devices</a>
<a class="nav-item nav-sub {{if eq .activeTab "settings-ai"}}active{{end}}"
   href="/settings/ai" hx-get="/settings/ai" hx-target="#main-content" hx-push-url="true">AI &amp; Processing</a>
<a class="nav-item nav-sub {{if eq .activeTab "settings-integrations"}}active{{end}}"
   href="/settings/integrations" hx-get="/settings/integrations" hx-target="#main-content" hx-push-url="true">Integrations</a>
<a class="nav-item nav-sub {{if eq .activeTab "settings-system"}}active{{end}}"
   href="/settings/system" hx-get="/settings/system" hx-target="#main-content" hx-push-url="true">System</a>
```

**Verification:** covered by Task 5 (asserts the active highlight renders per group).

**Commit:** `feat(web): add Settings sub-nav group to the sidebar`
<!-- END_TASK_4 -->

<!-- START_TASK_5 -->
### Task 5: Routing + nav tests; update stale assertions

**Verifies:** sync-model-and-settings-ia.AC4.1, AC4.2 (mechanism), AC4.3, AC4.4, AC4.5

**Files:**
- Modify: `internal/web/routes_test.go` (TestRoutes, TestSectionVisibility)
- Add: a `TestSettingsGroupRouting` test (routes_test.go or a new `settings_routing_test.go`)
- Modify: `internal/web/sync_devices_test.go` if it asserts the `/settings#sync-devices` redirect target

**Testing:**

- **AC4.1:** Update `TestRoutes`: `GET /settings` now expects `http.StatusSeeOther`; assert the `Location` header is `/settings/devices`. Add `GET /settings/{devices,ai,integrations,system}` → `http.StatusOK`.
- **AC4.2 (mechanism):** In `TestSettingsGroupRouting`, issue a non-HX `GET /settings/ai`; assert 200 and that the response is a full page rendered through the layout containing `id="main-content"` (the swap target). Issue the same with header `HX-Request: true`; assert 200 and that the body is the fragment (no `<!DOCTYPE`), i.e. content destined for `#main-content`. Bookmarkability is the plain-GET 200 plus the nav's `hx-push-url="true"` (assert that attribute is present on the rendered nav links).
- **AC4.3:** For each group, assert the rendered sidebar marks exactly that group's `nav-sub` link `active` (e.g. `/settings/ai` response contains `nav-item nav-sub active` on the "AI &amp; Processing" link and not on the others). A substring check on `activeTab`-driven `active` class adjacent to each `href` is sufficient.
- **AC4.4:** Drive `handleSyncDevicePrune` and `handleSyncDeviceCompact` with a wired mock `SyncDeviceService`, non-HX; assert the `Location` header is `/settings/devices` (not `/settings#sync-devices`). Update any existing assertion in `sync_devices_test.go` accordingly.
- **AC4.5:** `GET /settings/bogus` → assert NOT 500; specifically `http.StatusSeeOther` with `Location: /settings/devices`.
- Update `TestSectionVisibility` to request `GET /settings/devices` (expect 200) instead of `GET /settings`.

**Verification:**

Run: `go test -C /home/sysop/src/ultrabridge ./internal/web/`
Expected: all pass.

**Commit:** `test(web): cover settings group routing, nav highlight, and redirects`
<!-- END_TASK_5 -->
