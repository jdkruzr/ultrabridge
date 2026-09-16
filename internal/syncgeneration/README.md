# Deliberate shared-library replacement fence

Candidate/harness only; there is no public restore endpoint. See the cross-repo
[authoritative restore plan](../../../AragoniteAlexandria/docs/design-plans/2026-09-16-alexandria-authoritative-restore-sync.md).

One person owns the library. A deliberate restore replaces its shared state rather
than merging discarded history back in. Generations fence stale replica work, not users.

`syncidentity.Install` installs bindings once. `Bind` captures server-held admission;
incoming generation headers are informational, not authority. `CheckTx` revalidates
after taking the writer. It must precede *every* mutation. Candidate row exchange and
the asset store's optional transaction guard both do so; default/legacy routes remain
outside this candidate feature. Never publish a generation against an unfenced host.

`Publish` is an internal transaction primitive. Its caller must authenticate explicit
admin approval, validate and pin a staged snapshot, quiesce library workers, and supply
the actual content-replacement callback. The callback must preserve generation/identity
tables and unrelated UB data. Exact retry returns the prior receipt only while it is
still current; conflicting and superseded requests do not run the callback.

Snapshot staging/import, HTTP publication, worker quiescence and client baseline adoption
are still pending. **Do not expose Publish directly as a reset endpoint.** Behind-generation
devices are blocked; no code currently promotes them to the restored library automatically.

Tests: `go test -race ./internal/syncgeneration ./internal/syncidentity ./internal/readerlab ./internal/syncassets ./internal/syncstore`.
The asset hook source is identical in `~/rhizome/server-go/assets/sqlite.go` and UB's
vendored `third_party/rhizome-server-go/assets/sqlite.go`.
