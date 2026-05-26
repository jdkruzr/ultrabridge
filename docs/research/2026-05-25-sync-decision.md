# Sync Decision Dossier — ForestNote ↔ UltraBridge

**Date:** 2026-05-25
**Status:** DECIDED — **roll our own** device↔server sync (not yet built; design later).
**Audience:** portable record for the UltraBridge work session. Self-contained; merges the 2026-05-23 research (`sync-options.md`), the 2026-05-25 license/Android deep-dives, and the chosen direction.

---

## TL;DR

We will **build our own** SQLite device↔server sync rather than adopt PowerSync, cr-sqlite, sqlite-sync, or SQLSync. The deciding factor is **licensing against the business model**: there's a likely future of **hosting UltraBridge instances in AWS for paying customers** (most e-note users would rather pay than self-host). That hosted-service activity is exactly what the "managed service" clauses in **PowerSync (FSL)** and **sqlite-sync (Elastic License 2.0)** reserve for a paid commercial license. The two clean-license options (cr-sqlite MIT, SQLSync Apache-2.0) fail on Android-viability / maturity. Roll-our-own is the only path that is simultaneously **license-unencumbered**, **viable on the locked Viwoods device**, and **fully ours to host commercially**.

Our data shape makes this tractable: **append-mostly immutable strokes, ULID PKs, benign conflicts** (merge ≈ union of inserts + tombstone deletes; real semantic conflicts are rare). ForestNote already seeded **ULIDs + explicit `z` ordering** for exactly this.

---

## Decision rationale (why not adopt a library)

| Option | License for *our MIT stack* | Android on Viwoods (locked, targetSDK 30) | Maintenance (as of 2026-05) | Server model | Verdict |
|---|---|---|---|---|---|
| **PowerSync** | ❌ FSL **Competing Use** clause blocks a commercial sync product | ✅ official Kotlin SDK | ✅ active | Postgres/Mongo + their sync service | **OUT** — license blocks the product/hosting |
| **cr-sqlite** (vlcn-io) | ✅ **MIT** | ❌ prebuilt `aarch64` `.so` exists but needs static-link/JNI + own SQLite build | ❌ **dormant since Jan 2024** (author → Rocicorp/Materialite; "v2" unshipped) | bring-your-own (changeset/peer) | **OUT** as dep; **KEEP as design reference** |
| **sqlite-sync** (SQLite Cloud) | ⚠️ **Elastic 2.0** — OSS grant covers MIT *self-host*, but **managed-service clause gates the hosted business** | ✅ **Maven AAR**, loads from `nativeLibraryDir` (safe on locked device); bundles own SQLite | ✅ active (v1.0.19, commits this month) | self-host Postgres+`cloudsync` ext, **or** custom network layer; or their paid cloud | **OUT** — managed-service clause hits the hosted UltraBridge business |
| **SQLSync** (orbitinghail) | ✅ **Apache-2.0** (cleanest) | ❌ **none** — web/React/WASM only | ❌ pre-prod, paused (CHANGELOG ends 0.3.2 Mar 2024; "do not use in production"; coordinator "COMING SOON") | central coordinator + reducer model | **OUT** — no Android, rewrite-level model, unfinished |
| **roll-our-own** | ✅ **no entanglement — we own it** | ✅ pure Kotlin, fully debuggable on-device | n/a | UltraBridge, however we want | ✅ **CHOSEN** |

### License specifics (the crux)

