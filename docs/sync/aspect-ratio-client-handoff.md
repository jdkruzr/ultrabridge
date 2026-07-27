# ForestNote `notebook.aspect_long_axis` — rollout record (schema v4)

Unlike the other files in this directory, this is **not** a build spec handed to the
ForestNote (Kotlin) Claude. The client half shipped first and was already in the field
for three weeks before the server caught up, so this records what landed and what the
rollout actually demonstrated. Written 2026-07-27, after the server half deployed.

## What the column is for

A ForestNote notebook keeps the native page aspect of the device that created it, so a
note authored on a tall device does not distort when opened on a wider one. The renderer
letterboxes; it never stretches. `aspect_long_axis` carries the creating device's long-axis
page dimension so any device can reconstruct that ratio.

| column             | type              | notes                                            |
|--------------------|-------------------|--------------------------------------------------|
| `aspect_long_axis` | INTEGER, nullable | page long edge, short edge normalized to 10000    |

### What the number actually is

**The page's long edge expressed in a coordinate space where the short edge is exactly
10000** — i.e. the aspect ratio × 10000. Derived empirically from stroke geometry
(2026-07-27), since no consumer existed to document it:

- Stroke `points` are base64 little-endian records of 5 × int32 — `x, y, pressure,
  width, t` — 20 bytes per point.
- Across every device in the fleet, `max(x)` tops out at **exactly 10000** where a stroke
  reaches the right edge, and `max(y)` always lands at or under that notebook's
  `aspect_long_axis` — one sample reached 12542 against a bound of 12688. Strokes fill
  the box and never escape it.

So `12192` is a 1.2192 : 1 page, `18556` is 1.8556 : 1, and the `null` legacy default of
3:4 is **13333** in these units.

Two consequences worth knowing:

- It is a **ratio, not a size**. Two devices with the same proportions report the same
  value regardless of physical dimensions — a 10.3″ Tab Ultra C Pro and a 10.3″ Go 10.3
  both report `12688`. Never use it as a device fingerprint.
- It is **not the raw panel ratio**. A Boox Go 6's panel is 1448×1072 (1.3507) but it
  reports `12192` (1.2192), because the client measures the *drawable page* with device
  chrome excluded. That is the correct thing to sync: it is what has to letterbox on the
  receiving device.

Nullable is load-bearing: the client authors the notebook row first with
`aspect_long_axis: null`, then updates it a beat later once page geometry is known. Both
ops are normal LWW upserts, so a consumer sees null briefly and must not treat it as an
error.

Mirror DDL lives in `internal/syncstore/schema.go` (new-DB column set plus an
`ensureColumn` entry for existing databases); the wire registry entry is in
`third_party/rhizome-server-go/registry/forestnote.go`.

## Schema hash → v4

```
SCHEMA_HASH (v4) = 74e6b5d790c919290d0e1fca3462800a5dc4abb288042dda2b48d4eb0482bbf2
```

Canonical string (tables alphabetical, columns alphabetical within a table — note
`aspect_long_axis` sorts to the FRONT of `notebook`):

```
folder:created_at,deleted_at,name,parent_folder_id,sort_order;notebook:aspect_long_axis,created_at,deleted_at,folder_id,name,sort_order;page:created_at,deleted_at,notebook_id,sort_order,template,template_pitch_mm;page_text_from_client:created_at,deleted_at,model,ocr_at,text;page_text_from_server:created_at,deleted_at,model,ocr_at,text;stroke:color,created_at,deleted_at,page_id,pen_width_max,pen_width_min,points,z;text_box:border_width,color,created_at,deleted_at,font_name,font_size,height,page_id,text,weight,width,x,y,z
```

The prior **v3** hash `724411eb845ad3487393a77cb5559690e69332c35fdb5ee3e85c1767bf71f3fe`
stays in `AcceptsSchemaHash` for one release. **v2 and v1 are retired** — this bump closed
v2's grace window, so a v2 client now gets a hard 409. Every device was on v3 or later
before the server deployed, so nothing was cut off.

## What the rollout demonstrated

The client began sending `aspect_long_axis` on **2026-07-04**; the server half deployed
**2026-07-27**. For those three weeks the server ran v3 and did not know the column
existed. Two properties of the design made that safe, and both are worth relying on
deliberately for future column additions:

1. **Unknown columns survive in the changelog.** `sync_ops.payload` stores the op verbatim,
   so the column was recorded on every push even though `knownCols` had no entry for it and
   the mirror had no place to put it. Nothing was lost.
2. **The mirror caught up from history, not from zero.** When the server deployed v4 and
   `ensureColumn` added `fn_notebook.aspect_long_axis`, materialization drew on ops already
   in the log. Exactly 24 distinct notebooks had ever been sent with a non-null value, and
   exactly 24 carried one in the mirror afterward — no gaps, no invented rows. Devices did
   not have to re-author anything.

So a **client-ahead rollout is a supported shape**, not an accident: ship the client
whenever it is ready, let the payloads accumulate, and the server backfills when its half
lands. The reverse order (server ahead, client behind) is what the grace window protects.

## Verification performed

- Runtime `SchemaHash()` equals the v4 constant above; `AcceptsSchemaHash` returns true for
  v4 and v3, false for v2.
- `fn_notebook` gained `aspect_long_axis` on deploy via `ensureColumn`.
- A live device sync afterwards pushed 1046 ops (1031 stroke, 7 page_text_from_client,
  4 notebook, 4 page), relay high-water 67641 → 68688, no 409s, and the server authored a
  `page_text_from_server` op back — full round trip on v4.
- Notebook ops on the wire carried the null-then-value pair described above.

## Still open

Nothing on the sync contract. The remaining work is renderer-side: confirming every
consumer letterboxes rather than stretching when a notebook's aspect differs from the
viewing device.
