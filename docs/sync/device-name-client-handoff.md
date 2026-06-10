# ForestNote client hand-off: `device_name` on the sync envelope

Date: 2026-06-10. Server side is DONE and deployed on branch
`feat/sync-device-management` (spec: `forestnote-sync-protocol.md` §4 request
shape + new §4.3 "Device registry & lifecycle"). This doc is the work order for
the ForestNote (Kotlin) repo, in the style of `page-text-client-handoff.md`.

## What the server now does

- `POST /sync/v1` accepts an OPTIONAL `device_name` string on the request
  envelope. It is informational only: trimmed, truncated to **128 runes**,
  stored on the device's registry row, and shown in UltraBridge's
  Settings → Sync Devices card and `GET /api/v1/sync/devices`.
- **Absent or empty preserves the stored name** — an old client can never erase
  a name, and there is no need to send it conditionally; send it every sync and
  client-side renames (or OS model-string changes) propagate automatically.
- Not part of the schema hash (§6); `protocol_version` stays **1**. A v1-spec
  server that predates the field ignores it (§8 unknown-envelope-field rule),
  so the updated client is safe against older UltraBridge builds.
- Context for "why": the server can now *prune* a device's registry row. A
  reinstall/factory-reset mints a new `site_id`, so without a label the operator
  sees two opaque ULIDs and can't tell the dead install from the live one. Two
  rows named "Viwoods AiPaper" with different last-seen times are self-evident.

## Client changes (ForestNote repo, `~/ForestNote`)

### 1. `core/sync/.../SyncProtocol.kt` — wire DTO

Add the optional field to `SyncRequest` (after `siteId`):

```kotlin
@Serializable
data class SyncRequest(
    @SerialName("protocol_version") val protocolVersion: Int = PROTOCOL_VERSION,
    @SerialName("schema_hash") val schemaHash: String,
    @SerialName("site_id") val siteId: String,
    @SerialName("device_name") val deviceName: String? = null,
    val cursor: Long,
    val ops: List<WireOp>
)
```

Serialization note: `HttpUrlTransport`'s `Json` is configured with
`encodeDefaults = true` (HttpUrlTransport.kt:22), so a default-null
`deviceName` would serialize as `"device_name":null`. That is *harmless* to
both old and new servers (Go decodes JSON null into ""), but to keep the
unset wire byte-identical to today's, add `explicitNulls = false` to that
`Json { … }` block. Either way is contract-compatible; `explicitNulls=false`
is the tidy option.

### 2. `core/sync/.../SyncEngine.kt` — thread the value

`SyncEngine` (class at line ~41) gains a constructor parameter
`private val deviceName: String? = null`, and the `SyncRequest(...)`
construction at line ~59 passes `deviceName = deviceName`. `core:sync` is
platform-pure — do NOT read `android.os.Build` here; the string is injected
from the app layer.

### 3. `app/notes/.../SyncController.kt` — supply the value

Both `SyncEngine(...)` construction sites (lines ~76 and ~109) pass a value
computed once, e.g.:

```kotlin
private val deviceName: String =
    "${android.os.Build.MANUFACTURER} ${android.os.Build.MODEL}".trim()
```

(`MANUFACTURER` + `MODEL` reads like "Viwoods AiPaper" / "Onyx BOOX Tab
Ultra"; no permission needed.) If `SyncController` is meant to stay
Android-free, inject the string from `MainActivity` instead — the contract
only cares that *some* stable human-readable label arrives.

Optional follow-on (not required for v1 of this feature): a user-editable
"device name" field in `SettingsView` that overrides the Build-derived
default. The server treats any non-empty name as a rename.

### 4. Tests

In the `core:sync` test suite (alongside the existing envelope tests):

- `SyncRequest` with `deviceName = "Test Tablet"` serializes a
  `"device_name":"Test Tablet"` key.
- `SyncRequest` with the default (`null`) omits the key (with
  `explicitNulls = false`) — or, if you skip that config change, document
  that it emits `"device_name":null` and rely on the server-side
  null-tolerance noted above.

## Server contract summary (for the client engineer)

| Property | Value |
|---|---|
| Field | `device_name`, string, OPTIONAL, request envelope only |
| Length | server trims whitespace, truncates to 128 runes — never rejects |
| Empty/absent | preserves the previously stored name |
| Non-empty | overwrites (renames propagate on every sync) |
| Versioning | `protocol_version` stays 1; not in `schema_hash` |
| Old servers | ignore the field; `null` is also tolerated |

## Verified server behavior (curl, 2026-06-10)

Sync with `"device_name":"Test Tablet"` → `GET /api/v1/sync/devices` shows
`{"name":"Test Tablet","first_seen":<ULID-decoded ms>,…}`; a follow-up sync
without the field leaves the name intact; pruning the device and syncing again
re-registers it cleanly (accepted_through reseeds from the changelog, §4.3).
