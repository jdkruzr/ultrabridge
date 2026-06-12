# Human Test Plan — Source Sync-Semantics Surfacing + Settings IA

Implementation plan: `docs/implementation-plans/2026-06-12-sync-model-and-settings-ia/`
Branch: `sync-model-and-settings-ia`. All 24 acceptance criteria have passing automated
tests (see `test-requirements.md` in the plan directory); the items below cover rendering
fidelity and live-device behavior that unit/integration tests can only approximate.

## What automated tests already cover

- `internal/source/syncmodel_test.go` — descriptor contents, wire encoding (incl. round-trip), Unmanaged fallback, Boox-only `DeletesPropagate=false` (AC1).
- `internal/web/sources_api_test.go::TestListSourcesSyncModel` — `sync_model` on `GET /api/sources`, backward-compat decode (AC2, AC7.1).
- `internal/web/partials_smoke_test.go::TestSyncModelBanner` — glyph/tone/blurb per source, no icon-library markup (AC3, AC7.2).
- `internal/web/routes_test.go`, `settings_routing_test.go` — `/settings` 303, group routes, HX fragment vs full page, nav highlight, bogus-group fallback, mutation redirect targets (AC4).
- `internal/web/settings_groups_test.go` — group card membership, MCP consolidation, scoped save round-trips incl. the secret-omission guard (AC5).
- `internal/web/settings_devices_test.go` — uniform per-source sections, slot gating (AC6).

## Human verification

### 1. AC3.3 — banner tone (visual)

In a browser, open each Files tab (Supernote, Boox, ForestNote). Confirm:
- The Boox banner reads as a **muted attention accent** (amber `#c97` left border) — visually distinct from the quiet Supernote/ForestNote banners AND from any red error state.
- Check in both light and dark color schemes.

### 2. AC4.2 — deep-link + push-url (browser)

- Click each Settings sub-nav item (Devices / AI & Processing / Integrations / System); confirm the content swaps into `#main-content` **without a full reload** and the address bar updates.
- Hard-reload `/settings/integrations` and open it from a bookmark; confirm it lands directly on that group.
- Use browser Back/Forward across groups; confirm history navigates correctly.

### 3. AC6.3 — ForestNote device controls against a live device

With a real synced ForestNote device:
- Confirm the device appears in Settings → Devices → ForestNote → Device management with a correct last-seen timestamp.
- Prune it; confirm it disappears, then re-registers on the device's next sync.
- Run "Compact Relay Log"; confirm the green flash reports the pass results.

### 4. AC5.3 — regression sweep: every setting still saves

Walk all four groups; in each card change one field, save, reload, and confirm persistence (and the restart banner where expected):
- **Devices:** Supernote JIIX toggle/OCR prompt; UB-as-SPC listen address (restart banner); ForestNote sync batch limit (restart banner) + compaction checkbox; Boox OCR prompt/to-do toggle.
- **AI & Processing:** an OCR field (and confirm a *blank* API-key field does NOT wipe a stored key); RAG embed toggle; chat model.
- **System:** username (auth save); verbose API logging.
- **Integrations:** MCP port; CalDAV collection name (restart banner); create + revoke an MCP token (one-time display must show after create, on the Integrations page).

Confirms no field was lost or mis-wired in the split beyond the sampled automated round-trips.
