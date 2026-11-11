# Lanescape Actions

GitHub Actions for the repositories of the `lanescape` organization, for use with its runner service, [`lanescape/runners`](https://github.com/lanescape/runners). Workflows name the service's machines in `runs-on` by shape alone, and the service binds each run itself. The actions build the image a run's commit defines and output its reference for later jobs' containers, ask for a job's work disk, check out, and ask to save the work disk; outside the organization, in a fork or another organization, they do nothing of the service's.

The code is public so that anyone can read it and contribute, but the service the actions call serves only the `lanescape` organization.

- [0001 Actions](specs/0001-actions.md): runner labels, contributions, `image`, `disk`, `checkout` and `save-disk`, and how each degrades outside the organization.
- [0002 Code, tests and releases](specs/0002-releases.md): Go, tests against `lanescape/runners`' emulator of the runner service and its other emulators, and the `v1` tag only the release App can move.

So far `runners` and `checkout` are built. The specs now replace `runners` with `image` and `disk`, and `save` with `save-disk`; those come next.

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
