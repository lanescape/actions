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

Restore build state at a job's start and save it at its end, through the service's cache, on any runner inside the organization: the service's machines, and runners outside the service. The cache keeps the same scopes as saved disks, so a pull request restores main's entry and saves its own, and nothing it saves reaches main. The service keeps every entry's record, its index; where the bytes are kept depends on where the job runs, since a cache is local to where its jobs run (below, Stores). A job uses both actions, with the same family:

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
- **`lookup-only`,** optionally, `true`: report which entry the restore order would choose, and whether its key is exact, without fetching anything or touching the paths. The service records that as the run's choice, so a later `restore-cache` of the run for the same entry restores exactly what the lookup reported.
- **`store`,** optionally: where the entry's bytes are kept, `service` or `actions` (below, Stores). By default, `service` on the service's own machines, which the action recognizes as `save-disk` does, and `actions` on every other runner.

The platform is the runner's `RUNNER_OS` and `RUNNER_ARCH` in lowercase, joined by a hyphen, such as `linux-x64` or `macos-arm64`; entries are never restored on another platform than the one that saved them.

Inside the organization, the action asks the service for the run's choice for that store, family, platform and key (`lanescape/runners`' spec 0005, The cache), as a lookup if `lookup-only` is set. The service picks once per run, so every job of the run that asks for the same entry gets the same one, by the order disks use: in the first scope of the run's restore chain, its entry for the key, then its newest of the family; then the same in its base, if it has one; then nothing. Entries of one store are never chosen for another. The answer gives the entry's scope, key, step, size and SHA-256, and, for the `service` store only, a signed URL. The action fetches the entry's bytes from its store to a file in `$RUNNER_TEMP` (below), checks the file's size and SHA-256 against the answer, and only on an exact match extracts it into the paths, replacing what's there. It refuses any member whose name is absolute, holds `..`, or lies outside the listed paths, and any link that points outside them. Its outputs are:

- **`step`:** the step of the order the chosen entry came from, such as `main_exact` or `pull_request_newest`, or `none`;
- **`exact`:** `true` if the entry restored, or for a lookup the entry chosen, is for the key the job asked for, and `false` otherwise, nothing restored included;
- **`scope`:** the chosen entry's scope, or empty;
- **`key`:** the key the job asked for;
- **`store`:** the store the job used;
- **`address`:** in the `actions` store, the chosen entry's content address in the Actions cache (below), or empty, so that a job may delete an entry's bytes once the service no longer needs them, which the actions themselves never do;
- **`result`:** `restored`; `none`, when the service chose nothing; `missing`, when the chosen entry's bytes couldn't be fetched from its store; or `mismatch`, when what was fetched isn't exactly the recorded size and SHA-256. A lookup's is empty.

A lookup has the same outputs, and restores nothing. On `missing` or `mismatch` nothing is restored, and the job builds from what it has, its sources included, as it would with no entry. The action records the family's paths, key, platform and store in `$RUNNER_TEMP`, for `save-cache`.

**Restoring only** is `restore-cache` with no `save-cache` after it: the job starts warm and saves nothing, as a job that only tests or reads would.

**`save-cache`** takes `family`, which `restore-cache` must have set up earlier in the job, or the action fails, and optionally `skip`: `true` saves nothing and says so, for a save a job decides at run time it doesn't need, such as `skip: ${{ steps.cache.outputs.exact }}` when it restored its exact key and changed nothing that matters. It saves to the store `restore-cache` used. Inside the organization, a run whose scope saves nothing, such as a `read` run, packs nothing and says so. Otherwise it packs the paths, puts the archive in its store (below), and records the entry with the service, with its size and SHA-256. The service keeps the entry only once GitHub shows the job succeeded, and later runs restore it; a later job of the same run that asks for the same store, family, platform and key gets the run's choice, made before. A job places `save-cache` after the steps whose state it keeps; like any step, it runs only if every step before it succeeded, so a failed job saves nothing. A job may save before steps that would spoil its state, such as tests that write into the tree.

