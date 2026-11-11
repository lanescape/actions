# Actions

The `lanescape` organization's repositories use its runner service, `lanescape/runners`, through five actions in this repository, `lanescape/actions/<name>@v1`. They call the service through the interface it publishes and keeps compatible, its spec 0005, "The interface for actions", which is all they may rely on. Each works inside the organization and degrades gracefully outside it, so the same workflow runs in a fork, at GitHub's expense, as far as it can without the service.

## Where an action is

Each action decides the same way, and says which in the job's summary:

- **Inside the organization:** `github.repository_owner_id` is the organization's immutable ID, `endpoints.Organization` in the service's interface, never compared by name, and either the run isn't from a fork's pull request, by the event's head repository, or it's a `pull_request_target` run, whose jobs the service accepts only once a maintainer approves them.
- **Outside:** anything else, including a fork's own pushes, another organization, and a fork's pull request into one of the organization's repositories, which runs in the base repository but never on the service's machines.

A run is a fork's pull request's when its event's payload has a pull request, as `pull_request`, `pull_request_review` and `pull_request_target` events do, whose head repository, by ID, isn't the run's repository, or is gone. The owner's ID is `GITHUB_REPOSITORY_OWNER_ID`, or the payload's repository's owner's if that isn't set.

The decision is a courtesy, not a guard. The service and the proxy check everything for themselves, from GitHub's records and signed tokens.

## `runners`

Makes the labels a workflow's jobs run on. A consumer lists the runners it uses in `.github/runners.json`, each with its fallback for outside the organization:

```json
{
  "lanescape-80vcpu-linux-amd64": "ubuntu-24.04",
  "lanescape-2vcpu-linux-amd64": "ubuntu-24.04"
}
```

A workflow's first job runs `runners` on one of GitHub's own runners, since no label is known yet, and every other job takes its label from that job's output. The action reads `.github/runners.json` from the run's own commit through GitHub's API, with the job's token, so it needs no checkout; in a `pull_request_target` run, that's the default branch's. Inside the organization it also binds the run's scope with the service, by `POST /v1/bind`, so it must be the run's first job, with `contents: read` to read the file and `id-token: write` to sign in. In a `pull_request_target` workflow, the job instead calls the consumer's bootstrap workflow, which runs this action and nothing else, and whose reference the consumer names in `.github/runners.json` under `"bootstrap"`:

```yaml
jobs:
  runners:
    runs-on: ubuntu-24.04
    permissions:
      contents: read
      id-token: write
    outputs:
      map: ${{ steps.runners.outputs.map }}
    steps:
      - id: runners
        uses: lanescape/actions/runners@v1
        with:
          disks: '{"build": {"runner": "lanescape-80vcpu-linux-amd64", "spot": true, "from": ["deps/versions.json", "rust-toolchain.toml"]}}'
          fallbacks: ${{ vars.RUNNERS }}
  build:
    needs: runners
    runs-on: ${{ fromJSON(needs.runners.outputs.map)['build'] }}
```

Its inputs are `disks`, below; `fallbacks`, the `RUNNERS` variable, below, which the workflow passes as `${{ vars.RUNNERS }}`, since GitHub gives a composite action no `vars` context of its own; and `token`, which reads the files and defaults to the job's own, `github.token`.

`.github/runners.json` is the format lanescape/runners' spec 0005 gives: each runner name is a shape, `lanescape-<vCPUs>vcpu-linux-<amd64|arm64>` in lowercase, and each fallback is a label or a non-empty list of labels; `"bootstrap"`, if present, is a string; and `"branches"`, if present, is an object from each long-lived branch's name to the name of its scope, 1 to 16 lowercase letters and digits, starting with a letter, which no two branches share: `{"support/v1": "v1"}` gives pushes to `support/v1` the scope `branch-v1`. Only the service uses those two, and only from the default branch; the action checks them wherever it reads the file, so a mistake shows in the run that makes it. The action fails, saying why, on anything else, including a name given twice.

Each entry in `disks` is a saved work disk the workflow's jobs use, named by its family, a name of 1 to 32 lowercase letters, digits and hyphens starting with a letter, and has:

