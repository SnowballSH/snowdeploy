# Architecture

## The shape of the thing

snowdeploy is a reconciler, not a supervisor. It does not keep services
running — systemd does that. It makes the running state match a Git branch,
one service at a time, and records what happened.

The load-bearing decision is **merge-first**: the daemon never applies a change
it has not first merged into the configuration repository's default branch.
That inverts the usual deploy tool. Rather than acting and then recording, it
proposes, waits for the repository's own required checks, merges, and only then
touches the host. A red check means the host was never involved.

## Packages

| Package | Responsibility |
| --- | --- |
| `internal/manifest` | The manifest schema, strict parsing, and both halves of the safety model: what a manifest may say, and what a rendered unit may contain. |
| `internal/journal` | The append-only SQLite deploy record. A finished entry is a receipt and is immutable. |
| `internal/registry` | Polls container registries for the digest a tag currently resolves to. A failed poll keeps the previous answer. |
| `internal/gitops` | Two halves: a local mirror of the merged branch (`repo.go`), and the GitHub App pull-request flow (`github.go`). |
| `internal/reconcile` | Render a template into a unit, validate the render, write it, restart, probe. |
| `internal/deploy` | The state machine that sequences all of the above, including auto-rollback. |
| `internal/api` | The JSON API, the SSE stream, authentication, and the Prometheus surface. |
| `internal/config` | Loads and validates the one configuration file. |
| `web` | The built single-page app, embedded. |

Dependencies point one way. `deploy` consumes `gitops`, `reconcile`, and
`journal` through interfaces it declares itself, so every dependency can be
faked in a test without a network, a registry, or a container runtime.

## The state machine

```
        ┌──────────────────────────────────────────────┐
        │                                              │
   detected → pr-open → checks → merged → reconciling → probing
                          │                                │
                          │ checks red                     │ probe red
                          ▼                                ▼
                        failed                        rolled-back
                                                    (or failed, if the
                                                     rollback also fails)
```

Only `healthy`, `rolled-back`, and `failed` are terminal, and those three
constants live in `internal/journal` — `internal/deploy` aliases them, so a
live event and a stored receipt can never disagree about what "healthy" means.

Each transition writes the journal's detail column and publishes an event.
Subscribers (the web UI, the CLI, the metrics recorder) see the same stream.

### What happens when the probe fails

1. The previous manifest bytes — captured before the change was proposed — are
   re-applied, returning the host to the digest that was running.
2. A revert pull request restores that manifest on the default branch, so the
   branch describes what runs. It is retried with bounded backoff.
3. The receipt records `rolled-back`, with the original failure and the revert
   outcome both in its detail.

If step 1 fails too, the receipt is `failed` and carries both errors. Nothing
is retried silently and nothing is hidden: there is no alert transport, so the
journal, the event stream, and the metrics are the whole notification path.

## The safety model, and where it is enforced

The manifest checks (`Manifest.Validate`) constrain what an operator can write.
The rendered checks (`ValidateRendered`) constrain what a *template* can
produce — a separate problem, because a clean manifest rendered through a
hostile or careless template could still yield a privileged container or a
public listener.

`reconcile.Applier.Apply` runs them in this order:

```
Validate(manifest) → Render → ValidateRendered → WriteUnit → Restart → Probe
```

The ordering is the control. `ValidateRendered` precedes `WriteUnit`, so a
refused render never reaches the unit directory at all — and the test that
proves this asserts the fake systemd recorded *no* write, not merely that an
error was returned.

## Concurrency

One deploy per service at a time, enforced by a per-service channel used as a
mutex. A second request for the same service queues; requests for different
services proceed in parallel. `Deploy` and `Rollback` return as soon as the
journal entry exists, so an HTTP request never blocks for the length of a
deploy; the work continues under the daemon's lifetime context, not the
request's.

## Drift

A periodic reconciler compares each service's running image digest against its
merged manifest and publishes a per-service gauge. Drift is expected after a
manual intervention and after a rollback whose revert pull request has not
landed yet — it is information, not an error, and it is deliberately not
self-healing: silently re-applying a manifest over something a human did by
hand would destroy the evidence of why they did it.