**The archive** is a tar archive in the PAX format, compressed with zstd, of the paths as `path` names them, `~/` included, which restoring expands to the restoring runner's home. It keeps each file's mode and its modification time to the nanosecond, which PAX records and restoring sets as precisely as the filesystem keeps it, since build tools compare file times and a time rounded to the second can make a file look older or newer than what it was built from; and symbolic links, but not owners.

**Failures.** The cache is an optimization a job must be correct without, so neither action fails a job for want of it: if the service refuses, answers that GitHub couldn't be reached after the action's retries with backoff, or a fetch, save or check fails, the action says why in the job's summary and goes on, `restore-cache` restoring nothing. An error while extracting does fail `restore-cache`, since the paths may then be half restored, and so does a `store` the job's runner can't use, as any other bad input does. Neither action ever logs a token, a signed URL, an upload's session URI, or a URL of the runner's Actions cache.

**Outside the organization,** neither does anything, and each says so; `restore-cache`'s `step` is `none`.

**Why two actions.** A composite action can't have a step that runs after the job's own, so restoring and saving are steps the job places, as GitHub's own cache actions also offer. A JavaScript action could save after the job's steps, but would have to fetch and set up its own Go toolchain, outside `actions/setup-go`. Placing the save also lets a job save at the point where its state is complete. The disk's action is `save-disk`, not `save`, so that each action's name says what it saves or restores.

### Stores

A cache is local to where its jobs run. A runner outside Google Cloud that read from or wrote to the service's bucket would pay internet egress on every byte it read, and an entry can be many gigabytes, so such runners never use the bucket: the service gives them no signed URLs. The service keeps every entry's record, in every store, with the same scopes, restore order, success gating, one entry of each family for a pull request, and retention; only the bytes are elsewhere. A store's entries are kept apart from another's, so a job only ever restores an entry saved to the store it uses.

- **`service`:** the service's bucket, open only to the service's own machines, which are in its region. `restore-cache` downloads the entry through the signed URL the answer gives, with ranged `GET`s, resuming after a failure and asking again for a URL when one expires. `save-cache` reserves an upload first, so that a run whose scope saves nothing learns it before packing; then it uploads the archive through the signed URL the service gives, in chunks, resuming after a failure, and reserving again if the reservation expires first, and commits it with its size and SHA-256.
- **`actions`:** the runner's own Actions cache, GitHub's, or a runner provider's that hosts it next to its machines and serves `actions/cache` from there, scoped by branch as GitHub's is. The service records the entry without its bytes, and its answers carry no URL. This is the default everywhere but the service's machines, which can name it too.

A job names its store with `store`, and needs to only to depart from the default. The default comes from where the job runs, because that's what decides the cost: the bucket is free to read only from the service's region.

**The Actions cache isn't trusted.** It doesn't have the service's guarantees:

- Its scopes are GitHub's, by branch and pull request, enforced by GitHub or the provider, not by the run's binding.
- A provider may not apply GitHub's rule that runs of untrusted triggers, such as `pull_request_target`, only read the default branch's scope and never write it.
- So anything in it may have been written by code the consumer doesn't trust.

So the service's record is what the job trusts, and the Actions cache only holds bytes the record names: a restore uses them only if they're exactly the recorded size and SHA-256.

**Restoring from the Actions cache.** `restore-cache` asks the service first, as a restore, not a lookup, so the run's choice and the entry's last use are recorded as for any store. A lookup fetches nothing. The answer gives the entry's size and SHA-256. Then the action asks the runner's Actions cache for the key `lanescape-cache-<sha256>-<size>`, the entry's content address, made from the record's SHA-256 and size and nothing else, never its family, key, scope or branch, since its content is all a restore can check. The cache also matches a key by prefix, so an answer for any other key counts as a miss. A miss, any error, or a runner with no Actions cache is `missing`; bytes that aren't exactly the record's are `mismatch`, a warning in the job's summary that names the key and nothing else. Either way nothing of them reaches the paths, and the job builds from its sources. Without the service's record there's nothing to check the Actions cache against, so a restore that the service refuses or doesn't answer never reads it.