- **PowerSync — FSL-1.1-ALv2 (Functional Source License).** Split-licensed: **client SDKs are Apache-2.0** (fine to embed), but the **Service is FSL** with a **"Competing Use"** clause prohibiting use in a commercial product/service offering "the same or substantially similar functionality." A sync-bridge IS substantially similar → blocked. FSL converts to Apache **per-release, two years after each version's publish date** (so the version you ship now converts ~2 years later, rolling — not a usable plan). Refs: powersync.com/legal/fsl, powersync.com/legal/licensing-terms.
- **sqlite-sync — Elastic License 2.0 (modified) + Additional Grant for Open-Source Projects.** Free for use "incorporated into or used by an OSI-approved open-source project" (MIT qualifies → covers ForestNote client AND a *self-hosted, distributed* MIT UltraBridge). **BUT Condition 3:** "You may not provide the software to third parties as a managed service … unless you have a license for that use." Distributing OSS for users to self-host = fine (Elastic's own FAQ: contractor-setup/self-host is permitted). **Operating a paid hosted UltraBridge-as-a-service for third parties = the gated case** → needs a commercial license. There's genuine textual tension (Condition 1 "without restriction" vs. Condition 3) for the OSS-project-runs-a-service edge case → if we ever go hosted on sqlite-sync, get written clarification from SQLite Cloud. Client SDKs are NOT separately Apache here (unlike PowerSync) — the whole thing is Elastic. Refs: the repo `LICENSE.md`, elastic.co/licensing/elastic-license + /faq.
- **cr-sqlite — MIT.** No restrictions at all. Clean for any commercial/hosted use.
- **SQLSync — Apache-2.0.** No restrictions at all.

**Net:** only MIT/Apache (cr-sqlite, SQLSync) and roll-our-own permit the paid-hosting business. Of those, only roll-our-own is also device-viable + maintained-by-us.

### Android-on-Viwoods specifics

- **cr-sqlite:** CI builds an `aarch64-linux-android` `.so` and ships it in releases; static linking is supported (`SQLITE_EXTRA_INIT=core_init`). But Android 11 / targetSDK 30 won't reliably runtime-load extensions, so the real path is **statically linking crsqlite into your own SQLite build + JNI + rewiring SQLDelight off the system SQLite** — native work on a device where adb is broken and the only debug channel is a logfile. Schema constraints (enforced in `is_table_compatible`): explicit non-null PK, **no AUTOINCREMENT**, no UNIQUE indexes besides PK, **no *declared* foreign keys** (declared FKs appear in `pragma_foreign_key_list` even with enforcement off → would reject our `page`/`stroke` tables until the `REFERENCES` clauses are stripped), NOT-NULL non-PK columns need DEFAULTs. We satisfy the PK/autoincrement rules (ULIDs); FK-strip + NOT-NULL-default work would be required.
- **sqlite-sync:** **Maven `ai.sqlite:sync`**; loads the bundled `.so` from `getApplicationInfo().nativeLibraryDir + "/cloudsync"` via `SQLiteCustomExtension` + `SQLiteDatabaseConfiguration` — i.e. it ships **its own SQLite** (extension-loading enabled), and you route through that. The nativeLibraryDir loading model is exactly what's safe on a locked targetSDK-30 device (no runtime download, no SELinux fight). Self-host server = Postgres/Supabase + the `cloudsync` PG extension (deploy guides exist: `docs/internal/postgres-flyio.md`, `supabase-flyio.md`), **or** replace transport entirely with a **custom network layer** (`#define CLOUDSYNC_OMIT_CURL` + implement `network_send_buffer`/`network_receive_buffer`). Schema constraints are similar (see `docs/SCHEMA.md`).
- **SQLSync:** no native Android path at all (WASM-in-browser + JS worker + React/Solid).

---

## What to STEAL — the "chimera" design (for the UltraBridge sync build)

Roll-our-own does NOT mean invent a protocol from scratch. Harvest the proven primitives:

### From cr-sqlite → the **data model / changeset format**
- Per-column versioning: a **Lamport `col_version`** per cell; a **`site_id`** per replica; a monotonic **`db_version`** (per-DB transaction clock); causal-length (`cl`) + `seq` for ordering.
- **Deletes as a clock-table sentinel** (metadata marker), not visible tombstone rows — though for our benign model, simple `deleted_at` tombstones on rows may be enough; cr-sqlite's per-column LWW is the richer reference if we want it.
- The **`crsql_changes`-style pull**: "give me all changes since (version X, excluding site Y)" → ship rows over any transport → apply by inserting them on the other side.
- Schema discipline: stable non-rowid PKs, no autoincrement, defaults on NOT-NULL for forward/back compat. **ForestNote already does ULID + explicit `z`.**

### From sqlite-sync → **operational layer + multi-tenant hosting**
- A single **`sync()`** call that batches send + receive.
- **Schema-hash validation**: server rejects payloads whose table-shape hash it doesn't recognize.
- **Server-enforced Row-Level Security**: one shared DB, each client syncs only its authorized rows — **directly relevant to multi-tenant UltraBridge hosting** (the paid business).
- Transport/merge separation (their custom-network-layer abstraction).
- **Block-Level LWW** (line-level merge for text/markdown) — a gift if ForestNote grows text notes / for agent-memory-style data.

### From PowerSync → **partial replication + client ergonomics**
- **Sync buckets**: partition what each client pulls (matters once a notebook library is large, or you don't want every device pulling everything).
- Checkpoints; client **write-queue → upload-then-reconcile** loop; surfacing sync status as observable state.

### From SQLSync → **contrast + one idea**
- Its central-coordinator deterministic-rebase model is mostly **cautionary** (the reducer rewrite is what we're avoiding). Borrow the **reactive query subscription** idea and rebase-on-authoritative-timeline thinking for conflict edge cases.

### Synthesis target
> **cr-sqlite-style changeset format** + **sqlite-sync-style single-call sync with RLS** for paid multi-tenant hosting + **PowerSync-style buckets** for partial replication — **pure-Kotlin client on the device, UltraBridge server we own** (Postgres-backed, likely). UltraBridge hosts three adapters (ForestNote / Boox / Supernote); cr-sqlite-style CRDT magic only spans ForestNote↔ForestNote — the **cross-vendor reconciliation inside UltraBridge is our own logic regardless**, which is another reason owning the server is the right call.

### Our constraints (carry into the design session)
- Append-mostly immutable strokes; benign conflicts (union of inserts + tombstone deletes).
- ULID + explicit `z` already in the schema (seeded for sync).
- Off-thread persistence via `NotebookStore` (single-thread executor); UI never touches the repo directly. **No coroutines in the codebase yet** — sync is the phase where adopting kotlinx-coroutines + `Flow`/`StateFlow` pays for itself (network I/O, retries/backoff, periodic sync loop, connectivity observation, a `StateFlow<SyncStatus>` for the UI).
- Must run on the locked Viwoods (no root, targetSDK 30, adb broken — logfile-only debug) AND extend to Boox/Supernote adapters server-side.
- UltraBridge is **ours** + may be **hosted-for-pay** → no library whose license gates that.
- Near-term ForestNote overlap: **Area E soft-delete** is the first place the schema touches the sync model — make tombstones (`deleted_at`) the deletion mechanism so the future sync engine has clean delete semantics to replicate.

---

## Sources

- Local research report: `docs/research/sync-options.md` (2026-05-23 — PowerSync/cr-sqlite/libSQL-Turso/CouchDB/ElectricSQL/roll-your-own/Litestream).
- Local clones inspected 2026-05-25: `~/sqlite-sync` (SQLite Cloud), `~/sqlsync` (orbitinghail).
- cr-sqlite: github.com/vlcn-io/cr-sqlite (README, `core/Makefile`, `.github/workflows/publish.yaml`, `core/rs/core/src/tableinfo.rs` `is_table_compatible`), vlcn.io/docs.
- sqlite-sync: github.com/sqliteai/sqlite-sync (`LICENSE.md`, `README.md`, `docs/INSTALLATION.md`, `docs/internal/network.md`, `docs/postgresql/`), docs.sqlitecloud.io.
- SQLSync: github.com/orbitinghail/sqlsync (`README.md`, `GUIDE.md`, `CHANGELOG.md`).
- PowerSync: powersync.com/legal/{fsl,licensing-terms,commercial-license-and-services-agreement}; docs.powersync.com/architecture/powersync-protocol.
- Elastic License 2.0: elastic.co/licensing/elastic-license (+ /faq).

---

*Companion memory note: `~/.claude/projects/-home-jtd-ForestNote/memory/sync-decision.md`. This dossier is the portable, fuller version for the UltraBridge session.*
