# Source Sync-Semantics Surfacing + Settings IA — Implementation Plan

**Goal:** Render a per-source sync-model banner on each Files tab, above the pipeline-status panel — glyph + label + blurb, with Boox lifted to a muted attention tone.

**Architecture:** A shared `_sync_model_banner.html` partial fed a `source.SyncModel`. The three Files-tab handlers set `data["syncModel"]` alongside the existing `data["pipelinePanel"]`; each tab template renders the partial just above `{{template "_files_status_panel" ...}}`. Glyph and tone are derived in the template from `Direction.String` (a view concern) — no icon library, just the Unicode `⇅`/`⬇`.

**Tech Stack:** Go `html/template`, HTMX-served fragments, existing partial-smoke-test idiom (`partials_smoke_test.go` parses `templateFS` and `ExecuteTemplate`s).

**Scope:** Phase 3 of 6.

**Codebase verified:** 2026-06-12.
- Files tabs render the status panel via `{{template "_files_status_panel" .pipelinePanel}}` inside the `{{else if .files}}` branch (`files_supernote.html:10`; same idiom in `files_boox.html`, `files_forestnote.html`).
- Handlers set `data["pipelinePanel"] = pipelinePanel{...}` on the list path: `handleFilesSupernote` (handler.go:575), `handleFilesBoox`, `handleFilesForestNote`.
- `internal/web` is the same package as `sources_api.go`, which imports `internal/source`; `handler.go` can call `source.SyncModelFor` (add the import to handler.go if not already present).
- `//go:embed all:templates` (handler.go) already covers new `_`-prefixed partials — no embed change needed.
- Tone reference: the SPC reverse-proxy note uses `border-left: 3px solid #c97` (muted amber) — reuse that accent for the attention tone; it is deliberately NOT `var(--status-text-failed)` (error red).

---

## Acceptance Criteria Coverage

This phase implements and tests:

### sync-model-and-settings-ia.AC3: Files-tab banner
- **sync-model-and-settings-ia.AC3.1 Success:** Each Files tab renders one banner above the pipeline-status panel showing the source's glyph + label + blurb.
- **sync-model-and-settings-ia.AC3.2 Success:** Glyph derives from `Direction` (`⇅` two-way, `⬇` receive-only); both two-way sources share `⇅`.
- **sync-model-and-settings-ia.AC3.3 Success:** The Boox banner is rendered in the attention tone (muted accent), not error red; Supernote/ForestNote render in the quiet informational tone.
- **sync-model-and-settings-ia.AC3.4 Success:** The Boox blurb states deletes/renames on the device never reach UB.

### sync-model-and-settings-ia.AC7: Cross-cutting
- **sync-model-and-settings-ia.AC7.2 Success:** Banner glyphs are Unicode characters, not an icon-library dependency.

---

**Dependencies:** Phase 1 (`source.SyncModelFor`, `Direction.String`).

<!-- START_SUBCOMPONENT_A (tasks 1-3) -->
<!-- START_TASK_1 -->
### Task 1: Shared sync-model banner partial

**Files:**
- Create: `internal/web/templates/_sync_model_banner.html`

**Implementation:**

Define a `_sync_model_banner` block whose context is a `source.SyncModel`. Derive the glyph and tone from `Direction.String` (`"two_way"` → `⇅`, else `⬇`; `"one_way_in"` → attention). Use a stable class (`sync-model-attention` / `sync-model-quiet`) so the smoke test and reviewers can assert tone, and the muted-amber `#c97` left border (not error red) for attention.

