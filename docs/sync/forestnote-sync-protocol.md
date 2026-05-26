# ForestNote ↔ UltraBridge Sync Protocol — v1

**Status:** specification (the dual-language contract). Not yet implemented.
**Date:** 2026-05-26
**Design source:** `docs/design-plans/2026-05-25-forestnote-ub-sync.md` (Part A formalized here).
**Decision source:** `docs/research/2026-05-25-sync-decision.md` (why roll-our-own).
**Conformance:** `docs/sync/vectors/` — the JSON suite both implementations run in CI.

> This document is the **source of truth** for the wire protocol and the merge rule.
> Neither the Go server nor the Kotlin client is authoritative; both MUST agree with this
> spec and pass every vector in `docs/sync/vectors/`. Where this document says **MUST**,
> a divergent implementation is a bug.

---

## 1. Roles and model

- **Client** — a ForestNote installation (Kotlin, on a Viwoods device). Authors all ops.
  Holds a local SQLite (SQLDelight) database and an outbound oplog.
- **Server** — one UltraBridge instance (Go). A **relay + mirror**: it ingests client ops,
  materializes a mirror, and relays ops to the user's other devices. **The server never
  authors ForestNote ops.** All ops originate on a device; conflict is only device↔device.
- **Single user per instance.** No tenant key, no row-level security — one UB instance
  serves one user's devices. (Multi-instance hosting is a deployment concern, out of scope.)

The unit of replication is the **row**. Three tables sync: `notebook`, `page`, `stroke`.
Merge is **row-level last-writer-wins (LWW)** under a deterministic total order (§5).

---

## 2. Canonical encodings (MUST match byte-for-byte across languages)

| Concept | Encoding |
|---|---|
| **ULID** | 26-char **uppercase** Crockford base32 (alphabet `0123456789ABCDEFGHJKMNPQRSTVWXYZ`). Used for `pk` and `site_id`. |
| **Timestamp** | `int64`, milliseconds since Unix epoch, UTC. |
| **`op_seq`** | `int64`, per-device monotonic, starts at `1`, `+1` per authored op, never reused, persists across restarts. |
| **`points` BLOB** | little-endian `int32` array, 5 ints per point `[x, y, pressure, tsHi, tsLo]`, then **standard base64** (RFC 4648, with `=` padding) of the raw bytes. Transported as a JSON string. |
| **Integers on the wire** | all `int64` (JSON number, no fraction). See §3 for which columns. |
| **Strings** | UTF-8. |
| **Transport** | JSON over HTTPS, `POST /sync/v1`, behind UB's existing auth (§7). |

### 2.1 ULID comparison

ULIDs are compared as their 26-char ASCII strings, **byte-lexicographically**. Because the
uppercase Crockford alphabet is a strictly increasing subset of ASCII `[0-9A-Z]`, ASCII
string order equals ULID numeric order. Implementations MUST compare the canonical
uppercase string form (never decode to bytes first, never lowercase).

---

## 3. The op model — one op type

**Every op is a full-row UPSERT** carrying the row's complete current column set, including
`deleted_at`. There is **no separate delete op**: deletion sets `deleted_at` to a timestamp,
restoration sets it back to `null`. Both are ordinary upserts resolved by the same LWW rule
(§5). This is what makes recycle-bin **restore** converge.

```jsonc
Op = {
  "table":   "notebook" | "page" | "stroke",
  "pk":      "<ULID>",       // the row's primary key
  "site_id": "<ULID>",       // device that authored this op
  "op_seq":  <int64>,        // per-device monotonic counter
  "wall_ts": <int64 ms UTC>, // device wall clock at authoring time
  "cols":    { ...full row state, including deleted_at }
}
```

The identity of an op is `(site_id, op_seq)` — globally unique, the dedup key (§5.3).

### 3.1 Per-table columns (`cols`)

Column types are fixed. **All numeric columns are `int64`** (no floats on the wire — see
§9 open item if ForestNote stores pen widths as floats).

**`notebook`**
| col | type | notes |
|---|---|---|
| `name` | string | |
| `sort_order` | int64 | |
| `created_at` | int64 ms UTC | |
| `deleted_at` | int64 ms UTC \| null | `null` = live |

**`page`**
| col | type | notes |
|---|---|---|
| `notebook_id` | string (ULID) | parent notebook pk |
| `sort_order` | int64 | |
| `created_at` | int64 ms UTC | |
| `deleted_at` | int64 ms UTC \| null | |

