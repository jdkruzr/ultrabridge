# Future work — SPC features with no current UB analogue

**Status:** Audit captured 2026-05-22 during UB-as-SPC Phase 1 planning. Not a build plan; a triage record so each feature gets a deliberate decision (build / bridge / accept-loss) at its phase instead of inheriting a one-line "dropped" disposition.

## Why this doc exists

Several SPC features have no equivalent in UltraBridge today. The design plan (`docs/design-plans/2026-05-15-ub-as-spc-refactor.md`, "Scope dropped") triages them with short dispositions. While planning Phase 1 we noticed the triage has a **systemic blind spot**, and that at least one feature (digests) is a first-class community feature that cannot be papered over with a stub long-term.

## The systemic finding — sync soak is blind to user-initiated endpoints

The Phase 0b evidence base is a **passive sync trace** (device boots, syncs, idles). Several no-analogue features are **user-triggered actions** (note→PDF export, share) that do **not** fire during an idle sync. So "not seen in 0b" is **not** evidence "the user won't hit it" — the verification method is structurally blind to user-initiated endpoints. (The finding holds even though two features it originally cited — "dictionary lookup" and "tag edit" — turned out on closer reading of the decompiled source to be backend plumbing, not user features; see the reclassification below.)

Digests are the *only* user-facing feature of this class that *also* rides the sync path (the device diffs summary hashes every sync), which is why they got caught and reclassified from "drop" to "stub." The others got the same weak evidence ("not seen") but a worse disposition (404), purely by accident of not touching the sync path.

**Rule going forward:** before 404'ing any user-facing SPC feature on "not seen in 0b" grounds, re-verify with a *deliberate on-device action* (export a note to PDF, tap share, etc.), not just a sync soak. (Equally: confirm a feature *is* user-facing before flagging it — "dictionary"/"tag" looked user-facing by name but the decompiled source showed they were backend lookup tables and a filename search.)

## The features (high-risk first)

"Failure shape" = what the user experiences if we ship the current disposition.

| Feature | Controller(s) | Current disposition | Failure shape | Verdict |
|---|---|---|---|---|
| **Digests / Summaries** | `F_SummaryController` | Stubbed empty-success (Phase 1 AC4.7); `add/delete/download/summary*` 404 | Digests silently show empty on device | **Must build. First-class community feature. Stub is acceptable *only* as a Phase-1 placeholder so task sync works; not a long-term answer.** See "Digests" below. |
| **Note → PDF/PNG export** | `note/to/pdf`, `note/to/png`, `pdfwithmark/to/pdf` (`F_FileLocalController`) | Conditional Phase 5c, skippable ("not seen in sync") | On-device export-to-PDF fails | User-triggered; sync trace can't see it. Test the export UI on-device before skipping 5c. UB has rendering parts (`go-sn`, `booxrender`) to build it. |
| **Sharing** | `F_ShareController` (1 endpoint) | 404 OK | Share action errors | Single-user makes this *probably* fine; accept-loss is reasonable but make it explicit. |

## Genuine no-impact plumbing (accept-loss is correct)

