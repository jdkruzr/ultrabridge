# Source Sync-Semantics Surfacing + Settings IA — Implementation Plan

**Goal:** Expose the `SyncModel` descriptor on the `/api/sources` representation (API-first), without changing the persisted `SourceRow` shape.

**Architecture:** A thin view type (`sourceView`) embeds `source.SourceRow` and adds a derived `sync_model` object. `handleListSources` maps each persisted row through `source.SyncModelFor(row.Type)`. Embedding promotes the row's existing JSON fields to top level, so every current `[]source.SourceRow` decoder keeps working.

**Tech Stack:** Go (`encoding/json`), existing `internal/web` handler + `httptest` test idiom (`sources_api_test.go`).

**Scope:** Phase 2 of 6.

**Codebase verified:** 2026-06-12. `internal/web/sources_api.go` `handleListSources` does `json.NewEncoder(w).Encode(sources)` where `sources` is `[]source.SourceRow` from `h.config.ListSources(ctx)`. `sources_api_test.go` decodes responses into `[]source.SourceRow` (TestListSourcesEmpty, TestAddSourceSucceeds, TestUpdateSourceSucceeds, TestDeleteSourceSucceeds) — these must keep passing unchanged, which the embedding guarantees.

---

## Acceptance Criteria Coverage

This phase implements and tests:

### sync-model-and-settings-ia.AC2: API exposes the descriptor
- **sync-model-and-settings-ia.AC2.1 Success:** `GET /api/sources` includes a `sync_model` object per source with the five fields.
- **sync-model-and-settings-ia.AC2.2 Success:** The persisted `SourceRow` shape is unchanged (descriptor is additive, view-only).

### sync-model-and-settings-ia.AC7: Cross-cutting
- **sync-model-and-settings-ia.AC7.1 Success:** The `sync_model` descriptor has exactly one definition consumed by both the API and the HTML surfaces (no duplicated source of truth).

---

**Dependencies:** Phase 1 (`source.SyncModelFor`).

<!-- START_SUBCOMPONENT_A (tasks 1-2) -->
<!-- START_TASK_1 -->
### Task 1: sourceView wrapper on the sources list endpoint

**Files:**
- Modify: `internal/web/sources_api.go` — add `sourceView` type; rewrite `handleListSources` body (currently lines 14–24) to encode `[]sourceView`.

**Implementation:**

Add the view type and map rows through `source.SyncModelFor`. Per AC7.1, this reuses the Phase-1 function — no descriptor data is duplicated here.

```go
// sourceView is the API representation of a source: the persisted row plus its
// derived, view-only sync model. The embedded SourceRow promotes its JSON
// fields to top level, so existing []source.SourceRow decoders keep working —
// the added sync_model field is simply ignored by them (AC2.2).
type sourceView struct {
	source.SourceRow
	SyncModel source.SyncModel `json:"sync_model"`
}
```

In `handleListSources`, after loading `sources`, build the view slice and encode it instead of the raw rows:

```go
views := make([]sourceView, len(sources))
for i, row := range sources {
	views[i] = sourceView{SourceRow: row, SyncModel: source.SyncModelFor(row.Type)}
}
w.Header().Set("Content-Type", "application/json")
json.NewEncoder(w).Encode(views)
```

Leave `handleAddSource`, `handleUpdateSource`, `handleDeleteSource` untouched — they decode/return raw rows and status maps, not views.

**Verification:**

Run: `go build -C /home/sysop/src/ultrabridge ./internal/web/`
Expected: builds without errors.

Run: `go test -C /home/sysop/src/ultrabridge ./internal/web/ -run 'TestListSources|TestAddSource|TestUpdateSource|TestDeleteSource'`
Expected: existing sources tests still pass (proves AC2.2 — the embedded fields decode into `[]source.SourceRow` unchanged).

**Commit:** `feat(web): expose sync_model on GET /api/sources via sourceView`
<!-- END_TASK_1 -->

<!-- START_TASK_2 -->
### Task 2: sync_model API test

**Verifies:** sync-model-and-settings-ia.AC2.1, AC2.2, AC7.1

**Files:**
- Modify: `internal/web/sources_api_test.go` — add one test (reuse `initSourceTestDB` + `setupTestHandler`).

**Testing:**

Seed one source of each type (`supernote`, `boox`, `forestnote`) via `source.AddSource`, call `h.handleListSources` against `GET /api/sources`, then:

- **AC2.1:** Decode the body into a local struct mirroring `sourceView` (embedded `source.SourceRow` + `SyncModel source.SyncModel` with tag `sync_model`). Assert each element's `SyncModel` equals `source.SyncModelFor(elem.Type)` — i.e. the boox row carries `Receive-only` / `deletes_propagate:false`, the two-way rows carry their labels. Also decode into a `[]map[string]json.RawMessage` and assert the `sync_model` key is present and non-empty for each.
- **AC2.2:** Decode the *same* body into `[]source.SourceRow` and assert the persisted fields (Type, Name, Enabled, ConfigJSON) survive intact — proving the added field is additive and backward-safe.
- **AC7.1:** The assertion that `view.SyncModel == source.SyncModelFor(view.Type)` is itself the proof that the API derives from the single Phase-1 definition rather than a duplicate. No separate test needed; note it in a comment.

**Verification:**

Run: `go test -C /home/sysop/src/ultrabridge ./internal/web/ -run TestListSourcesSyncModel`
Expected: passes.

**Commit:** `test(web): assert /api/sources carries sync_model per source type`
<!-- END_TASK_2 -->
<!-- END_SUBCOMPONENT_A -->
