# Source Sync-Semantics Surfacing + Settings IA — Implementation Plan

**Goal:** Give the Devices group a uniform per-source layout — heading → sync-model banner → configuration → device-management slot — where the slot's content is gated by the source's sync model: a real list for ForestNote, a reserved placeholder for Supernote, a "no registry" note for Boox.

**Architecture:** `settings_devices.html` (a flat card stack after Phase 5) becomes a repeated section frame, one per source type. The Phase-3 `_sync_model_banner` partial is reused; the per-source banner model is supplied to the template by a new `syncModelFor` template func (wrapping `source.SyncModelFor`). The device-management slot is three small `{{define}}` blocks in a new `_device_source_section.html` partial; each section renders the one matching its type. The descriptor → device-list rule is a single decision: a source with a real registry (ForestNote) gets the list; one with a reserved future registry (Supernote → `spc_devices`) gets a "coming" placeholder; a receive-only source (Boox) gets "no registry". Supernote and ForestNote device management stay separate surfaces — never merged.

**Tech Stack:** Go `html/template` (`FuncMap`, `{{define}}`/`{{template}}` with constant names, `$` root access), existing settings handler data.

**Scope:** Phase 6 of 6. Depends on Phase 3 (banner partial) and Phase 5 (devices group has the cards).

**Codebase verified:** 2026-06-12.
- FuncMap is assembled in `NewHandler` and passed to `template.New("").Funcs(funcMap)` (handler.go ~:230–284) — the registration point for `syncModelFor`.
- The Sync Devices list + prune/compact controls live in the card moved to `settings_devices.html` in Phase 5 (original `settings.html:327–405`), gated on `.SyncDevicesEnabled`/`.SyncDevices`; prune posts `/settings/sync-devices/prune`, compact posts `/settings/sync-devices/compact` (routes unchanged, already tested).
- Source visibility flags available in `settingsData`: `.SNPipelineActive`, `.BooxActive`, `.ForestNoteSourceActive`, `.SyncDevicesEnabled`, `.Config`.
- Inside the device sections, config markup references root-level keys (`.Config.*`, `.BooxOCRPrompt`, `.SyncDevices`, …); rendered at root scope (no `{{range}}` shadowing) so those references keep resolving.

---

## Acceptance Criteria Coverage

This phase implements and tests:

### sync-model-and-settings-ia.AC6: Devices group per-source uniformity
- **sync-model-and-settings-ia.AC6.1 Success:** The Devices group opens with a Sources card, then one structurally identical section per configured source.
- **sync-model-and-settings-ia.AC6.2 Success:** Each source section renders the SyncModel banner (same partial as the Files tab), then configuration, then a device-management slot.
- **sync-model-and-settings-ia.AC6.3 Success:** ForestNote's section shows its device list (name, last-seen, prune, compact); the controls work.
- **sync-model-and-settings-ia.AC6.4 Success:** Boox's device slot shows a "No device registry — receive-only" note and no list.
- **sync-model-and-settings-ia.AC6.5 Success:** Supernote's device slot renders a reserved "coming" placeholder (the future `spc_devices` surface), never merged with ForestNote's.

---

**Dependencies:** Phase 3, Phase 5.

<!-- START_TASK_1 -->
### Task 1: `syncModelFor` template func

**Files:**
- Modify: `internal/web/handler.go` (add to the `funcMap` in `NewHandler`)

**Implementation:**

```go
"syncModelFor": source.SyncModelFor,
```

This lets templates fetch a source's banner model by type: `{{template "_sync_model_banner" (syncModelFor "boox")}}`.

**Verification:**
Run: `go build -C /home/sysop/src/ultrabridge ./internal/web/`
Expected: builds.

**Commit:** `feat(web): expose syncModelFor template func`
<!-- END_TASK_1 -->

<!-- START_TASK_2 -->
### Task 2: Device-management slot partials

**Files:**
- Create: `internal/web/templates/_device_source_section.html`

**Implementation:**

Three slot blocks, each rendered at root scope (`.` = page data), so the ForestNote slot can reach `.SyncDevices`/`.SyncCompactResult`/`.SyncDevicesEnabled`:

```html
{{define "_device_mgmt_forestnote"}}
  {{/* Real registry: the ForestNote Sync Devices list + prune/compact.
       Moved verbatim from the Phase-5 Sync Devices card (the table,
       SyncCompactResult flash, prune forms, and Compact Relay Log form). */}}
  … existing Sync Devices markup (gated on .SyncDevicesEnabled; "No devices
    have synced yet." when .SyncDevices is empty) …
{{end}}

{{define "_device_mgmt_supernote"}}
  {{/* Reserved slot for the future spc_devices registry — never merged with
       ForestNote's list (separate protocols/permissions, decision 2026-06-11). */}}
  <p class="device-info">Per-device identity for synced Supernote hardware is coming
    (a dedicated <code>spc_devices</code> registry). It will appear here, separate from
    ForestNote's devices.</p>
{{end}}

{{define "_device_mgmt_boox"}}
  {{/* Receive-only: no registry by design — the ⬇ banner already explains why. */}}
  <p class="device-info">No device registry — receive-only. Boox exports notes one way,
    so there are no devices to manage here.</p>
{{end}}
```

