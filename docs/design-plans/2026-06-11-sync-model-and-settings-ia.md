# Source Sync-Semantics Surfacing + Settings IA Restructure — Design

## Summary

This design surfaces each note backend's fundamentally different sync behavior — which the parity-focused UI currently hides — and reorganizes the overgrown Settings page around it. The core is a small, pure `SyncModel` descriptor in `internal/source` keyed on source *type* (two-way / receive-only / live mirror), exposed on the `/api/sources` representation and reused by every surface that needs it: a Unicode-glyph banner on the Files tabs and a matching banner in Settings. Boox's one-way, receive-only nature — device deletes never reach UB — gets a distinct standing warning, since UB has no event to react to.

The same descriptor drives a Settings restructure: the single long page splits into four deep-linkable groups (Devices, AI & Processing, Integrations, System) using the app's existing HTMX sidebar nav idiom, eliminating the two duplicate "General" grab-bag cards and the three-way MCP split. The Devices group renders a uniform per-source section — banner, configuration, and a device-management slot that appears only when the descriptor says a registry exists. Supernote and ForestNote device management remain separate surfaces; Boox shows none. Implementation runs in six phases: descriptor → API field → Files banner, then Settings routing → regroup → Devices sections.

## Definition of Done

1. Every note source's **sync model** — how that backend syncs and what a delete means — is legible to the user, not hidden behind a uniform UI. Specifically: Supernote = two-way, ForestNote = live two-way mirror, Boox = one-way receive-only.
2. Boox's defining gotcha — **device deletes/renames never reach UB** — is communicated as a standing, visually distinct statement, since UB has no event to react to.
3. The sync model is a **first-class concept in code** (not a string in a template) and is exposed on the **`/api/sources`** representation (API-first), then reused by every surface that shows it.
4. The **Settings page is split** from one long scroll into four deep-linkable, navigable groups — **Devices · AI & Processing · Integrations · System** — using the app's existing sidebar nav idiom.
5. The regroup **eliminates the taxonomy smells**: the two separate "General" grab-bag cards and the three-way MCP split (Server/Connection/Tokens).
6. The **Devices group** presents a **uniform per-source section** (sync-model banner + configuration + source-scoped device management). Supernote and ForestNote device management stay **separate** (different protocols, unrelated permissions); Boox shows **no device list** by design.

## Acceptance Criteria
<!-- TO BE VALIDATED with user, then finalized -->

### sync-model-and-settings-ia.AC1: Sync model is a typed, exhaustive descriptor
- **AC1.1 Success:** `SyncModelFor("supernote"|"boox"|"forestnote")` returns the correct `{Label, Direction, Authority, DeletesPropagate, Blurb}` for each, pinned by test.
- **AC1.2 Success:** `Direction` marshals to stable wire strings (`"one_way_in"` / `"two_way"`).
- **AC1.3 Edge:** An unknown source type returns the explicit `Unmanaged` descriptor, never a zero/blank value.
- **AC1.4 Success:** Boox is the only descriptor with `DeletesPropagate == false`.

### sync-model-and-settings-ia.AC2: API exposes the descriptor
- **AC2.1 Success:** `GET /api/sources` includes a `sync_model` object per source with the five fields.
- **AC2.2 Success:** The persisted `SourceRow` shape is unchanged (descriptor is additive, view-only).

### sync-model-and-settings-ia.AC3: Files-tab banner
- **AC3.1 Success:** Each Files tab renders one banner above the pipeline-status panel showing the source's glyph + label + blurb.
- **AC3.2 Success:** Glyph derives from `Direction` (`⇅` two-way, `⬇` receive-only); both two-way sources share `⇅`.
- **AC3.3 Success:** The Boox banner is rendered in the attention tone (muted accent), not error red; Supernote/ForestNote render in the quiet informational tone.
- **AC3.4 Success:** The Boox blurb states deletes/renames on the device never reach UB.

### sync-model-and-settings-ia.AC4: Settings split into deep-linkable groups
- **AC4.1 Success:** `GET /settings` redirects to `/settings/devices`.
- **AC4.2 Success:** `GET /settings/{devices|ai|integrations|system}` renders only that group's cards into `#main-content`, with the URL pushed (bookmarkable).
- **AC4.3 Success:** The sidebar shows a "Settings" group with four sub-items; the active group is highlighted via `activeTab`.
- **AC4.4 Success:** Settings mutations that previously redirected to `/settings#sync-devices` now redirect to `/settings/devices` (no hash hack).
- **AC4.5 Failure:** An unknown group path (e.g. `/settings/bogus`) does not 500 — it falls back to the default group or 404s cleanly.