**`stroke`**
| col | type | notes |
|---|---|---|
| `page_id` | string (ULID) | parent page pk |
| `color` | int64 | packed ARGB |
| `pen_width_min` | int64 | device units |
| `pen_width_max` | int64 | device units |
| `points` | string | base64 of LE int32 array (§2) |
| `z` | int64 | stroke ordering within page |
| `created_at` | int64 ms UTC | |
| `deleted_at` | int64 ms UTC \| null | erase = set; un-erase = clear |

### 3.2 Unknown columns are ignored (forward-compat)

When materializing, the server **drops** any key in `cols` not listed for that table. A v2
client may send extra columns; a v1 server ignores them rather than rejecting. The stored
row and the merge therefore consider only the known column set. (Conformance vector:
`unknown-columns-ignored`.)

---

## 4. The sync round-trip — one call batches send + receive

`POST /sync/v1`, JSON request and response:

```jsonc
// Request
{
  "protocol_version": 1,
  "schema_hash": "0df009c588f7d4b663b82861f10565fde7776e50da738bbca2ef174b27b83cd2",
  "site_id": "<ULID>",
  "cursor": <int64>,     // last global seq this device has applied (0 = never synced)
  "ops": [ Op, ... ]     // pending local ops, in op_seq order
}

// Response (200)
{
  "protocol_version": 1,
  "accepted_through": <int64>,  // max op_seq from THIS device durably accepted
  "ops": [ Op, ... ],           // ops with global seq > cursor, EXCLUDING this site_id
  "cursor": <int64>,            // new high-water mark for this device
  "has_more": <bool>            // server capped the batch; call again immediately
}
```

### 4.1 Server obligations

1. Validate envelope: `protocol_version == 1`, `schema_hash` matches (§6) → else error (§7).
2. Apply `ops` in one transaction (§5): per-op validate → dedup → merge → assign global seq
   → append to changelog. Malformed individual ops are skipped + logged; the batch still
   succeeds.
3. `accepted_through` = the greatest `op_seq` from this `site_id` that is now durably in the
   changelog (whether newly applied or already present from a prior call). The device uses
   this to stop resending acked ops.
4. Select relay ops: changelog entries with global `seq > cursor` **and** `site_id !=` the
   requesting device, in ascending `seq`, capped at the batch limit (`SyncBatchLimit`,
   default 500).
5. `cursor` (response) = the global seq of the last relay op returned (or the request cursor
   if none). `has_more` = true iff more entries above that seq exist.

### 4.2 Client loop

```
loop:
  ops := outbox entries with op_seq > acked_through, in op_seq order
  resp := POST /sync/v1 { cursor: localCursor, ops }
  mark outbox acked through resp.accepted_through
  for op in resp.ops: apply(op) idempotently via the single-writer store
  localCursor = resp.cursor
  if resp.has_more: continue
  else: break
```

Applying a relayed op is **idempotent** (same merge rule, §5) so retries and duplicate
delivery are safe. On any transport error, retry with backoff; resends cannot corrupt state.

---

## 5. Conflict resolution — deterministic, both languages MUST agree

### 5.1 The total order

For two ops touching the same `(table, pk)`, define the comparison key:

```
key(op) = (op.wall_ts, op.op_seq, op.site_id)
```

Compared **lexicographically**: first `wall_ts` (int64), then `op_seq` (int64), then
`site_id` (ULID string, §2.1). The op with the **greater** key wins; its `cols` become the
row's materialized state.

This is a strict total order over distinct ops:
- Different devices ⇒ different `site_id` ⇒ always distinguished at the final component.
- Same device ⇒ different `op_seq` (monotonic, never reused) ⇒ distinguished at the second.
- Identical `(site_id, op_seq)` ⇒ the **same op** ⇒ deduped, not compared (§5.3).

### 5.2 Convergence property (asserted + tested)

Because the winner for each `(table, pk)` is selected by a total order that is **independent
of arrival order**, every replica that has observed the same *set* of ops for a pk computes
the same state — regardless of delivery order or duplication. This is correctness under
at-least-once, out-of-order delivery. The vector suite includes shuffled and duplicated
orderings that MUST all reduce to one expected state (`shuffled-order`, `duplicate-ops`).

Consequences that fall out for free:
- **Stroke union:** distinct strokes have distinct ULIDs ⇒ distinct pks ⇒ all survive; no
  same-pk collision across devices is possible.
