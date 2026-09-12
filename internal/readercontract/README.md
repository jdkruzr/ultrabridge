# Candidate ForestRead contract and reduction (Stage 2D8–D9)

Inactive host-domain code, not a production registry, router, migration or capability.
No production package imports this package. The existing seven-table writer registry and accepted
schema hashes remain unchanged. Rhizome still owns generic transport, LWW and registry mechanics;
ForestNote/UltraBridge own reader meaning.

## Boundaries

- `Registry()` describes 15 reader/reference tables. `CandidateCombined()` derives the proposed
  writer-plus-reader registry without changing `registry.ForestNote()` or its accepted hash set.
- `DecodeJSON` is the new lossless boundary: complete rows, strict text/blob/integer types, int64
  provenance, unsigned wire colors converted to signed ARGB, and domain shape validation. Do not
  first route reader rows through the existing legacy `map[string]any` float64 decoding path.
- `Validate` checks typed storage rows. `Check` applies the same immutable-identity, dependency,
  session-owner, terminal-state and producer rules as Kotlin's `ReaderDomainRules`. Its lookup
  must return previously validated rows with real provenance, or nil for an absent row.
  `applied` means eligible for generic LWW; it is not a claim that the row won. Retain/retry
  `pending` rows, including unversioned legacy conflicts. Never invent local authorship on receive.
- `RequireClientAuthor` requires a **host-verified site binding**, not a request's claimed site.
  Current UB Basic auth establishes an account, not an individually authenticated device.
  Device enrollment/credential binding is an unresolved activation gate. This helper implements
  neither enrollment nor credentials; it must not be described as deployed device authentication.
  Server-produced recognition is also deliberately rejected until its separate binding exists.
- `CompositeID` implements the exact compact JSON-array SHA-256 encoding, without Go's HTML
  escaping or Unicode normalization. Cross-language outer text must be losslessly representable
  in UTF-8; reject unpaired UTF-16 escapes rather than silently substituting replacement characters.
  Versioned selector JSON stays raw, including unknown versions/fields and literal `\ud800` text
  inside that opaque JSON string. Identity/title lengths follow Kotlin UTF-16 units, not UTF-8 bytes.
- `Fingerprint` streams FNRI1 over virtual canvas geometry and caller-ordered canonical ink.
  It includes signed ARGB, exact point/dynamics bytes, brush identity/version/seed, and all 17
  supported pen types. `Ink.Bottom()` uses virtual `y + widthMax`, never screen pixels.
- `Reduce` takes a detached, bounded, consistent snapshot of already-LWW-winning rows, one per
  identity, and derives effective annotation state. It never authenticates, changes storage,
  reauthors rows, renders, or reads a clock. Guarded ingress remains required: reduction is not
  a substitute for history/ownership validation. The host snapshot adapter is not implemented yet.
  Returned strokes share immutable snapshot data; callers must not mutate them during/after use.
  Book metadata presence is distinct from verified original-byte readiness.
- Reduction matches Kotlin cancellation, independent erase claims, property-version winners,
  exact raw anchor preservation, virtual minimum height, status precedence and recognition hashes.
  Only `READY` gets a hash. A `PENDING` result may retain visible partial context but cannot claim
  current recognition. Unknown surviving ink is unsupported; cancelled/erased unknown ink does
  not invalidate the surviving projection. Opaque IDs sort by Kotlin UTF-16 order without
  Unicode normalization, not Go UTF-8 order.
- Malformed-shape and unsupported-ink diagnostics use stable shared text, not language-specific
  exception strings. Other diagnostic reasons and ordering match Kotlin exactly.

## Verification

Run the sibling ForestNote headless runner:

```sh
node /home/jtd/ForestNote/docs/test-plans/forestread-stage-2/run.mjs --import-dir /home/jtd/Downloads
```

Kotlin generates vectors from the actual registry, real repository operations and pure rules;
Go independently checks those freshly generated vectors under the race detector. Full typed
descriptors are compared, because Rhizome's compatibility hash covers column names only. The
actual production Kotlin writer registry is compared too, not the older Rhizome v3 test fixture.
The runner fails if either Go parity test is absent, fails or skips; it records source/vector/log
hashes and the derived candidate hash. That hash is evidence, not permission to activate it.

Stage 2D9 adds 68 full-projection scenarios with 12 independent shuffled orders on each side.
These compare every output field, including surviving canonical stroke bytes/provenance, raw
anchors, nullability, statuses, diagnostic order and fingerprints. All six statuses and all 17
brush types are covered, with nonmutation checks and a standalone concurrent-reader race test.

Standalone `go test ./internal/readercontract` runs pinned fingerprints, Unicode, trust-boundary
and pure-reduction tests. Cross-language tests skip without `FORESTREAD_CONTRACT_VECTORS` and
`FORESTREAD_PROJECTION_VECTORS`; use the full runner for current parity evidence.
Stage 2D10 adds the inactive [`readerstore`](../readerstore/README.md) adapter for durable receipt,
mirrors and bounded transactional snapshots. Its off-writer preparation uses full validation;
`CheckPrepared` is the on-writer gate for that privately owned validated data, avoiding repeated
ink validation while holding the writer. Ordinary `Check` still performs validation itself.
HTTP reader interoperability, coordinated production migrations, server recognition, search and
activation remain separate work.