The action reads and writes the Actions cache with its own program, which speaks the runner's cache service as `actions/cache` does at v6.1.0, `55cc8345863c7cc4c66a329aec7e433d2d1c52a9`, but keeps the archive's bytes as they are, with no wrapping. It doesn't use `actions/cache` itself: `actions/cache/restore` extracts whatever entry it finds into the workspace with tar's `-P`, so a member named with an absolute path or `..` lands wherever the runner's user may write, before any later step could check a byte of it; and `actions/cache/save` turns every failure into a warning and reports nothing, so a job couldn't tell whether its bytes were saved, and with debug logging on it logs the cache service's answers, whose signed URLs it masks only in the form GitHub's own service gives them. Every entry the program writes or reads carries, in the field `actions/cache` calls its version, the SHA-256, in lowercase hex, of the UTF-8 string `lanescape-cache/1`, which keeps the actions' entries apart from anything `actions/cache` writes.

To restore, the program asks the cache service for the content address's download URL, by that key alone, and takes it only if the key the answer matched is exactly that one. It reads the URL in ranges to a file in `$RUNNER_TEMP`, resuming after a failure, hashing as it goes, and stops, as a mismatch, at the first byte past the record's size; a file shorter than the size, or with another SHA-256, is a mismatch too, and is deleted. Only a file whose size and SHA-256 are exactly the record's is extracted.

GitHub gives the cache service's address and token, `ACTIONS_RESULTS_URL` and `ACTIONS_RUNTIME_TOKEN`, only to JavaScript and container actions, never to a composite action's own commands, and a composite action can't name another action of its own repository at its own commit; so `actions/github-script`, pinned at v9.0.0 by its full commit SHA, `3a2844b7e9c422d3c10d287c895573f7108da1b3`, starts the program for these steps and does nothing else. A runner whose environment doesn't name GitHub's current cache service, `ACTIONS_CACHE_SERVICE_V2`, has no Actions cache as far as the actions are concerned. The runtime token and the cache service's URLs are credentials like the others, which the program never logs, at any log level.

**Saving to the Actions cache.** `save-cache` first asks the service for the run's binding, with `POST /v1/bind`, which answers with the binding the run already has, so that a `read` run packs nothing. Then it packs the paths to a file in `$RUNNER_TEMP`, and its program asks the cache service to create an entry for the archive's content address, with the version above. Given an upload URL, it uploads the file as `actions/cache` does: in one request up to 128 MiB, and otherwise in blocks of 64 MiB and a list of them, each block retried after a failure; then it asks the cache service to finalize the entry, with its size, and looks the key up again. If the cache service refuses to create the entry, because the key is already there, the run may only read, or for any other reason, the program looks the key up instead, and if it's there, readable by this job, reads it back in full, as a restore would, to check that it's exactly this archive. Only once the content address is readable by this job, with exactly the key asked for, and holds this archive, whether the job wrote it or checked it, does the action record the entry with the service, naming the store `actions`, with the archive's size and SHA-256, and commit it (`lanescape/runners`' spec 0005, The cache). An entry the job created is this archive, since nothing could have written that key in its scope first without these bytes' SHA-256, which no one has before the job makes them. Otherwise the action records nothing, and says why. The Actions cache needs no permission beyond those the job already has.

**How GitHub's scopes and the restore order fit.** An entry is recorded in a scope only by a job bound to that scope, and only once that job could read its bytes from GitHub's scopes. A job reads its own ref's scope and the default branch's, and a pull request's run also reads its base branch's. GitHub's saves land in the saving run's ref's scope:

