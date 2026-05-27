# ForestNote text-box sync — client cutover hand-off

**For: the ForestNote (client) side.** Written 2026-05-27 against UB branch
`feat/forestnote-text-box-sync` (commit `9bd0ea5`), cross-checked against client branch
`feat/text-boxes`.

The UltraBridge server now **accepts and materializes** `text_box` sync ops (schema v2). The
client code for text boxes is already built but gated off. This note is everything the client
side needs to turn it on. (Server-side render + search indexing of box text are separate,
later UB phases and do **not** block the client cutover — once the steps below land, boxes
round-trip device↔device through UB; they just won't yet appear in UB's PDFs/search.)

## 1. Two constants to change (coordinated release)

```kotlin
// core/sync/.../SyncProtocol.kt
const val SCHEMA_HASH = "bc1953e2b85e766a572329e7023b4582b768094b4d27e28a632e21bedb776874"

// core/format/.../NotebookRepository.kt
internal const val TEXT_BOX_SYNC_ENABLED = true
```

- The new hash is the SHA-256 of the v2 canonical schema string (folder/notebook/page/stroke
  **+ text_box**). It is derived, not invented — UB's `op_test.go` asserts
  `SchemaHash() == bc1953e2…` and the spec records the canonical string
  (`docs/sync/forestnote-sync-protocol.md` §6).
- The prior v1 hash was `9b807dc88cd0465d171892bb17e65ad94190eda058594e207caad3368eb1f2fe`.

## 2. You are NOT under time pressure to flip (grace window)

UB now accepts a **set** of schema hashes — currently `{v1, v2}` — not a single value. So:

- An un-updated client still sending v1 (with `TEXT_BOX_SYNC_ENABLED=false`) **keeps syncing**;
  it just never emits `text_box` ops. No 409, no lockout.
- An updated client sending v2 with the flag on also syncs, and its `text_box` ops materialize.

So the client release can ship on its own schedule. Once every client is on v2, UB will drop
v1 from its accepted set (a one-line server change) to re-tighten the gate.

## 3. Mirror the conformance vectors and prove them green in Kotlin

This is the load-bearing step — it is the actual cross-language proof that the two merge
implementations agree on `text_box`. UB authored three new canonical vectors:

```
docs/sync/vectors/20-text-box-basic.vector.json   # one op → live row, 14 cols + provenance
docs/sync/vectors/21-text-box-delete.vector.json  # deleted_at LWW: create → delete → restore → live
docs/sync/vectors/22-text-box-lww.vector.json     # two ops on one pk converge to the greater key
```

Copy them **verbatim** into the client's `core/format/src/test/resources/sync-vectors/` and run
the client's conformance test. They must pass unchanged. If any vector is green in Go but red
in Kotlin, the implementations disagree — a release blocker (do not flip the flag until green).

(These were deliberately written on the UB side first and NOT yet added to the client; the
vectors directory is "edit on UB, mirror to client".)

## 4. The contract that was cross-checked (FYI — already verified, no action)

Verified byte-for-byte against the client's `SyncWire.kt`/`SyncMerge.kt`:

- **Column set + order** (alphabetical): `border_width, color, created_at, deleted_at,
  font_name, font_size, height, page_id, text, weight, width, x, y, z`.
- **Wire types**: all int64 except `text`/`font_name`/`page_id` (string); `deleted_at` nullable.
- **`color`**: signed ARGB Int → unsigned int64 via `and 0xFFFFFFFFL` (identical to `stroke`);
  decode reinterprets the low 32 bits and sign-extends. UB stores the unsigned value verbatim
  and treats it exactly as `stroke.color`.
- **Geometry / `font_size`**: virtual units, page short axis = 10,000.
- **`z`**: paint band, 0 = below ink, 1 = above ink.

## 5. After cutover

Verify a text box created on an updated client round-trips into UB's `fn_text_box` mirror, and
that a still-v1 client (if any) keeps syncing. UB-side render (boxes in PDFs/page images) and
search indexing of box text land in subsequent UB phases — track those separately;
`docs/sync/text-box-server-support.md` Parts D/E.