### sync-model-and-settings-ia.AC5: Regrouping + consolidation
- **AC5.1 Success:** No card is labeled a duplicate "General"; every former General-card setting appears under its correct group (Auth→System, OCR→AI, MCP-Server→Integrations, RAG/Chat→AI, CalDAV→Integrations, Debugging→System).
- **AC5.2 Success:** MCP appears as a single section (Server + Connection + Tokens subsections) under Integrations.
- **AC5.3 Success:** Every setting that existed pre-restructure still loads and saves.

### sync-model-and-settings-ia.AC6: Devices group per-source uniformity
- **AC6.1 Success:** The Devices group opens with a Sources card, then one structurally identical section per configured source.
- **AC6.2 Success:** Each source section renders the SyncModel banner (same partial as the Files tab), then configuration, then a device-management slot.
- **AC6.3 Success:** ForestNote's section shows its device list (name, last-seen, prune, compact); the controls work.
- **AC6.4 Success:** Boox's device slot shows a "No device registry — receive-only" note and no list.
- **AC6.5 Success:** Supernote's device slot renders a reserved "coming" placeholder (the future `spc_devices` surface), never merged with ForestNote's.

### sync-model-and-settings-ia.AC7: Cross-cutting
- **AC7.1 Success:** The `sync_model` descriptor has exactly one definition consumed by both the API and the HTML surfaces (no duplicated source of truth).
- **AC7.2 Success:** Banner glyphs are Unicode characters, not an icon-library dependency.

## Glossary

- **SyncModel**: the typed descriptor (`Label`, `Direction`, `Authority`, `DeletesPropagate`, `Blurb`) classifying how a source syncs. Keyed on source type, derived not stored.
- **Source / SourceRow**: a configured note backend (Supernote, Boox, ForestNote) with its own lifecycle and config; `SourceRow` is its database row in `internal/source`.
- **Two-way / Receive-only / Live mirror**: the three sync models — bidirectional file sync (Supernote), one-way export ingest (Boox), and a transactional row-level mirror (ForestNote).
- **Authority**: which side owns the canonical truth for a source — `Device`, `Shared (UB-hosted)`, or `Shared (row-level LWW)`.
- **Tombstone**: a soft-delete marker (`deleted_at`) that keeps a row in the mirror so delete/restore converge across devices.
- **UB-as-SPC**: UltraBridge's reimplementation of the Supernote Private Cloud server — how Supernote devices sync to UB.
- **Sync Devices / `spc_devices`**: ForestNote's device registry (sync cursors + `device_name`, exists today) and the proposed Supernote device registry (persist `equipmentNo` at login), respectively. Kept as separate surfaces.
- **RhizomeSync**: the shared sync engine ForestNote↔UB now runs on; relevant here only as the reason the descriptor describes user-facing behavior, not the engine.
- **HTMX**: the bundled hypermedia library; the app swaps server-rendered fragments into `#main-content` via `hx-get` + `hx-push-url`.
- **FCIS (Functional Core, Imperative Shell)**: house pattern separating pure logic (the descriptor lookup) from side-effecting code (rendering, encoding).
- **Carbon read-only treatment**: the quiet-but-legible visual style for non-interactive informational UI (de-emphasized, no interactive affordances).

## Architecture

Two coupled parts. The first defines what each backend's sync behavior *is*; the second reorganizes where the user encounters it.

### Part A — SyncModel descriptor (the functional core)

A new pure value type and lookup in `internal/source/syncmodel.go`:

```go
type Direction int // OneWayIn, TwoWay  — MarshalJSON → "one_way_in" | "two_way"

type SyncModel struct {
    Label            string    `json:"label"`             // "Receive-only"
    Direction        Direction `json:"direction"`
    Authority        string    `json:"authority"`         // "Device" | "Shared (UB-hosted)" | "Shared (row-level LWW)"
    DeletesPropagate bool      `json:"deletes_propagate"`
    Blurb            string    `json:"blurb"`             // the surprise-prevention line
}

func SyncModelFor(sourceType string) SyncModel // exhaustive over supernote|boox|forestnote; Unmanaged fallback
```

The three descriptors:

| Type | Label | Direction | Authority | DeletesPropagate | Blurb (gist) |
|---|---|---|---|---|---|
| supernote | Two-way sync | TwoWay | Shared (UB-hosted) | true | deletes → recoverable recycle bin |
| boox | Receive-only | OneWayIn | Device | false | exports only; device deletes/renames never reach UB |
| forestnote | Live mirror | TwoWay | Shared (row-level LWW) | true | two-way; deletes are recoverable tombstones |

