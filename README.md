# snowdeploy

A small, merge-first deployment layer for Podman Quadlet services managed by a
rootless `systemd --user` instance on a single host.

`snowdeployd` watches a Git repository of service manifests and a container
registry. One click — or one CLI call — turns "deploy this digest" into a pull
request, a merge behind that repository's own required checks, a rendered
Quadlet unit, a restart, a health probe, and an append-only receipt. A probe
that fails rolls the service back automatically and opens a revert pull
request, so the repository keeps describing what is actually running.

`snowdeploy` is the CLI. Its `validate` subcommand needs no daemon and no
credential, so a configuration repository's own CI can run it.

## Why it is shaped this way

- **Merge-first.** The host only ever applies merged state. Nothing reaches a
  service that has not passed the configuration repository's required checks.
- **Least privilege.** The daemon runs as an unprivileged user and owns only
  that user's services. It cannot reach root, the reverse proxy, the firewall,
  the VPN, the backups, or the secret store.
- **No standing credential.** The GitHub App key is read from disk per
  operation and never cached; installation tokens are minted per operation and
  never persisted. If the key is unavailable — for example because a secret
  store is sealed — deploys fail closed with a clear error while the daemon
  keeps serving reads and running services are untouched.
- **Nothing is pushed.** Outcomes surface in the UI, the CLI, the journal, and
  Prometheus metrics. There is no alert transport to configure or to page you.

## Install

Download the `linux/amd64` binaries and `SHA256SUMS` from a
[release](https://github.com/SnowballSH/snowdeploy/releases), verify them, and
install `snowdeployd` where a `systemd --user` unit can run it. Building from
source needs only Go — the web UI is committed pre-built and embedded.

```bash
sha256sum --check SHA256SUMS
```

## Configuration

Every site-specific value lives in one YAML file; the binary carries none of
them. `example.config.yaml` is the annotated reference, and the loader rejects
unknown keys so a typo can never silently disable a setting.

| Key | Meaning |
| --- | --- |
| `listen` | API and SSE address. Must be loopback. Default `127.0.0.1:8092`. |
| `metrics_listen` | Prometheus address. Must be loopback, and differ from `listen`. Default `127.0.0.1:9105`. |
| `repo_url`, `repo_branch` | The configuration repository and the branch that is the source of truth. |
| `manifest_dir`, `template_dir` | Where manifests and Quadlet templates live in that repository. |
| `cache_dir` | Local mirror of the merged branch. Reset hard on every sync. |
| `unit_dir` | Where rendered `.container` units are written for this user's systemd. |
| `journal_path` | The SQLite deploy record. Back this up. |
| `github_app_id`, `github_install_id` | The App installation allowed to author manifest pull requests. |
| `github_key_file` | The App private key. Read per operation, never cached. |
| `github_owner`, `github_repo` | The repository the App may write to. |
| `cli_token_hash_file` | SHA-256 hashes of accepted CLI tokens, one per line, each optionally followed by a label used as the journalled actor. |
| `poll_interval` | How often the registry is asked for new digests. |
| `drift_interval` | How often running containers are compared to merged manifests. |
| `check_poll_interval` | How often a pull request's checks are polled. |
| `probe_interval` | How often a health probe retries within its deadline. |
| `volume_prefixes` | Host path prefixes a manifest's volumes may use. Empty disables the check. |
| `env_file_prefixes` | Host path prefixes a manifest's env files may use. Empty disables the check. |

## Manifests

One file per service, named for the service:

```yaml
name: web
image:
  repository: registry.example.com/acme/web
  digest: sha256:0000000000000000000000000000000000000000000000000000000000000abc
template: web.container.tmpl
port: 8080
network: acme
env:
  MODE: production
env_files:
  - /etc/acme/web.env
volumes:
  - /srv/acme/web:/data:Z
health:
  url: http://127.0.0.1:8080/healthz
  timeout: 45s
```

Templates are ordinary `text/template` files rendered from these fields into a
Quadlet `.container` unit.

## API

All JSON, all under `/api/v1`, all on the loopback listener.

| Method | Path | Result |
| --- | --- | --- |
| `GET` | `/api/v1/services` | Every service with its running, merged, and latest-available digests, drift flag, and last receipt. |
| `POST` | `/api/v1/services/{name}/deploy` | `{"digest": "sha256:…"}` → `{"journalId": N}`, `202`. |
| `POST` | `/api/v1/services/{name}/rollback` | `{"digest": "sha256:…"}` (optional) → `{"journalId": N}`, `202`. |
| `GET` | `/api/v1/services/{name}/history?n=20` | Newest-first receipts. |
| `GET` | `/api/v1/events` | Server-sent `state` events for every transition. |
| `GET` | `/healthz` | `200`, no identity required. |
| `GET` | `/metrics` | Prometheus, on the **metrics** listener only. |

Authentication is either `Authorization: Bearer <token>`, checked against the
hashes in `cli_token_hash_file`, or a `Remote-User` header set by the
authenticating proxy in front of the daemon. Neither present is `401`. The
authenticated identity is recorded as the actor on every receipt.

Because `Remote-User` is trusted, the listener must stay on loopback and must
only be reachable through that proxy. The daemon refuses to start on any other
address.

## CLI

```
snowdeploy status
snowdeploy deploy   <service> [--digest sha256:…]
snowdeploy rollback <service> [--to sha256:…]
snowdeploy history  <service> [-n 20]
snowdeploy validate <dir> [--volume-prefixes p1,p2] [--env-file-prefixes p1,p2]
```

`--server` and `--token-file` (or `SNOWDEPLOY_SERVER` and
`SNOWDEPLOY_TOKEN_FILE`) point it at a daemon. `deploy` and `rollback` follow
the event stream and exit non-zero unless the service ends healthy, so they
work in a script.

## Security model

- **Digest pins only.** A tag in a manifest is rejected. So is a repository
  with an embedded digest, or a digest that is not `sha256:` plus 64 hex
  characters.
- **Loopback publishes only.** A rendered unit publishing anywhere but
  `127.0.0.1:` or `[::1]:` is refused, as is `Network=host`, which would
  defeat the rule.
- **No privilege escalation.** `AddCapability`, `Privileged`, `AddDevice`, and
  `SecurityLabelDisable` are refused, as are the equivalent escapes through
  `PodmanArgs`.
- **The rendered unit is the thing checked.** Validation runs on the rendered
  text, before it is written, so a template cannot smuggle past the manifest
  rules. A rejected render never touches the unit directory.
- **Confined host paths.** Volumes and env files must resolve under the
  configured prefixes.
- **Immutable receipts.** A finished journal entry can never be rewritten.

## Development

```bash
go test -race ./...
golangci-lint run
cd web && npm ci && npm test && npm run build
```

`web/dist` is committed, so `go build` alone produces a working daemon. CI
fails if a web source change was not rebuilt.

## License

MIT — see [LICENSE](LICENSE).
