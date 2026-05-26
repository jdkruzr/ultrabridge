# PROPOSED v1 schema amendment — folder + per-page template sync

**Status:** proposal, awaiting UB-side implementation
**Author:** ForestNote client session (2026-05-26)
**Affects:** `internal/syncstore/op.go`, `internal/syncstore/schema.go`,
`docs/sync/forestnote-sync-protocol.md` (§3.1, §6), `docs/sync/vectors/`
**Companion:** ForestNote client plan (`~/.claude/plans/sync-for-forestnote-via-linear-kurzweil.md`, §A)

## Why

The frozen v1 schema syncs `notebook`, `page`, `stroke` only. Two things ForestNote users
expect to survive across devices are **not** currently synced:

1. **Folders** — ForestNote organizes notebooks into a nested folder tree (`folder` table +
   `notebook.folder_id`). v1 has no `folder` table and no `folder_id`, so folder hierarchy and
   notebook placement are device-local. **Should sync.**
2. **Per-page templates** — `page.template` (ruled/grid/blank/…) + `page.template_pitch_mm`.
   Not in v1's `page` columns. **Should sync.**

This is an **additive** change (new table + new columns). Merge semantics are unchanged
(row-level LWW, full-row upsert), so `protocol_version` **stays 1** — only `schema_hash`
changes. Since **no client has shipped yet**, fold this into v1 in place; the old-hash → `409`
path is moot.

## New canonical schema string

Table order `folder, notebook, page, stroke`; columns alphabetical within each table
(per spec §6):

```
folder:created_at,deleted_at,name,parent_folder_id,sort_order;notebook:created_at,deleted_at,folder_id,name,sort_order;page:created_at,deleted_at,notebook_id,sort_order,template,template_pitch_mm;stroke:color,created_at,deleted_at,page_id,pen_width_max,pen_width_min,points,z
```

## New schema_hash

```
9b807dc88cd0465d171892bb17e65ad94190eda058594e207caad3368eb1f2fe
```

Reproduce (no trailing newline):

```sh
printf '%s' 'folder:created_at,deleted_at,name,parent_folder_id,sort_order;notebook:created_at,deleted_at,folder_id,name,sort_order;page:created_at,deleted_at,notebook_id,sort_order,template,template_pitch_mm;stroke:color,created_at,deleted_at,page_id,pen_width_max,pen_width_min,points,z' | sha256sum
```

(The same method reproduces the current published hash `0df009c5…b83cd2` from the current
string — verified — so this new value is trustworthy.)

## Per-table `cols` (spec §3.1)

### `folder` — NEW synced table
| col | type | notes |
|---|---|---|
| `name` | string | |
| `sort_order` | int64 | |
| `created_at` | int64 ms UTC | |
| `deleted_at` | int64 ms UTC \| null | `null` = live |
| `parent_folder_id` | string (ULID) \| null | `null` = root-level folder |

Not synced (ForestNote-local): `modified_at`, `deleted_batch_id`, `deleted_root_id`.
(Per-row `deleted_at` makes delete/restore converge; the recycle-bin *batch grouping* is a
local UX concern that deliberately does not replicate.)

### `notebook` — add one column
| col | type | notes |
|---|---|---|
| `folder_id` | string (ULID) \| null | `null` = root (no folder) |

(existing: `name`, `sort_order`, `created_at`, `deleted_at`)

### `page` — add two columns
| col | type | notes |
|---|---|---|
| `template` | string \| null | PageTemplate name; `null` = inherit `Settings.defaultTemplate` |
| `template_pitch_mm` | int64 \| null | `null` = inherit `Settings.defaultPitchMm` |

(existing: `notebook_id`, `sort_order`, `created_at`, `deleted_at`)

### `stroke` — unchanged

## Code touch-points (Go)

**`internal/syncstore/op.go`** — `knownCols` + `tableOrder`:
```go
var knownCols = map[string][]string{
    "folder":   {"created_at", "deleted_at", "name", "parent_folder_id", "sort_order"},
    "notebook": {"created_at", "deleted_at", "folder_id", "name", "sort_order"},
    "page":     {"created_at", "deleted_at", "notebook_id", "sort_order", "template", "template_pitch_mm"},
    "stroke":   {"color", "created_at", "deleted_at", "page_id", "pen_width_max", "pen_width_min", "points", "z"},
}
var tableOrder = []string{"folder", "notebook", "page", "stroke"}
```
`canonicalSchema()` / `SchemaHash()` then produce the string + hash above automatically.

**`internal/syncstore/schema.go`** — idempotent migrations (match the existing
`pragma_table_info`-guarded `ALTER TABLE ADD COLUMN` style):
```sql
CREATE TABLE IF NOT EXISTS fn_folder (
    id               TEXT PRIMARY KEY,
    name             TEXT,
    sort_order       INTEGER,
    created_at       INTEGER,
    deleted_at       INTEGER,
    parent_folder_id TEXT,
    lww_wall_ts      INTEGER NOT NULL,
    lww_op_seq       INTEGER NOT NULL,
    lww_site_id      TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_fn_folder_parent ON fn_folder(parent_folder_id);
-- fn_notebook: ADD COLUMN folder_id TEXT
-- fn_notebook: CREATE INDEX idx_fn_notebook_folder ON fn_notebook(folder_id)
-- fn_page:     ADD COLUMN template TEXT
-- fn_page:     ADD COLUMN template_pitch_mm INTEGER
```
`fn_folder` carries the same `lww_*` provenance triple as the other mirror tables — uniform
LWW rule, no special-casing. `parent_folder_id` is a plain LWW column (moves resolve by
greatest key); the mirror has no FK enforcement, so apply order is irrelevant.

**Docs:** update spec §3.1 (the three column tables) and §6 (canonical string + hash).

**Tests:** update the published-hash assertion to `9b807dc8…f2fe`.

## New conformance vectors (add to `docs/sync/vectors/`, mirror to ForestNote)

The merge rule is unchanged, so these just exercise the new tables/columns under the existing
LWW semantics:
- `folder-single` — one folder op materializes.
- `folder-delete-restore` — `deleted_at` LWW on a folder (mirror of the notebook case).
- `folder-move` — `parent_folder_id` resolves by greatest key (re-parenting).
- `notebook-move-folder` — `folder_id` LWW (notebook moved between folders / to root).
- `page-template` — `template` + `template_pitch_mm` set then cleared, LWW.

Run `python3 docs/sync/vectors/_oracle.py docs/sync/vectors/*.vector.json` after adding.

## Report back

Once implemented, confirm the live `schema_hash` equals `9b807dc88cd0465d171892bb17e65ad94190eda058594e207caad3368eb1f2fe`.
That unblocks **Phase 6** of the ForestNote client plan (folder + template sync).
