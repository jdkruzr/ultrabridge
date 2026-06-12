# Test Requirements — Source Sync-Semantics Surfacing + Settings IA

Maps each acceptance criterion to an automated test (type + expected file) or documented human verification. Slug: `sync-model-and-settings-ia`. Test idioms: Go `testing` table tests for `internal/source`; `html/template` `ExecuteTemplate`-on-`templateFS` smoke tests + `httptest`/`ServeHTTP` handler tests for `internal/web`. No DB/network mocking beyond the existing in-memory SQLite + service mocks.

## Coverage summary

| AC | Verification | Type | File |
|---|---|---|---|
| AC1.1 | Automated | unit | `internal/source/syncmodel_test.go` |
| AC1.2 | Automated | unit | `internal/source/syncmodel_test.go` |
| AC1.3 | Automated | unit | `internal/source/syncmodel_test.go` |
| AC1.4 | Automated | unit | `internal/source/syncmodel_test.go` |
| AC2.1 | Automated | integration | `internal/web/sources_api_test.go` |
| AC2.2 | Automated | integration | `internal/web/sources_api_test.go` |
| AC3.1 | Automated | unit (template) | `internal/web/partials_smoke_test.go` |
| AC3.2 | Automated | unit (template) | `internal/web/partials_smoke_test.go` |
| AC3.3 | Automated + Human | unit (template) / visual | `internal/web/partials_smoke_test.go` + manual |
| AC3.4 | Automated | unit (template) | `internal/web/partials_smoke_test.go` |
| AC4.1 | Automated | integration | `internal/web/routes_test.go` |
| AC4.2 | Automated + Human | integration / browser | `internal/web/settings_routing_test.go` + manual |
| AC4.3 | Automated | integration | `internal/web/settings_routing_test.go` |
| AC4.4 | Automated | integration | `internal/web/sync_devices_test.go` / routing test |
| AC4.5 | Automated | integration | `internal/web/settings_routing_test.go` |
| AC5.1 | Automated | integration | `internal/web/settings_groups_test.go` |
| AC5.2 | Automated | integration | `internal/web/settings_groups_test.go` |
| AC5.3 | Automated | integration | `internal/web/settings_groups_test.go` |
| AC6.1 | Automated | integration | `internal/web/settings_devices_test.go` |
| AC6.2 | Automated | integration | `internal/web/settings_devices_test.go` |
| AC6.3 | Automated + Human | integration / device | `internal/web/settings_devices_test.go` + manual |
| AC6.4 | Automated | integration | `internal/web/settings_devices_test.go` |
| AC6.5 | Automated | integration | `internal/web/settings_devices_test.go` |
| AC7.1 | Automated | integration | `internal/web/sources_api_test.go` |
| AC7.2 | Automated | unit (template) | `internal/web/partials_smoke_test.go` |

## Automated tests — detail

### AC1: typed descriptor (`internal/source/syncmodel_test.go`)
- **AC1.1** — table over `supernote|boox|forestnote`; assert full `SyncModel` equality (Label, Direction, Authority, DeletesPropagate, Blurb) per type.
- **AC1.2** — `json.Marshal(TwoWay)==`"two_way"`, `json.Marshal(OneWayIn)==`"one_way_in"``; and a marshaled `SyncModel` carries `direction:"two_way"` for supernote.
- **AC1.3** — `SyncModelFor("bogus")` and `SyncModelFor("")` both `DeepEqual` `Unmanaged`; `Unmanaged.Label != ""`.
- **AC1.4** — exactly one of the three known descriptors has `DeletesPropagate==false` (boox).

### AC2 / AC7.1: API descriptor (`internal/web/sources_api_test.go`)
- **AC2.1** — seed one source per type; `GET /api/sources`; decode into a `sourceView`-shaped struct; assert each `sync_model` equals `source.SyncModelFor(type)`; assert `sync_model` key present in the raw JSON.
- **AC2.2** — decode the same body into `[]source.SourceRow`; assert persisted fields intact.
- **AC7.1** — the `view.SyncModel == source.SyncModelFor(view.Type)` assertion is the single-source-of-truth proof (no duplicate descriptor data in the web layer).

### AC3 / AC7.2: Files banner (`internal/web/partials_smoke_test.go`)
- **AC3.1/AC3.2** — render `_sync_model_banner` for each type; supernote/forestnote contain `⇅` + their labels; boox contains `⬇` + "Receive-only".
- **AC3.3** — boox output has `sync-model-attention` + `#c97`; supernote/forestnote have `sync-model-quiet` and not `sync-model-attention`; no output contains `var(--status-text-failed)`.
- **AC3.4** — boox blurb contains "never reach UltraBridge".
- **AC7.2** — literal `⇅`/`⬇` runes present; no `<svg`/`<i class=`/icon-font markup.