The descriptor is keyed on source *type*, never on a device instance. It is derived (a constant function of type), not stored.

### Part B — surfaces consuming the descriptor

- **`/api/sources`** (`internal/web/sources_api.go`) stops encoding raw `source.SourceRow` and encodes a view that adds the descriptor (`sourceView{ SourceRow; SyncModel }`). The DB model is untouched.
- **Files-tab banner** — a shared `_sync_model_banner` partial, rendered above the existing `_files_status_panel`, fed by `source.SyncModelFor(<type>)` from the per-tab handlers. Glyph maps from `Direction` in the template (view concern). Carbon-style quiet read-only tone; Boox lifted to a muted attention accent.
- **Settings / Devices group** reuses the *same* banner partial and descriptor (see Part B-IA).

### Part B-IA — Settings information architecture

The single `GET /settings` page becomes four deep-linkable group routes that ride the app's existing HTMX sidebar idiom (`hx-get` → `#main-content`, `hx-push-url`, `activeTab` highlighting — already used for the Supernote/Boox nav groups):

- `GET /settings` → redirect `/settings/devices`
- `GET /settings/{devices|ai|integrations|system}` → renders only that group's cards

Group taxonomy:

| Group (route) | Contents |
|---|---|
| **Devices** `/settings/devices` | Sources · Supernote (settings + UB-as-SPC server) · ForestNote (settings + Sync Devices) · Boox (settings + DB maintenance) |
| **AI & Processing** `/settings/ai` | OCR · RAG Search · AI Chat |
| **Integrations** `/settings/integrations` | MCP (Server + Connection + Tokens, one section) · CalDAV |
| **System** `/settings/system` | Authentication · Debugging |

The **Devices group** renders a Sources card followed by a uniform per-source section via a shared `_device_source_section` partial: **SyncModel banner → configuration → device-management slot**. The device-management slot is populated *only when the descriptor says a registry exists*, and is always source-scoped:

- ForestNote → existing Sync Devices list (name / last-seen / prune / compact).
- Supernote → reserved slot for the future `spc_devices` registry (informational identity; no cursor to prune); rendered "coming" until that work lands. Never merged with ForestNote.
- Boox → no list; the slot states "No device registry — receive-only," which the `⬇` banner already explains.

This makes the descriptor → device-list relationship a single rule: `Direction`/authority decides whether a list appears and what it can do.

## Existing Patterns

Investigation grounded this design in current code; it follows existing patterns throughout.

- **Source abstraction** — `internal/source` exposes `Source{Type/Name/Start/Stop}`, `SourceRow`, and a registry. `SyncModelFor` is a pure function keyed on `Type`, consistent with the package's role as the platform-neutral source layer. Follows house-style FCIS: the descriptor lookup is functional core; encoding/rendering is the imperative shell.
- **Sources API** — `internal/web/sources_api.go` `handleListSources` currently `json.Encode`s `[]source.SourceRow`. The `sourceView` wrapper is the minimal additive change; tests decode the response, so the additive field is backward-safe.
- **Files-tab handlers** — `internal/web/handler.go` builds `data["pipelinePanel"] = pipelinePanel{Source: "...", StartStop: ...}` then `renderTemplate(..., "files_<src>", data)`, with shared partials (`_files_status_panel`). The banner follows the same data-into-shared-partial pattern.
- **Sidebar nav idiom** — `internal/web/templates/layout.html` already implements grouped left-rail sub-navigation: `nav-group-label` + `nav-sub` items, each `hx-get` → `#main-content` with `hx-push-url="true"`, highlighted by `activeTab` (used today for Supernote and Boox groups). The Settings split reuses this verbatim — no new nav machinery.
- **Settings serving** — `GET /settings` → `handleSettings` → `renderTemplate(w, r, "settings", ...)`. Mutations currently redirect to `/settings#sync-devices`, a hash-anchor scroll hack that exists *because* the page is too long; the route split removes it.
- **Applied feedback patterns** — `feedback_api_first_design` (descriptor on `/api/sources`, not just HTML) and `feedback_unicode_icons` (banner glyphs are Unicode, not an icon library). The existing status-panel `▶/⏹` buttons are out of scope but noted as a future cleanup.

## Implementation Phases

<!-- START_PHASE_1 -->
### Phase 1: SyncModel descriptor (functional core)
**Goal:** Sync semantics become a typed, testable domain concept.

**Components:**
- `internal/source/syncmodel.go` — `SyncModel` struct, `Direction` enum + JSON marshaling, `SyncModelFor(type)` exhaustive over the three types with an `Unmanaged` fallback.

**Dependencies:** None.

