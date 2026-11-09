# Actions

The `lanescape` organization's repositories use its runner service, `lanescape/runners`, through five actions in this repository, `lanescape/actions/<name>@v1`: `runners`, `checkout`, `save-disk`, `restore-cache` and `save-cache`. They call the service through the interface it publishes and keeps compatible, its spec 0005, "The interface for actions", which is all they may rely on. Each works inside the organization and degrades gracefully outside it, so the same workflow runs in a fork, at GitHub's expense, as far as it can without the service.

## Where an action is

Each action decides the same way, and says which in the job's summary:

- **Inside the organization:** `github.repository_owner_id` is the organization's immutable ID, `endpoints.Organization` in the service's interface, never compared by name, and either the run isn't from a fork's pull request, by the event's head repository, or it's a `pull_request_target` run, whose jobs the service accepts only once a maintainer approves them.
- **Outside:** anything else, including a fork's own pushes, another organization, and a fork's pull request into one of the organization's repositories, which runs in the base repository but never on the service's machines.

The decision is a courtesy, not a guard. The service checks everything for itself, from GitHub's records and signed tokens.

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
  build:
    needs: runners
    runs-on: ${{ fromJSON(needs.runners.outputs.map)['build'] }}
```

Each entry in `disks` is a saved work disk the workflow's jobs use, named by its family, a name of 1 to 32 lowercase letters, digits and hyphens starting with a letter, and has:

- **`runner`:** the name of a runner in `.github/runners.json`.
- **`spot`:** `true` to run on Spot machines, which can be preempted, so the job must be able to carry on from its work disk on a rerun. Standard machines otherwise.
- **`from`,** optionally: the files the disk's contents depend on, such as a list of dependency versions and a toolchain file. The action reads them through GitHub's API at the commit the job builds: the run's own commit, by its SHA, never a moving ref; and in a `pull_request_target` run, the contribution's own, `github.event.pull_request.head.sha` in the head repository, which is what the contribution's jobs check out, even though `.github/runners.json` is read from the default branch there. A listed file that doesn't exist fails the action, naming it.

  The key is the first 20 hex digits of the SHA-256 of these parts, in this order: the family, the runner, then each listed path and its contents, in the order listed. Each part is written as its length in bytes, an unsigned 64-bit big-endian number, followed by its bytes: names and paths in UTF-8, and contents exactly as the file's bytes at that commit. For example, family `build`, runner `lanescape-80vcpu-linux-amd64`, `deps/versions.json` holding `café 1.0` and a newline, and an empty `rust-toolchain.toml`, give the key `a77434447088c41f6e9f`. So the key changes when a listed file's contents do, and also when the family, the runner, the paths or their order do.
- **`key`,** optionally, instead of `from`: the key itself, 20 lowercase hex digits, for a disk whose inputs aren't files in the repository.

With neither `from` nor `key`, the family has one key per scope and architecture, `00000000000000000000`, since the service's selectors always carry one. A save that the service accepts makes a newer version of that disk, and later runs restore the newest. That's enough for most jobs, since a build redoes only what's out of date. A save doesn't pass a disk to a later job of the same run: the service picks each run's disk once, for all its jobs with the same selector.

Whatever the key, a job restores by the service's order, along its run's restore chain: in the chain's first scope, the disk for that key, then the newest of the family; then the same in its base, if it has one; then an empty disk. A pull request's chain is its own scope, then its base, the long-lived branch it targets if the consumer lists it, or else main's; a long-lived branch's push reads its own, then main's; main's pushes and `read` runs read main's. A pull request that changes a listed file gets a new key, so it saves beside main's disk rather than over it. A pull request keeps one disk of each family, so its saving one replaces its others of that family: a family is one lineage, and jobs that need several keys alive at once use separate families.

The output `map` has an entry for each runner name in `.github/runners.json`, which is every key but `bootstrap` and `branches`, the service's own settings (`lanescape/runners`' spec 0005, A consumer's files), and one for each entry in `disks`, named by its family:

- **Inside the organization,** each runner name maps to its selector for this run, with the scope the binding returned, such as `pr42`, `main` or a long-lived branch's `branch-v1`: `lanescape-80vcpu-linux-amd64-pr42`. Each disk entry maps to its runner's selector with `-spot` if asked and `-disk-<family>-<key>`, with the key it was given or made: `lanescape-80vcpu-linux-amd64-spot-disk-build-3f9c2e7a10b4d5e6f708-pr42`. The service checks every selector against the run's binding before a job runs. If the binding answers that GitHub couldn't be reached, the action retries with backoff, and fails only once its retries are spent.
- **Outside,** each runner name maps to its fallback, and each disk entry to its runner's fallback. A fork can override any fallback with a repository or organization variable `RUNNERS`, a JSON object in the same shape as `.github/runners.json`, whose values may be a label or a list of labels. Names it doesn't override keep the file's fallback, and names the file doesn't list as runners, `bootstrap` and `branches` among them, are ignored. For a fork's pull request into an organization repository, the action ignores `RUNNERS` from the base repository, since it's the organization's.

A job that asks for a name the map lacks gets an empty label, which GitHub refuses; the action's summary lists the names it has.

## `checkout`

Checks out the repository for a job. With no inputs, it checks out the run's own commit, as `actions/checkout` does. With `contribution: true`, in a `pull_request_target` run, it checks out exactly `github.event.pull_request.head.sha`, the commit the approved run was made for, from the head repository, with no credentials persisted, and refuses in any other event. Outside the organization, `contribution: true` does the same in a fork's own `pull_request_target` runs, which use nothing of the service's.

## `save-disk`

Asks for the job's work disk to be saved when it finishes, by writing the service's save request on the machine. The service saves the disk if the job succeeded and its scope and selector allow. Outside the organization, or on a machine that isn't the service's, it does nothing and says so.

## `restore-cache` and `save-cache`

Restore build state at a job's start and save it at its end, through the service's cache, on any runner inside the organization: the service's machines, and runners outside the service. The cache keeps the same scopes as saved disks, so a pull request restores main's entry and saves its own, and nothing it saves reaches main. A job uses both, with the same family:

```yaml
  build:
    needs: runners
    runs-on: ${{ fromJSON(needs.runners.outputs.map)['build'] }}
    permissions:
      contents: read
      id-token: write
    steps:
      - uses: lanescape/actions/checkout@v1
      - uses: lanescape/actions/restore-cache@v1
        id: cache
        with:
          family: build
          path: |
            target
            ~/.cargo/registry
          from: |
            deps/versions.json
            rust-toolchain.toml
      - run: ./build.sh
      - uses: lanescape/actions/save-cache@v1
        with:
          family: build
