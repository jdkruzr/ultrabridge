# Shared library host

`libraryhost` owns the enrolled mixed-library HTTP assembly and projection/search
lifetime. `cmd/assetlab --reader --reader-assets --reader-enrollment` now uses it;
the host contains no fixture credentials, loopback listener or test-only routes.
`readerlab` retains only explicit A/B fixture authentication and CLI projection.
Production `cmd/ultrabridge` does **not** mount this host yet.

- `EnrolledHandler` shares bounded receipt, assets, search and identity routing.
  Account Basic credentials can enroll/revoke; only a bound device key can exchange
  rows/assets. Writer-page notifications run after commit; reader hints remain
  nonblocking and durable worker recovery does not depend on them.
- `New` installs the explicit enrolled host. Restore is off unless requested.
  When enabled, publication/adoption/receipt and ordinary device paths share one
  owner. There must be no separate legacy row route bypassing this owner.
- `Do` admits synchronous/manual library work. Replacement blocks new admission,
  joins old requests, then cancels and joins all registered workers. Failed
  publication restarts fresh workers too. Projection and search objects are both
  recreated, not merely resumed with old in-memory cursors.
- `Auxiliary` joins a production source's writer bridge/backfill/other materializers
  into that lifetime. Validate dependencies first; the factory must not fail.
- `ReplaceDerived` runs in the publication SQL transaction. It must touch only
  this library's derived rows, perform no network calls or cache changes, and
  return errors so the whole publication rolls back. `AfterReplace` invalidates
  in-memory state only after commit, before workers restart.
- Closing cancels and joins admitted requests, including restore staging, before
  joining workers. The caller closes the shared database afterward. Neither a
  closed worker owner nor a cancelled parent can restart workers via replacement.
  Request-body reads have a 30-second deadline; shutdown expires socket I/O
  deadlines as well as cancelling contexts, including partially sent bodies.
  Production request logging now exposes `Unwrap` so these deadlines reach the
  real socket; the slow-body test runs through that actual middleware.

Tests use a disposable database opened by the real `notedb` migrations. They cover
account/device separation, disabled restore, committed writer notification,
manual-request and auxiliary-worker joining, rollback/restart, stalled HTTP-body shutdown, actual
publication, unrelated-source derived/cache preservation, peer supersession after
restart, receipt recovery and adoption. No paid OCR or embedding calls are needed.

## Remaining production connection

Wire this owner into the ForestNote source behind an explicit default-off source
capability; mount enrollment/rows/assets/search/restore together. Connect source
manual work, global embedding backfill admission and prefix-scoped derived/cache
invalidation. Source manual mutations now join its existing background lifetime;
`rag.Store.SetBackfillAdmission` and `ForgetPrefix` provide the global-backfill and
cache seams. They are not automatically connected by constructing this package.

Global backfill re-reads each page after admission because its initial inventory
can predate replacement. Connect that admission before starting global jobs. Do
not use a restored search cache to author new server OCR or make a blanket OCR
request for every restored page. Retained transcription provenance stays intact.

Qualify production source restart/index rebuilding, default-off legacy operation,
cutover/downgrade fencing and the actual executable routes before live activation.
Unrelated sources, settings, tasks and credentials remain outside replacement.