- **`runner`:** the name of a runner in `.github/runners.json`. Required.
- **`spot`:** `true` to run on Spot machines, which can be preempted, so the job must be able to carry on from its work disk on a rerun. Standard machines otherwise, or if it's left out.
- **`from`,** optionally: the files the disk's contents depend on, such as a list of dependency versions and a toolchain file. The action reads them through GitHub's API at the commit the job builds: the run's own commit, by its SHA, never a moving ref; and in a `pull_request_target` run, the contribution's own, `github.event.pull_request.head.sha` in the head repository, which is what the contribution's jobs check out, even though `.github/runners.json` is read from the default branch there. A listed file that doesn't exist fails the action, naming it.

  The key is the first 20 hex digits of the SHA-256 of these parts, in this order: the family, the runner, then each listed path and its contents, in the order listed. Each part is written as its length in bytes, an unsigned 64-bit big-endian number, followed by its bytes: names and paths in UTF-8, and contents exactly as the file's bytes at that commit. For example, family `build`, runner `lanescape-80vcpu-linux-amd64`, `deps/versions.json` holding `café 1.0` and a newline, and an empty `rust-toolchain.toml`, give the key `a77434447088c41f6e9f`. So the key changes when a listed file's contents do, and also when the family, the runner, the paths or their order do.
  A listed path is relative to the repository's root, with no leading or trailing slash, no `.`, `..` or empty segment and no backslash, and is listed once; `from` lists at least one.
- **`key`,** optionally, instead of `from`: the key itself, 20 lowercase hex digits, for a disk whose inputs aren't files in the repository.

An entry with any other field, a field named in another case, both `from` and `key`, or a family that is also a runner's name in `.github/runners.json`, fails the action. The action reads the listed files and checks every entry outside the organization too, though the map there shows no keys, so a contribution's mistake fails the same way in its fork's own runs.

With neither `from` nor `key`, the family has one key per scope and architecture, `00000000000000000000`, since the service's selectors always carry one. A save that the service accepts makes a newer version of that disk, and later runs restore the newest. That's enough for most jobs, since a build redoes only what's out of date. A save doesn't pass a disk to a later job of the same run: the service picks each run's disk once, for all its jobs with the same selector.

Whatever the key, a job restores by the service's order, along its run's restore chain: in the chain's first scope, the disk for that key, then the newest of the family; then the same in its base, if it has one; then an empty disk. A pull request's chain is its own scope, then its base, the long-lived branch it targets if the consumer lists it, or else main's; a long-lived branch's push reads its own, then main's; main's pushes and `read` runs read main's. A pull request that changes a listed file gets a new key, so it saves beside main's disk rather than over it. A pull request keeps one disk of each family, so its saving one replaces its others of that family: a family is one lineage, and jobs that need several keys alive at once use separate families.

The output `map` has an entry for each runner name in `.github/runners.json`, which is every key but `bootstrap` and `branches`, the service's own settings (`lanescape/runners`' spec 0005, A consumer's files), and one for each entry in `disks`, named by its family:

- **Inside the organization,** each runner name maps to its selector for this run, with the scope the binding returned, `main`, a long-lived branch's such as `branch-v1`, a pull request's such as `pr42`, or `read`: `lanescape-80vcpu-linux-amd64-pr42`. Each disk entry maps to its runner's selector with `-spot` if asked and `-disk-<family>-<key>`, with the key it was given or made: `lanescape-80vcpu-linux-amd64-spot-disk-build-3f9c2e7a10b4d5e6f708-pr42`. The service checks every selector against the run's binding before a job runs. The binding's body is empty, except in a `pull_request_target` run, where it's what the event says: the pull request's number, its head repository's ID and its head commit; the service grants the contribution's scope only to the bootstrap workflow's job, and `read` to any other. The job's OIDC token, with the service as its audience, is got afresh for each attempt. If the binding answers `503`, retryable, since GitHub couldn't confirm an answer or the service couldn't read GitHub's signing keys, the action retries with backoff, and fails only once its retries are spent: eight attempts in all, waiting 2 seconds, then twice as long each time up to a minute, or longer if `Retry-After` asks, up to a minute. It retries the same way when the service can't be reached, or a server in front of it answers 429 or a 5xx; any other answer outside the interface fails it at once, and a refusal fails it with the service's reason.
- **Outside,** each runner name maps to its fallback, a label or a list as written, and each disk entry to its runner's fallback, after `RUNNERS`. A fork can override any fallback with a repository or organization variable `RUNNERS`, a JSON object in the same shape as `.github/runners.json`, whose values may be a label or a list of labels. Names it doesn't override keep the file's fallback, and names the file doesn't list as runners, `bootstrap` and `branches` among them, are ignored. For a fork's pull request into an organization repository, the action ignores `RUNNERS` from the base repository, since it's the organization's.

A job that asks for a name the map lacks gets an empty label, which GitHub refuses; the action's summary lists the names it has, with their labels.

## `checkout`

Checks out the repository for a job. With no inputs, it checks out the run's own commit, as `actions/checkout` does. With `contribution: true`, in a `pull_request_target` run, it checks out exactly `github.event.pull_request.head.sha`, the commit the approved run was made for, from the head repository, with no credentials persisted, and refuses in any other event. Outside the organization, `contribution: true` does the same in a fork's own `pull_request_target` runs, which use nothing of the service's.

