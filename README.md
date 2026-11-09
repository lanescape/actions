# Lanescape Actions

GitHub Actions for the repositories of the `lanescape` organization, for use with its runner service, [`lanescape/runners`](https://github.com/lanescape/runners). They work anywhere: inside the organization they use the service's machines and saved disks; outside it, in a fork or another organization, they fall back to GitHub's runners, or to a fork's own, and save nothing.

The code is public so that anyone can read it and contribute, but the service the actions call serves only the `lanescape` organization.

- [0001 Actions](specs/0001-actions.md): `runners`, `checkout` and `save-disk`, and how each degrades outside the organization.
- [0002 Code, tests and releases](specs/0002-releases.md): Go, tests against `lanescape/runners`' emulator of the runner service and its other emulators, and the `v1` tag only the release App can move.

Licensed under the [MIT License](LICENSE).