```html
{{define "_sync_model_banner"}}
{{- /* Context: source.SyncModel. Glyph + tone are view concerns derived from
       Direction. Unicode glyphs only — no icon library (AC7.2). */ -}}
{{- $oneWay := eq .Direction.String "one_way_in" -}}
<div class="sync-model-banner {{if $oneWay}}sync-model-attention{{else}}sync-model-quiet{{end}}"
     style="display:flex; align-items:flex-start; gap:0.6rem; padding:0.75rem 1rem; margin-bottom:1rem; border:1px solid var(--border-color); border-radius:var(--radius); background:var(--bg-color);{{if $oneWay}} border-left:3px solid #c97;{{end}}">
  <span aria-hidden="true" style="font-size:1.1rem; line-height:1.3;">{{if eq .Direction.String "two_way"}}⇅{{else}}⬇{{end}}</span>
  <span>
    <span class="text-bold">{{.Label}}</span>
    <span class="text-small" style="display:block; color:var(--text-secondary);">{{.Blurb}}</span>
  </span>
</div>
{{end}}
```

**Verification:** covered by Task 3's smoke test.

**Commit:** `feat(web): add shared sync-model banner partial`
<!-- END_TASK_1 -->

<!-- START_TASK_2 -->
### Task 2: Feed syncModel into the Files-tab handlers + templates

**Files:**
- Modify: `internal/web/handler.go` — in `handleFilesSupernote` (alongside `data["pipelinePanel"]` at ~:575), `handleFilesBoox`, and `handleFilesForestNote`, set `data["syncModel"] = source.SyncModelFor("<type>")` (`supernote` / `boox` / `forestnote` respectively) on the list path. Ensure `internal/source` is imported.
- Modify: `internal/web/templates/files_supernote.html` — render the banner immediately above `{{template "_files_status_panel" .pipelinePanel}}`.
- Modify: `internal/web/templates/files_boox.html` — same.
- Modify: `internal/web/templates/files_forestnote.html` — same (above its status panel).

**Implementation:**

In each tab template, guard on the key so the error/empty branches (which don't set it) stay clean:

```html
{{if .syncModel}}{{template "_sync_model_banner" .syncModel}}{{end}}
{{template "_files_status_panel" .pipelinePanel}}
```

(An absent map key renders falsy; a present `source.SyncModel` struct is always truthy — so this shows the banner exactly on the list path where the handler set it.)

**Verification:**

Run: `go build -C /home/sysop/src/ultrabridge ./internal/web/`
Expected: builds without errors.

**Commit:** `feat(web): render sync-model banner on each Files tab`
<!-- END_TASK_2 -->

<!-- START_TASK_3 -->
### Task 3: Banner partial smoke test

**Verifies:** sync-model-and-settings-ia.AC3.1, AC3.2, AC3.3, AC3.4, AC7.2

**Files:**
- Modify: `internal/web/partials_smoke_test.go` — add a test parsing `templates/_sync_model_banner.html` from `templateFS` and `ExecuteTemplate`-ing `source.SyncModelFor(<type>)` for each type (mirrors `TestSharedPartialsRender`).

**Testing:**

Render the partial for each of the three descriptors and assert:

- **AC3.1 / AC3.2:** supernote and forestnote outputs contain `⇅` and their labels (`Two-way sync`, `Live mirror`); boox output contains `⬇` and `Receive-only`. (Both two-way sources sharing `⇅` proves AC3.2's "share the glyph".)
- **AC3.3:** boox output contains `sync-model-attention` and the `#c97` accent; supernote/forestnote outputs contain `sync-model-quiet` and do NOT contain `sync-model-attention`. Assert NO output contains `var(--status-text-failed)` (proves attention ≠ error red).
- **AC3.4:** boox blurb output contains the substring `never reach UltraBridge`.
- **AC7.2:** assert the raw glyphs `⇅` / `⬇` are literal runes in the output and that the template references no `<svg`, `<i class=`, or icon-font markup (grep the rendered string for their absence).

**Verification:**

Run: `go test -C /home/sysop/src/ultrabridge ./internal/web/ -run TestSyncModelBanner`
Expected: passes.

**Commit:** `test(web): smoke-test sync-model banner glyph, tone, and blurb`
<!-- END_TASK_3 -->
<!-- END_SUBCOMPONENT_A -->