It checks out with `actions/checkout`, pinned by its commit, in the job's workspace, so on the service's machines too the checkout is on the boot disk, never `/work`, and does the same on any runner. The action's program decides what to check out, and passes `actions/checkout` its own defaults for the run's own commit, credentials persisted as there; for a contribution, the head repository and commit, `persist-credentials: false`, and `allow-unsafe-pr-checkout: true`, which `actions/checkout` asks for before it checks out a fork's commit in a `pull_request_target` run. Afterwards the program confirms that the checkout's `HEAD` is that commit, and that its git config keeps no `extraheader`, `includeIf` or `credential` entry, and fails otherwise. `contribution` is `true` or `false`; a contribution whose head repository is gone is refused.

## `save`

Asks for the job's work disk to be saved when it finishes, by writing the service's save request on the machine. The service saves the disk if the job succeeded and its scope and selector allow. Outside the organization, or on a machine that isn't the service's, it does nothing and says so.

## `proxy`

Points a job's reads of upstream sources at the service's proxy. It reads the consumer's `.github/proxy.json` from the default branch through GitHub's API, as the service does. On the service's machines it uses the forwarder the machine already runs, and configures git to fetch the consumer's allowlisted git upstreams through it. On other runners it builds the service's forwarder from the version of `lanescape/runners` this repository's `go.mod` names and starts it, for HTTP reads only, since the proxy's git mirror is reachable only from the service's machines; git there reads straight from upstream. Where the proxy won't serve the job, it does nothing, and reads go straight to upstream: outside the organization, and on runners other than the service's for a `pull_request_target` run, whose GitHub token the proxy refuses. Tools other than git use `fetch`, or the forwarder's address, which the action sets as `LANESCAPE_PROXY`. The job needs `contents: read`, to read `proxy.json`, and `id-token: write`, for the forwarder's token on runners other than the service's; without them the action fails, saying which is missing.

## `fetch`

Downloads a URL through the proxy to a file, with an optional expected SHA-256, which it checks before writing the file. It needs the same permissions as `proxy`, and starts the forwarder the same way if `proxy` hasn't. Outside the organization, or for an upstream the consumer hasn't allowlisted, it downloads straight from upstream, and still checks the SHA-256.

## How actions run

Each action is a composite action that builds its Go program from the action's own source with `go build`, run in the repository root of the action's own downloaded copy, `$GITHUB_ACTION_PATH/..`, into `$RUNNER_TEMP`, and runs the binary. What runs is exactly the code at the commit the action was referenced at, with no separate release to keep in step, and the job sees the program's own exit status, which `go run` wouldn't give. The service's base image has Go; elsewhere, the action sets up that version with `actions/setup-go` from `go.mod`. Which applies is told by the Go on the machine: the action uses it if `go env GOVERSION` is exactly `go.mod`'s `toolchain` version, and otherwise sets that exact version up, named as `go-version` so that no `GOTOOLCHAIN` the job has can make `actions/setup-go` pick another, with its cache off, since a consumer's workspace has no `go.sum` of ours. The build fails if Go still isn't that version. `actions/setup-go` puts its Go first on the job's `PATH` for the steps after, so a job that sets up a Go of its own does so after these actions. Builds use Go's build cache, so later steps of a job build in moments. A build runs with `GOENV=off`, `GOWORK=off`, `GOTOOLCHAIN=local`, `GO111MODULE=on` and `CGO_ENABLED=0`, and with `GOOS`, `GOARCH`, `GOAMD64`, `GOARM64`, `GOFLAGS`, `GOEXPERIMENT`, `GOROOT`, `GODEBUG` and `GOFIPS140` cleared, so a job's own Go settings, in its environment or saved with `go env -w`, can't change what's built, into a new directory under `$RUNNER_TEMP`; `actions/setup-go`, which runs Go itself, and the check for Go run with the same settings, and `actions/setup-go`, which prints `go env`, also without the job's module and cache settings, such as a `GOPROXY` that holds a credential, and without `GO_DOWNLOAD_BASE_URL`, so it downloads Go from Go's own site rather than print a mirror's URL; it downloads this module's dependencies through the `GOPROXY` in the job's environment, if any, and otherwise Go's own. A composite action gets no `INPUT_` variables, so each `action.yml` passes its inputs to its program as `INPUT_<NAME>` in the step's environment, and never writes an expression into a script. The programs are pure Go, with CGO off, and CI builds them for every platform runners use: Linux on amd64 and arm64, macOS on arm64, and Windows on amd64.

## Versions

Consumers reference `@v1`, a tag that moves (specs/0002-releases.md). `v1` stays compatible: its actions keep their inputs, outputs and behaviour. A change that would break them comes as `v2`, while `v1` keeps working, until no consumer references it.