```

The run must be bound by its `runners` job first, since the cache uses the run's binding, and the job needs `id-token: write`, for the token it sends the service, and `contents: read`, to read the `from` files; without them both actions fail, saying which is missing. A contribution's job in a `pull_request_target` run, normally on a runner outside the service, needs them too, which lets the contributor's code mint the repository's tokens while it runs: the consumer uses the cache there only if nothing but the service trusts its tokens from such a run (`lanescape/runners`' spec 0006, Who may use it).

**`restore-cache`** takes:

- **`family`:** 1 to 32 lowercase letters, digits and hyphens, starting with a letter. A family is one lineage, as a disk's is.
- **`path`:** the files and directories the entry holds, one to a line, each relative to the workspace, or starting with `~/` for the runner's home directory. Any other path fails the action.
- **`from`,** optionally: the files the entry's contents depend on, one to a line, read through GitHub's API at the commit the job builds, exactly as `runners` reads a disk's (above), the contribution's own commit in a `pull_request_target` run. A listed file that doesn't exist fails the action, naming it. The key is the first 20 hex digits of the SHA-256 of these parts, in this order, each written as `runners` writes them: the family, the platform, then each listed path and its contents.
- **`key`,** optionally, instead of `from`: the key itself, 20 lowercase hex digits. With neither, the key is `00000000000000000000`.
- **`lookup-only`,** optionally, `true`: report which entry the restore order would choose, and whether its key is exact, without downloading anything or touching the paths. The service records that as the run's choice, so a later `restore-cache` of the run for the same entry restores exactly what the lookup reported.

The platform is the runner's `RUNNER_OS` and `RUNNER_ARCH` in lowercase, joined by a hyphen, such as `linux-x64` or `macos-arm64`; entries are never restored on another platform than the one that saved them.

Inside the organization, the action asks the service for the run's choice for that family, platform and key (`lanescape/runners`' spec 0005, The cache), as a lookup if `lookup-only` is set. The service picks once per run, so every job of the run that asks for the same entry gets the same one, by the order disks use: in the first scope of the run's restore chain, its entry for the key, then its newest of the family; then the same in its base, if it has one; then nothing. The action downloads the entry with ranged `GET`s to a file in `$RUNNER_TEMP`, resuming after a failure and asking again for a URL when one expires, checks its size and SHA-256, then extracts it into the paths, replacing what's there. It refuses any member whose name is absolute, holds `..`, or lies outside the listed paths, and any link that points outside them. Its outputs are `step`, the step of the order the entry came from, such as `main_exact` or `pull_request_newest`, or `none`; `exact`, `true` if the entry is for the key the job asked for, and `false` otherwise, nothing restored included; `scope`, the scope it came from, or empty; and `key`, the key the job asked for. A lookup has the same outputs, and restores nothing. It records the family's paths, key and platform in `$RUNNER_TEMP`, for `save-cache`.

**Restoring only** is `restore-cache` with no `save-cache` after it: the job starts warm and saves nothing, as a job that only tests or reads would.

**`save-cache`** takes `family`, which `restore-cache` must have set up earlier in the job, or the action fails, and optionally `skip`: `true` saves nothing and says so, for a save a job decides at run time it doesn't need, such as `skip: ${{ steps.cache.outputs.exact }}` when it restored its exact key and changed nothing that matters. Inside the organization, it reserves an upload first, so a run whose scope saves nothing, such as a `read` run, packs nothing and says so. Then it packs the paths, uploads the archive through the signed URL the service gives, in chunks, resuming after a failure, and reserving again if the reservation expires first, and commits it with its size and SHA-256. The service keeps the entry only once GitHub shows the job succeeded, and later runs restore it; a later job of the same run that asks for the same family, platform and key gets the run's choice, made before. A job places `save-cache` after the steps whose state it keeps; like any step, it runs only if every step before it succeeded, so a failed job uploads nothing. A job may save before steps that would spoil its state, such as tests that write into the tree.

**The archive** is a tar archive in the PAX format, compressed with zstd, of the paths as `path` names them, `~/` included, which restoring expands to the restoring runner's home. It keeps each file's mode and its modification time to the nanosecond, which PAX records and restoring sets as precisely as the filesystem keeps it, since build tools compare file times and a time rounded to the second can make a file look older or newer than what it was built from; and symbolic links, but not owners.

**Failures.** The cache only saves time, so neither action fails a job for want of it: if the service refuses, answers that GitHub couldn't be reached after the action's retries with backoff, or a download, upload or check fails, the action says why in the job's summary and goes on, `restore-cache` restoring nothing. An error while extracting does fail `restore-cache`, since the paths may then be half restored. Neither action ever logs a token, a signed URL or an upload's session URI.

**Outside the organization,** neither does anything, and each says so; `restore-cache`'s `step` is `none`.

**Why two actions.** A composite action can't have a step that runs after the job's own, so restoring and saving are steps the job places, as GitHub's own cache actions also offer. A JavaScript action could save after the job's steps, but would have to fetch and set up its own Go toolchain, outside `actions/setup-go`. Placing the save also lets a job save at the point where its state is complete. The disk's action is `save-disk`, not `save`, so that each action's name says what it saves or restores.

## How actions run

Each action is a composite action that builds its Go program from the action's own source with `go build`, run in the repository root of the action's own downloaded copy, `$GITHUB_ACTION_PATH/..`, into `$RUNNER_TEMP`, and runs the binary. What runs is exactly the code at the commit the action was referenced at, with no separate release to keep in step, and the job sees the program's own exit status, which `go run` wouldn't give. The service's base image has Go; elsewhere, the action sets up that version with `actions/setup-go` from `go.mod`. Builds use Go's build cache, so later steps of a job build in moments. The programs are pure Go, with CGO off, and CI builds them for every platform runners use: Linux on amd64 and arm64, macOS on arm64, and Windows on amd64.

## Versions

Consumers reference `@v1`, a tag that moves (specs/0002-releases.md). `v1` stays compatible: its actions keep their inputs, outputs and behaviour. A change that would break them comes as `v2`, while `v1` keeps working, until no consumer references it.
