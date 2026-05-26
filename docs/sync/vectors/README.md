# Sync Conformance Vectors — v1

These JSON vectors are the **ironclad cross-language contract** for the ForestNote ↔
UltraBridge sync merge. Both the Go server and the Kotlin client load every `*.vector.json`
file here, run its `ops` through their merge function, and assert the result equals
`expected_state`. A vector that is green in one language and red in the other means the
implementations disagree — a release-blocking bug.

Protocol spec: `../forestnote-sync-protocol.md`.

This directory is mirrored verbatim into the ForestNote repo. **Edit here; mirror there.**

## Vector format

```jsonc
{
  "name": "<unique kebab-case id>",
  "description": "<what property this pins down>",
  "ops": [ Op, ... ],            // fed to the merge function, in the given order
  "expected_state": {            // the materialized mirror after merging ALL ops
    "folder":   [ Row, ... ],    // a table with no winning rows may be omitted
    "notebook": [ Row, ... ],
    "page":     [ Row, ... ],
    "stroke":   [ Row, ... ]
  }
}
```

An `Op` is exactly the wire op (spec §3):

```jsonc
{ "table", "pk", "site_id", "op_seq", "wall_ts", "cols": { ... } }
```

A `Row` in `expected_state` is the **winning op** for that `(table, pk)`, with `table`
dropped (it is implied by the array it sits in):

```jsonc
{ "pk", "site_id", "op_seq", "wall_ts", "cols": { ...known columns only } }
```

i.e. an expected row carries both the resulting `cols` **and** the provenance triple
`(wall_ts, op_seq, site_id)` of the op that won. Asserting provenance — not just `cols` —
catches tiebreak bugs even when two ops would produce identical column values.

## The merge function under test (spec §5, restated executably)

```
merge(ops) -> state:
  winners = {}                      # (table, pk) -> op
  for op in ops:
    op = normalize(op)              # drop cols keys not in the known set for op.table
    cur = winners.get((op.table, op.pk))
    if cur is None or key(op) > key(cur):
      winners[(op.table, op.pk)] = op
  # group winners by table; each becomes a Row (drop `table`)
  return group_by_table(winners)

key(op) = (op.wall_ts, op.op_seq, op.site_id)   # lexicographic; site_id = ASCII string cmp
```

Comparison of `expected_state` is **order-independent**: within each table, match rows by
`pk` as a set. Deleted rows (`cols.deleted_at != null`) **are** part of the state and appear
in `expected_state` — filtering deleted rows is a query concern, not a merge concern (§5.3).

Duplicate ops (identical `(site_id, op_seq)`) collapse under the merge tie rule
(`key(dup) == key(orig)`, no `>` so no overwrite), so the pure merge converges without an
explicit dedup step; the server's changelog dedup (§5.3) is a separate, additive concern not
exercised here.

## Reference oracle

`_oracle.py` is a ~40-line Python implementation of the merge above. It is **not** part of
the contract (the contract is this spec + these JSON files); it is a dev aid that validates
every vector is internally self-consistent:

```bash
python3 docs/sync/vectors/_oracle.py docs/sync/vectors/*.vector.json
```

Run it after editing any vector. CI for the real implementations runs the vectors in Go and
Kotlin, not Python.

## Coverage

| Vector | Property pinned |
|---|---|
| `single-op` | one op materializes to one row |
| `lww-wall-ts` | higher `wall_ts` wins |
| `lww-op-seq` | equal `wall_ts` → higher `op_seq` wins |
| `lww-site-id` | equal `wall_ts` + `op_seq` → greater `site_id` (ASCII) wins |
| `clock-skew` | `wall_ts` dominates `op_seq` (lower `wall_ts` loses despite higher `op_seq`) |
| `delete-then-restore` | `deleted_at` is an LWW column; latest writer → live |
| `delete-restore-delete` | latest writer → deleted; restore that loses on key stays lost |
| `stroke-union` | distinct stroke pks all survive (no same-pk collision) |
| `shuffled-order` | same op set, shuffled, converges to one state (convergence) |
| `duplicate-ops` | duplicated ops converge to one state (idempotence) |
| `unknown-columns-ignored` | unknown `cols` keys dropped on materialize (forward-compat) |
| `multi-table` | notebook + page + strokes, one erased, full mirror incl. deleted row |
| `folder-single` | one folder op materializes to one live root-level folder |
| `folder-delete-restore` | `deleted_at` LWW on a folder (mirror of the notebook case) |
| `folder-move` | `parent_folder_id` resolves by greatest key (re-parenting) |
| `notebook-move-folder` | `folder_id` LWW (notebook moved between folders / to root) |
| `page-template` | `template` + `template_pitch_mm` set then cleared, LWW |

**Out of scope for vectors:** transport/envelope error handling, per-op `rejected` semantics,
`accepted_through` contiguity, idempotent resend, and cursor reconciliation are *protocol*
behaviors (spec §7), not pure-merge properties. They are covered by each implementation's
HTTP/integration tests, not by these vectors.