**Verification:** covered by Task 4.

**Commit:** `feat(web): add per-source device-management slot partials`
<!-- END_TASK_2 -->

<!-- START_TASK_3 -->
### Task 3: Rebuild settings_devices.html as uniform per-source sections

**Files:**
- Modify: `internal/web/templates/settings_devices.html`

**Implementation:**

Keep the restart banner and Sources card at top. Then render one section per source type, each following the identical frame (heading → banner → Configuration → Device management). Preserve the existing visibility gates so behavior is unchanged on partial deployments.

```html
{{template "_settings_restart_banner" .}}
{{if .Config}}… Sources card (unchanged) …{{end}}

{{/* Supernote */}}
{{if or .SNPipelineActive .Config}}
<section class="device-source">
  <h2>Supernote</h2>
  {{template "_sync_model_banner" (syncModelFor "supernote")}}
  <h3>Configuration</h3>
  … Supernote Settings card body + UB-as-SPC Device Sync Server card body …
  <h3>Device management</h3>
  {{template "_device_mgmt_supernote" .}}
</section>
{{end}}

{{/* ForestNote */}}
{{if .Config}}
<section class="device-source">
  <h2>ForestNote</h2>
  {{template "_sync_model_banner" (syncModelFor "forestnote")}}
  <h3>Configuration</h3>
  … ForestNote Device Sync card body …
  <h3>Device management</h3>
  {{template "_device_mgmt_forestnote" .}}
</section>
{{end}}

{{/* Boox */}}
<section class="device-source{{if not .BooxActive}} settings-inactive{{end}}">
  <h2>Boox</h2>
  {{template "_sync_model_banner" (syncModelFor "boox")}}
  <h3>Configuration</h3>
  … Boox Settings card body + Boox Database Maintenance card body …
  <h3>Device management</h3>
  {{template "_device_mgmt_boox" .}}
</section>
```

Notes:
- The Sync Devices markup that was a standalone card in Phase 5 now lives inside `_device_mgmt_forestnote` — remove the duplicate standalone card so it appears once, under ForestNote (AC6.5's "never merged" plus no duplication).
- The banner reuses the same partial the Files tabs use (AC6.2).
- Each `<section>` has the same skeleton (`h2` → banner → `Configuration` → `Device management`), satisfying "structurally identical" (AC6.1).

**Verification:**
Run: `go build -C /home/sysop/src/ultrabridge ./internal/web/`
Expected: builds.

**Commit:** `feat(web): uniform per-source sections in the Devices settings group`
<!-- END_TASK_3 -->

<!-- START_TASK_4 -->
### Task 4: Devices-group section tests

**Verifies:** sync-model-and-settings-ia.AC6.1, AC6.2, AC6.3, AC6.4, AC6.5

**Files:**
- Add: `internal/web/settings_devices_test.go`
- Optionally extend: `internal/web/partials_smoke_test.go` for the slot partials

**Testing:**

Render `GET /settings/devices` (non-HX) against a `Handler` with a wired `SyncDeviceService` mock returning at least one device, plus `Config` set and Boox active:

- **AC6.1:** Assert the response opens with the Sources card and contains three `<section class="device-source"` blocks (Supernote, ForestNote, Boox), each carrying an `<h2>` of the source name and the two `<h3>` subheads `Configuration` and `Device management`. Assert the three sections share the same skeleton (each has exactly one banner + the two subheads).
- **AC6.2:** Assert each section contains a `sync-model-banner` element; Supernote/ForestNote carry `⇅`, Boox carries `⬇` (same partial as the Files tabs — reuse proven by the shared block name).
- **AC6.3:** Within the ForestNote section, assert the device list renders the seeded device's name, a `formatTimestamp`-rendered last-seen, the prune form (`/settings/sync-devices/prune`), and the compact form (`/settings/sync-devices/compact`). The controls are the existing routes (already covered by `sync_devices_test.go`), so this asserts presence/placement, not re-tests the routes.
- **AC6.4:** Within the Boox section, assert `No device registry — receive-only` and that no prune/compact form appears in that section.
- **AC6.5:** Within the Supernote section, assert the reserved `spc_devices` placeholder text and that the section does NOT contain the ForestNote prune control (`/settings/sync-devices/prune`) — i.e. the two surfaces are not merged.
- Slot smoke tests (optional, mirrors `TestSharedPartialsRender`): render `_device_mgmt_boox`/`_device_mgmt_supernote` directly and assert their fixed copy.

**Verification:**
Run: `go test -C /home/sysop/src/ultrabridge ./internal/web/`
Expected: all pass.

**Commit:** `test(web): Devices group uniform sections + descriptor-gated slots`
<!-- END_TASK_4 -->
