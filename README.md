# snowdeploy

A small, merge-first deployment layer for Podman Quadlet services managed by a
rootless `systemd --user` instance on a single host.

`snowdeployd` is a daemon that watches a Git configuration repository of
service manifests and a container registry, and turns "deploy this digest"
into a pull request, a merge, a rendered Quadlet unit, a restart, a health
probe, and an append-only journal entry — rolling back automatically when the
probe fails. `snowdeploy` is the CLI that drives it, and doubles as the
manifest validator a configuration repository's own CI can run.

Status: pre-release. See [`docs/architecture.md`](docs/architecture.md).

## License

MIT — see [LICENSE](LICENSE).