User registration / SMS / password reset / valid-code / sensitive-ops; email server config; FTP upload (SPC-internal); OSS multipart (>50MB only); File V2 query-by-path; SPC Vue web UI (UB's `internal/web` is the real replacement); Redis; multi-user. These are internal machinery or single-user-obviated — dropping/404'ing them has no user-visible cost.

**Reclassified here 2026-05-25 (were wrongly flagged "high-risk" above — corrected after reading the decompiled source):**

- **Dictionary / Reference** (`B_DictionaryController` `/api/system/base/dictionary/*`, `B_ReferenceController` `/api/system/base/reference/*`). NOT a handwriting word-lookup feature. "数据字典" (*data dictionary*) is enterprise jargon for a backend **lookup/enumeration table** — code→label mappings and localized UI strings (the `language` param: blank = Chinese, `US` = English) used to populate the SPC web app's dropdowns. The Supernote device's actual on-device dictionary is local **StarDict** files that never touch the cloud. Kicker: nearly every endpoint path literally ends in `/deleteApi` (method names `*_deleteApi`) — SPC's own marker for deprecated/scheduled-for-deletion APIs. Pure backend plumbing; the device won't call it. **Accept-loss.**
- **File "label" search** (`F_FileSearchController` `/api/file/label/list/search`). NOT a tagging feature. `FileLabelSearchDTO` carries only `fileName` + `directoryId` (no tag/label field), and the file domain (`f_user_file`) has no label column — it's a directory-scoped **filename substring search**, on the Partner/web search controller the device never hits. There is no device-facing way to put a tag on a file in SPC. **Accept-loss.**
- **Digest tags** (`SummaryDO.tags` + `SummaryTagDO` → `t_summary_tag`) — the *only* real tags in SPC, and they belong to **digests**, not files. **Already built** in Phase D (D1): `t_summary_tag` + tag CRUD in `handlers/summary.go`. Not an open item.

## Digests — the one that needs a real build

**User stance (2026-05-22): digests are a first-class feature in the Supernote community. A stub that returns empty is fine *only* to keep Phase 1 task sync unblocked; it is not a rational long-term disposition. "RAG/search supersedes it" is not true** — a digest is a user-curated, deliberately-saved excerpt with its own handwritten annotation (an intentional artifact), whereas RAG is on-demand retrieval. They are different features.

### What a UB-native digest would need (investigate fresh; pointers only)

- **Storage:** a `digests` (and digest-group/category) table in `notedb`. Fields from `docs/PRIVATE_CLOUD_REFERENCE.md` §6: `md5Hash`, `handwriteMd5`, `parent_unique_identifier` (→ group UUID), source reference, page index, layer info, `is_deleted`. Note-digest vs document-digest metadata differ (§6).
- **Handwriting sidecars:** `.mark` files (RATTA_RLE-encoded handwriting) live in `sndata/digest/{md5}.mark`, uploaded/downloaded via the OSS-signed-URL path (§6 OSS HMAC). UB must store and serve these blobs (passthrough is enough; decoding RATTA_RLE strokes is optional/later).
- **Endpoints (replace the Phase-1 stubs with real impls):** create-then-update — `POST /api/file/add/summary` → OSS upload → `PUT /api/file/update/summary`; queries `POST /api/file/query/summary/{group,id,hash}`; soft-delete syncs so other devices remove local copies.
- **Engine.IO:** `DIGEST-SYN` events (`ADD_DIGEST`/`UPDATE_DIGEST`/`DELETE_DIGEST`) on the file channel — currently Phase 5d (conditional). Becomes required if digests are first-class.
- **Surfacing in UB:** digests should appear in `internal/web` (a Digests tab?) and ideally feed `internal/search`/`internal/rag` as high-value curated excerpts — *in addition to* being stored as first-class artifacts, not instead of.

### Suggested phasing

Don't block the 6-phase refactor. Land Phase 1's empty stub, ship task sync, then schedule a dedicated "Digests as a first-class UB feature" build session (likely after Phase 3 download / Phase 4 upload exist, since digests reuse the OSS signed-URL upload/download path). The `.mark` blob store + OSS path is the prerequisite.

## Dropped infrastructure: Redis (offline push queue)

The design drops Redis entirely (`UB-as-SPC` "Scope dropped"). SPC uses Redis for four jobs, all of which reduce to *it is a multi-instance, multi-tenant cloud and Redis is the shared state plane*: (1) token cache / existence check (`user/service/impl`, `CacheKeyUtil.nonExpiryToken`), (2) Engine.IO connection registry + **pending-message ZSets** (`socket/io/*`, 30-min TTL, drained on reconnect), (3) distributed sync lock (`file/service/impl`, `CacheKeyUtil.lockCloud` + `synchronousStart` 24h TTL), (4) connected/disconnected counters.

For UB (single process, single user, 1–2 devices) dropping Redis loses **almost nothing** — in-memory maps *are* the shared state because there's one process; the distributed sync lock is replaced by SQLite single-writer (WAL, `MaxOpenConns=1`) + UB-wins reconciliation; TTL'd codes become the in-memory randomCode store (Phase 1 Task 9).

**The one real gap is the offline push queue.** SPC queues a push (e.g. STARTSYNC) when the target device is offline and delivers it on reconnect. UB's socket registry (Phase 1 Task 14) `Emit`s only to currently-connected sockets and no-ops otherwise — so a web/CalDAV edit made *while the device is offline* won't deliver the instant STARTSYNC nudge. **This is not data loss:** the device still pulls the change on its next periodic sync/poll; only the immediacy is lost for edits during the offline window. Judged an acceptable degradation for 1–2 devices (decision 2026-05-22). Minor secondary loss: stateless JWT can't revoke a single device's session server-side (only `spc_jwt_secret` rotation, which logs out everything).

**If parity is ever wanted**, the cheap path is NOT Redis — it's a small "pending nudges per userId" slice in the registry (`internal/spcserver/socketio`) that buffers undelivered emits and flushes on connect. That's the natural seam; don't build it speculatively.

## Starting points

- This doc; `docs/spc-protocol.md` §5 (endpoint dispositions), §6 (OSS HMAC); `docs/PRIVATE_CLOUD_REFERENCE.md` §6 (full digest wire contract).
- Design plan "Scope dropped" + Phase 5 (5b search, 5c render, 5d DIGEST-SYN).
- Memory: `project_spc_no_analogue_features`.