- **Main's entries,** recorded by pushes to the default branch, have their bytes in the default branch's scope, which every run of the repository can read. So every restore chain that falls back to main's reaches them.
- **A long-lived branch's entries,** recorded by its pushes, have their bytes in that branch's scope, or the default branch's if they were there already. Its own pushes and the pull requests into it read both.
- **A pull request's entries,** recorded by its `pull_request` runs, have their bytes in its merge ref's scope, its base's or the default branch's. Only its own later runs restore them, and they read all three.
- **A `pull_request_target` run** runs on the default branch's ref: its uploads go to the default branch's scope, where GitHub's rule, and a provider that applies it, refuses them. A refused upload records nothing, unless the same archive is already readable there, checked as above. Whatever is recorded has its bytes in the default branch's scope, which the pull request's later runs read.
- **A `read` run** records nothing.

So a pull request choosing its base's entry whose bytes were saved only in another pull request's scope can't happen: its base's entries are recorded only by pushes to the base, which can't read any pull request's scope, so their bytes are in the base's scope or the default branch's, both of which the pull request reads. A chosen entry's bytes can still be out of reach, and then the restore is `missing`: when the cache has evicted them, as it does by last access and size; when a pull request's base has changed since its entry was saved in the old base's scope; when one pull request has runs under both `pull_request` and `pull_request_target`, which read different refs' scopes; or when the provider scopes differently from GitHub. A miss doesn't move on to the next step of the order, since the service chooses one entry for the run; the job builds from its sources, and its save makes a new entry, whose bytes are where it can read them.

**Whole entries, not parts.** An entry is saved to the Actions cache whole. GitHub's documentation of the Actions cache, its "Dependency caching reference" on docs.github.com, states no limit on one entry's size, only on a repository's total: 10 GB by default, more if an enterprise owner raises it, and billed beyond 10 GB. The `@actions/cache` library that `actions/cache` v6.1.0 bundles refuses an archive over 10 GiB only for GitHub's legacy cache service, not for the current one, which takes up to 50,000 blocks, over 3 TiB in blocks of 64 MiB. Parts would add a failure without removing one: eviction is by last access, part by part, and losing any part loses the whole. If a provider refuses large entries, their saves warn and record nothing, and every restore of that family builds from its sources; the warnings would show it, and splitting would be decided then. An entry larger than a repository's whole allowance can't be kept by that cache at all, in parts or not.

**What the Actions cache can and can't harm.**

- **Integrity** comes from the service's record: a restore uses bytes only when they're exactly the recorded size and SHA-256, which the job that committed the entry gave, in a scope it could write anyway (`lanescape/runners`' spec 0006). The Actions cache gives no one more than that job could.
- **Availability:** whoever can write a scope a run reads, or fill the repository's allowance so that older entries are evicted, can cause misses, and so builds from sources, which cost runner time but not correctness. A key first saved with other bytes stays, since saving an existing key does nothing, and every mismatch warning names it, until a maintainer deletes it from the repository's Actions caches; the actions hold no permission to.
- **Confidentiality:** anything in the Actions cache can be read by every run that can read its scope, and the default branch's by every pull request's run, a fork's included. The organization's repositories are public, and entries are their build state: an entry must never hold a secret, in any store.

**Following `actions/cache`.** The program follows the requests `actions/cache` makes at v6.1.0, and moving to a later release's is an ordinary pull request, whose tests run against runners' real Actions caches (specs/0002-releases.md).

## How actions run

Each action is a composite action that builds its Go program from the action's own source with `go build`, run in the repository root of the action's own downloaded copy, `$GITHUB_ACTION_PATH/..`, into `$RUNNER_TEMP`, and runs the binary. What runs is exactly the code at the commit the action was referenced at, with no separate release to keep in step, and the job sees the program's own exit status, which `go run` wouldn't give. The service's base image has Go; elsewhere, the action sets up that version with `actions/setup-go` from `go.mod`. Builds use Go's build cache, so later steps of a job reuse what earlier ones built. The programs are pure Go, with CGO off, and CI builds them for every platform runners use: Linux on amd64 and arm64, macOS on arm64, and Windows on amd64.

## Versions

Consumers reference `@v1`, a tag that moves (specs/0002-releases.md). `v1` stays compatible: its actions keep their inputs, outputs and behaviour. A change that would break them comes as `v2`, while `v1` keeps working, until no consumer references it.
