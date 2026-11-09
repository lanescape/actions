# Code, tests and releases

## Code

Go, at the latest stable release, pinned by `go.mod`'s `toolchain` line. One module, `github.com/lanescape/actions`, with each action's `action.yml` and program in `<name>/`. It depends on `github.com/lanescape/runners` at a version `go.mod` names, for the interface's constants in its `endpoints` package and for the emulators the tests run against. The service's interface for actions, its spec 0005, is what the actions may rely on; nothing else of the service's.

## Tests

Every behaviour in specs/0001-actions.md has a test. No test runs or calls the real runner service. They run against `lanescape/runners`' own emulators, at the version `go.mod` names, never anyone else's: its emulator of the service's interface for actions, `emulators/service`, which serves binding, and its emulators of GitHub and GitHub's OIDC issuer, as that repository's spec 0005 (Testing against the emulator) asks of consumers. Its conformance suite, `conformance/`, runs the same cases against that emulator and against the real service in its own CI, and both must pass every one, so what the actions do against the emulator at a version is what they do against the service at that version, as far as the interface goes. The actions rely on that suite for it, and don't run the service themselves. End-to-end tests run each action as GitHub would, inside the organization and outside it: a pull request's run through to its saves, a fork's run falling back to GitHub's runners, a fork's pull request into an organization repository, an approved contribution, and the service answering that GitHub couldn't be reached. The actions' tests run natively on Blacksmith's Linux amd64, Linux arm64 and macOS arm64 machines, since paths, shells, signals and file locking differ between them, and CI builds every program for every platform it targets.

Moving to a newer `lanescape/runners` is an ordinary pull request that changes `go.mod`, so the tests show the actions still work with that version's emulator, which that version's conformance suite holds to its service, before anything relies on it.

## CI and releases

Workflows here run on Blacksmith, never on the runner service's own machines, so a broken service can never stop its actions being fixed. A pull request from a fork runs on GitHub's runners instead; it needs no credentials, since nothing in CI does.

- **`ci.yml`, on pull requests:** `go vet`, `gofmt` and the tests.
- **`release.yml`, on pushes to main:** the same, then it moves `v1` to the commit. It runs one at a time, and moves `v1` only forward: the new commit must descend from the one `v1` names.

GitHub's rulesets name who may bypass them by account or App, not by workflow, and the ordinary `GITHUB_TOKEN` of any workflow acts as the same GitHub Actions App, including a same-repository pull request's workflow, which may ask for write access. So `v1` is moved by a GitHub App of its own, "Lanescape Actions", installed only on this repository, with Contents read and write and nothing else. Its private key is a secret of the `release` environment, which only the `main` branch may deploy to, so only workflows on `main`, which are reviewed, can use it. `release.yml` is the only one that does. No job that uses the `release` environment ever runs a contribution's code, or any code not on `main`. A tag ruleset stops anyone but that App creating, updating or deleting `v*` tags, so moving `v1` takes the same review as merging to main.

That matters because these actions run inside consumers' own jobs, beside whatever those jobs hold: whoever can merge here can change code in every consumer's jobs, which is the risk of a moving tag, and why moving it takes a merge.

Whoever merges to main releases. No agent reviews, approves or merges a pull request without the explicit instruction of the person it works for, for that pull request.
