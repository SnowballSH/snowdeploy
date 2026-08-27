# Converge action

2026-08-27. Motivated by the snowsys Phase 12 budget red test: a manifest
change that only edited `env` (a new `BUDGET_MONTHLY_USD`) merged to main, but
`Engine.start()` refuses any request whose digest equals the manifest pin
(`ErrAlreadyAtDigest`), so the rendered unit on the host kept the old
environment until the next digest change. Observed live 2026-08-27.

## Problem

The engine's only two actions — deploy and rollback — are both "pin a digest".
A manifest edit that changes env, volumes, or the template without moving the
digest has no path to convergence: nothing re-renders the unit from the merged
manifest.

## Approaches considered

1. **Explicit `converge` action (chosen).** A third engine action that applies
   the merged manifest as main has it right now: sync, render, write the unit,
   restart, probe. No pull request — the change it applies already merged
   through the configuration repository's own review. Distinct intent, small
   surface, honest journal receipts.
2. **Same-digest deploys fall through to apply when the rendered unit would
   differ.** Requires reading the current unit back from the host (a new
   `Systemd` surface) and comparing renders, and it muddies the deploy
   contract ("pin a digest") with a second meaning. Rejected.
3. **Automatic unit-content drift reconciliation.** A watcher that re-renders
   every manifest and applies on difference. A standing-behavior change with
   its own failure modes; far beyond what the incident needs. Rejected.

## Design

### Engine

- `journal.ActionConverge = "converge"`.
- `Engine.Converge(ctx, service, actor) (id int64, joined bool, err error)`,
  wired through the existing `start()`: the resolve function returns the
  current pin, and the `ErrAlreadyAtDigest` guard is skipped for converge
  (equality is the point). `setDigest` is skipped; there is no content change
  to propose.
- Deduplication: `pendingRun` gains the action. A second converge click joins
  the pending converge run; it never joins a deploy that happens to carry the
  same digest, because a deploy's apply may predate the manifest edit the
  converge was clicked for.
- The run reuses `run()`'s prologue — detected event, sync, per-service lock
  (the one-run-per-service queue) — then branches into a converge path that
  never touches the merge queue (`mergeMu`): converge opens no pull request,
  so it has no open-to-merge span to serialize.
- At the front of the queue the converge **syncs again** and then re-reads the
  manifest. The prologue sync predates the queue wait, so the mirror can be
  stale by the time the converge's turn comes — or still pinning a digest a
  revert since took back. The sync failure wording goes through the same
  `syncFailure` path as every other sync, so the credential-bearing transport
  error never reaches the journal.
- If interleaved deploys moved the pin while the converge waited, converge
  applies the new pin — host matches main is the whole contract — and rewrites
  the still-unfinished journal entry's digests to the pin actually applied
  (`Journal.SetDigests`, which refuses finished receipts). Receipts stay
  immutable; an in-flight entry is not yet a receipt. Without the rewrite, a
  healthy converge row carrying the click-time pin would feed
  `PreviousHealthyDigest` a digest that was never verified by this run.
- States: detected → reconciling → probing → healthy/failed. The UI's
  progress rail already renders runs that skip pr-open/checks/merged (the
  same-digest deploy path produces the same shape).
- `applyMerged` is split: the template-fetch-and-apply tail becomes
  `applyManifest(ctx, req, m)`, shared by the deploy path and converge.
  Converge does not use `applyMerged` itself — its digest assertion is against
  the Begin-time pin, which converge deliberately does not hold fixed.

### No automatic rollback

Deploy can roll back because it holds the click-time manifest (`originalRaw`).
For converge, the click-time manifest *is* the merged manifest being applied;
the previously rendered unit's content is recorded nowhere. A converge whose
probe fails finishes `failed`, with a detail saying exactly that: there is no
previous unit to restore — fix the manifest and converge again, or deploy a
known-good digest. `LastHealthyDigest` is untouched by a failed run, so a
subsequent bare rollback still resolves sensibly.

A healthy converge journals `new_digest` = the current pin with state healthy,
which is true and keeps the healthy-digest queries correct.

### API

`POST /api/v1/services/{name}/converge`, authenticated like deploy and
rollback, no request body (any body is ignored — there is no digest to accept,
and accepting one would invite the same-digest confusion converge exists to
end). Responds 202 with `{journalId, joined}` via the shared `dispatch`.

### CLI

`snowdeploy converge <service>` — the followed-command flow (subscribe to
events first, POST, stream to a terminal state, exit nonzero unless healthy),
minus the digest flag and the latest-digest resolution. `client.startDeploy`
sends no body for converge.

### Out of scope

- A converge button in the web UI. The UI renders converge runs arriving on
  the event stream (action rides on every event and history row); adding a
  button is a separate, purely frontend change.
- Skipping the restart when the rendered unit is byte-identical to what is on
  the host. Converge is a deliberate operator action; an occasional no-op
  restart, gated by the probe, is acceptable. Revisit only if converge grows
  an automated caller.

## Tests

Engine (`internal/deploy/engine_test.go` harness):

- Converge applies the current manifest with no PR opened, states
  detected→reconciling→probing→healthy, journal receipt action=converge.
- Converge applies the manifest as it stands at the front of the queue after
  an interleaved deploy moved it, and its receipt records the applied pin.
- Converge syncs the mirror at the front of the queue: an edit that lands on
  main after the prologue sync is still applied (the fake repo keeps main and
  the fetched mirror apart so staleness is visible at all).
- A front-of-queue sync failure fails the run in the repository's own words,
  never the transport error.
- Converge probe failure finishes failed, applies nothing else, opens no PR,
  and leaves LastHealthyDigest unchanged.
- A duplicate converge click joins the pending converge run.
- A converge does not join a pending deploy carrying the same digest.
- Sync failure fails the run without leaking the transport error (existing
  wording path).

API (`internal/api/api_test.go`):

- POST converge dispatches to the engine and returns 202 with the journal id;
  unknown service 404s; unauthenticated 401s (existing middleware table).

CLI (`cmd/snowdeploy/client_test.go` / main): converge posts to the right
path with no body and follows to a terminal state.
