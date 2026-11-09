# Lanescape Actions

GitHub Actions for the repositories of the `lanescape` organization, for use with its runner service, [`lanescape/runners`](https://github.com/lanescape/runners). They work anywhere: inside the organization they use the service's machines, saved disks and proxy; outside it, in a fork or another organization, they fall back to GitHub's runners, or to a fork's own, and read straight from upstream.

The code is public so that anyone can read it and contribute, but the service the actions call serves only the `lanescape` organization.

- [0001 Actions](specs/0001-actions.md): `runners`, `checkout`, `save`, `proxy` and `fetch`, and how each degrades outside the organization.
- [0002 Code, tests and releases](specs/0002-releases.md): Go, tests against `lanescape/runners`' emulator of the runner service and its other emulators, and the `v1` tag only the release App can move.

So far `runners` and `checkout` are built; `save`, `proxy` and `fetch` come next.

## Development

Go is pinned by `go.mod`'s `toolchain` line. Each action's `action.yml` and program are in `<name>/`, and what they share is in `internal/`.

```sh
go vet ./... && go vet -tags e2e ./...
gofmt -l .
go test -race ./...         # with the end-to-end tests against the service emulator
go test -short -race ./...  # without them
```

The end-to-end tests in `e2e/` run the built programs against `lanescape/runners`' emulator of the runner service, `emulators/service`, at the version `go.mod` names, and that repository's GitHub emulator (specs/0002-releases.md).

Licensed under the [MIT License](LICENSE).