**Done when:** A table test pins all three descriptors and the unknown-type fallback. Covers AC1.1–AC1.4.
<!-- END_PHASE_1 -->

<!-- START_PHASE_2 -->
### Phase 2: Expose `sync_model` on the sources API
**Goal:** API-first surfacing of the descriptor.

**Components:**
- `internal/web/sources_api.go` — `sourceView{ source.SourceRow; SyncModel source.SyncModel }`; `handleListSources` encodes the view.

**Dependencies:** Phase 1.

**Done when:** An API test asserts `GET /api/sources` returns `sync_model` with correct values per type, and the existing `SourceRow` fields remain. Covers AC2.1–AC2.2, AC7.1.
<!-- END_PHASE_2 -->

<!-- START_PHASE_3 -->
### Phase 3: Files-tab sync-model banner
**Goal:** The semantics become visible per source on the Files tabs.

**Components:**
- `internal/web/templates/_sync_model_banner.html` — shared partial; glyph from `Direction`, quiet/attention tone.
- `internal/web/handler.go` — each Files-tab handler adds `data["syncModel"] = source.SyncModelFor(<type>)`; templates render the partial above `_files_status_panel`.

**Dependencies:** Phase 1.

**Done when:** The partials smoke test renders the banner for each source with the right glyph/tone/blurb. Covers AC3.1–AC3.4, AC7.2.
<!-- END_PHASE_3 -->

<!-- START_PHASE_4 -->
### Phase 4: Settings routing + sidebar sub-nav shell
**Goal:** Settings becomes four deep-linkable groups; nav reflects them.

**Components:**
- `internal/web/handler.go` — `GET /settings` redirect; `GET /settings/{group}` dispatch in `handleSettings`; mutation redirects retargeted to `/settings/devices`.
- `internal/web/templates/layout.html` — "Settings" `nav-group-label` + four `nav-sub` items; `activeTab` values (`settings-devices`, …).

**Dependencies:** None (independent of Phases 1–3).

**Done when:** Each group route renders into `#main-content`, is bookmarkable, and is highlighted; unknown group paths fall back cleanly; existing settings still save. Covers AC4.1–AC4.5.
<!-- END_PHASE_4 -->

<!-- START_PHASE_5 -->
### Phase 5: Regroup cards + consolidations
**Goal:** Cards land in the right groups; smells removed.

**Components:**
- `internal/web/templates/settings.html` split into per-group templates/partials following the taxonomy table.
- Merge the three MCP cards into one section; dissolve both "General" cards; relocate OCR→AI, CalDAV→Integrations, Auth+Debugging→System.

**Dependencies:** Phase 4.

**Done when:** Every former setting appears under its correct group and still loads/saves; no duplicate "General"; MCP is one section. Covers AC5.1–AC5.3.
<!-- END_PHASE_5 -->

<!-- START_PHASE_6 -->
### Phase 6: Devices group uniform per-source sections
**Goal:** Each source gets an identical banner → config → device-management layout; device lists are source-scoped and gated by the descriptor.

**Components:**
- `internal/web/templates/_device_source_section.html` — shared partial composing the SyncModel banner (reuse Phase 3 partial) + the source's config card + a device-management slot.
- Devices-group template — Sources card, then one section per source; ForestNote → existing Sync Devices list; Boox → "no registry" note; Supernote → reserved `spc_devices` placeholder.

**Dependencies:** Phase 3 (banner partial), Phase 5 (group structure).

**Done when:** Each source renders the uniform section; ForestNote device controls work; Boox shows the no-registry note; Supernote shows the reserved slot, never merged with ForestNote. Covers AC6.1–AC6.5.
<!-- END_PHASE_6 -->

## Additional Considerations

**Forward compatibility.** The Supernote device-management slot is a deliberate reserved seam for the queued `spc_devices` work (persist `equipmentNo` at SPC login). That work stays a *separate* service + surface from ForestNote's — different protocols, unrelated permission models (decision recorded 2026-06-11). The IA already leaves its slot.

**Descriptor describes behavior, not engine.** ForestNote's user-facing semantics (two-way, recoverable tombstones) are unchanged by its internal move to the RhizomeSync engine. `SyncModel` documents what the user observes, so the RhizomeSync swap does not touch it.

**Out of scope (intentional).** Banners and the Devices view *communicate*; they add no reconciliation tooling (no bulk-prune of stale Boox notes, etc.). That was an explicit scoping decision during brainstorming.

**Granularity.** The sync-model signal is per-source only; the converged per-item file grid is left untouched. Per-item provenance/staleness badges were considered and declined to avoid noise.