- **Delete / restore:** `deleted_at` is just an LWW column; the latest writer decides
  live-vs-deleted. `delete-then-restore` and `restore-then-delete` converge to the op with
  the greatest key.

### 5.3 Per-op processing (the apply algorithm)

For each incoming op, in order:

1. **Validate** — known `table`; `cols` parseable; `op_seq > 0`; `pk` and `site_id` are
   valid ULIDs. Fail → skip op, log, continue the batch.
2. **Dedup** — if `(site_id, op_seq)` is already in the changelog, skip (idempotent). It
   still counts toward `accepted_through`.
3. **Normalize** — drop unknown columns (§3.2); fill missing-but-known columns? No: a valid
   op MUST carry all known columns for its table (a full-row upsert). Missing a known column
   is a malformed op → skip + log.
4. **Merge** — load the current mirror row for `(table, pk)`. The incoming op wins **iff**
   `key(incoming) > key(stored_winner)` (or no row exists). On win, write `cols` and record
   the winner's `(wall_ts, op_seq, site_id)`. On loss/tie-to-stored, the mirror is unchanged
   but the op is still recorded in the changelog (step 5) for relay completeness.
5. **Record** — assign the next global `seq` and append the op to the changelog keyed by
   `seq`, with its `(site_id, op_seq)` unique. (Deduped ops from step 2 are not re-appended.)

> **Materialized state includes deleted rows.** A row with `deleted_at != null` stays in the
> mirror (needed for restore convergence and relay). Query/render layers filter
> `deleted_at IS NULL`; that is a read concern, not a merge concern. Conformance vectors
> therefore list deleted rows in `expected_state`.

---

## 6. Schema-hash validation

`schema_hash` is the lowercase hex SHA-256 of a canonical schema string. The string is built
deterministically (no implementation-order dependence):

- Tables in fixed order: `notebook`, `page`, `stroke`.
- Within each table, column names sorted **ascending ASCII** (alphabetical).
- Format: `table:col,col,...` per table, tables joined by `;`, no spaces, no trailing newline.

The v1 canonical string is:

```
notebook:created_at,deleted_at,name,sort_order;page:created_at,deleted_at,notebook_id,sort_order;stroke:color,created_at,deleted_at,page_id,pen_width_max,pen_width_min,points,z
```

```
schema_hash = sha256(utf8(canonical string))
            = 0df009c588f7d4b663b82861f10565fde7776e50da738bbca2ef174b27b83cd2
```

The server rejects a request whose `schema_hash` it does not recognize (`409`, §7) so a
client on an unknown schema cannot corrupt the mirror. (Only the column **set** is hashed —
identity/envelope fields `pk`, `site_id`, `op_seq`, `wall_ts` are not part of it.)

---

## 7. Auth and error codes

Reuse UB's existing `auth` middleware: bearer token via `mcpauth` preferred, Basic auth
fallback.

| Status | Meaning |
|---|---|
| `200` | success (some individual ops may have been skipped + logged) |
| `400` | malformed request envelope |
| `401` | unauthenticated |
| `409` | `schema_hash` unrecognized, or `protocol_version` unsupported |

Individual malformed ops inside an otherwise-valid batch are **skipped and logged**, not
fatal (mirrors the pipeline's "skip bad shape" tolerance). The batch returns `200`.

---

## 8. Versioning / forward-compatibility

- `protocol_version: 1` is this document. Unknown JSON fields in request/response/op MUST be
  ignored, not rejected.
- Reserved for `protocol_version: 2`: per-column LWW, block-level LWW (text notes), sync
  buckets / partial replication. v1 is one global cursor + row-level LWW + one implicit
  bucket (everything).
- Adding a column to an existing table changes `schema_hash` → a coordinated bump on both
  sides; the versioned envelope is the seam for doing that without breaking older clients.

---

## 9. Open items (resolve at implementation; do not block the contract)

1. **Pen-width type.** This spec fixes `pen_width_min/max` as `int64`. If ForestNote stores
   them as floats, define a fixed-point encoding (e.g. integer thousandths) **before** the
   first client ships, and update §3.1 + the canonical string + `schema_hash` accordingly.
   No floats on the wire.
2. **`color` representation.** Fixed here as packed-ARGB `int64`. Confirm against
   ForestNote's stroke schema.
3. **`points` int width.** Fixed as LE `int32` × 5. Confirm `pressure` and the `ts` split
   fit int32 on the device.

Items 1–3 are the only places the wire could still shift; everything else is frozen. Each is
a one-line edit to §3.1 + §6 if it changes.