### AC4: routing (`internal/web/routes_test.go`, `settings_routing_test.go`)
- **AC4.1** — `GET /settings` → 303, `Location: /settings/devices`.
- **AC4.2 (mechanism)** — `GET /settings/ai` non-HX → 200 full page with `id="main-content"`; with `HX-Request: true` → 200 fragment (no `<!DOCTYPE`); nav links carry `hx-push-url="true"`.
- **AC4.3** — each group response marks exactly that group's `nav-sub` link `active`.
- **AC4.4** — `handleSyncDevicePrune`/`handleSyncDeviceCompact` non-HX → `Location: /settings/devices`.
- **AC4.5** — `GET /settings/bogus` → 303 `Location: /settings/devices` (not 500).

### AC5: regroup/consolidation (`internal/web/settings_groups_test.go`)
- **AC5.1 / AC4.2 (content)** — per group, assert its cards present and others absent; no card titled exactly "General".
- **AC5.2** — `/settings/integrations` has a single `<h2>MCP</h2>` card containing Server/Connection/Tokens subsections.
- **AC5.3** — round-trip saves: `POST /settings/save` with `section=ai|integrations|system` each persists its fields via `GetConfig`; a `section=ai` POST leaves `LogVerboseAPI`/`CalDAVCollectionName` unchanged; partial `PUT /api/config` (`{username}`, `{mcp_port}`, `{ocr_model}`) each updates only its field; `ocr_api_key` omitted leaves a set key intact.

### AC6: Devices uniformity (`internal/web/settings_devices_test.go`)
- **AC6.1** — Sources card first; three `<section class="device-source">` (Supernote/ForestNote/Boox) each with one banner + `Configuration` + `Device management` subheads.
- **AC6.2** — each section has a `sync-model-banner`; SN/FN `⇅`, Boox `⬇`.
- **AC6.3** — ForestNote section renders seeded device name, last-seen, prune form, compact form.
- **AC6.4** — Boox section contains "No device registry — receive-only", no prune/compact form.
- **AC6.5** — Supernote section shows the reserved `spc_devices` placeholder and does NOT contain `/settings/sync-devices/prune`.

## Human verification

These complement (do not replace) the automated checks; they cover rendering fidelity and live behavior that unit/integration tests can only approximate.

1. **AC3.3 — banner tone (visual).** In a browser, confirm the Boox Files-tab banner reads as a muted attention accent (amber left border), visually distinct from both the quiet Supernote/ForestNote banners and from any red error state, in both light and dark color schemes.
2. **AC4.2 — deep-link + push-url (browser).** Click each Settings sub-nav item; confirm the content swaps into `#main-content` without a full reload and the address bar updates. Hard-reload and bookmark `/settings/integrations`; confirm it lands directly on that group. Use browser Back/Forward across groups.
3. **AC6.3 — controls against a live ForestNote device.** With a real synced device, confirm the device appears in the ForestNote section with a correct last-seen; prune it and confirm it disappears and re-registers on the device's next sync; run "Compact Relay Log" and confirm the flash result. (Route correctness is unit-tested; this verifies end-to-end against real sync state.)
4. **Regression sweep — every setting still saves (AC5.3, manual pass).** Walk all four groups; change one field in each card, save, reload, confirm persistence (and the restart banner where expected). Confirms no field was lost or mis-wired in the split beyond the sampled automated round-trips.
