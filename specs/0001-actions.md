# Actions

The `lanescape` organization's repositories use its runner service, `lanescape/runners`, through four actions in this repository, `lanescape/actions/<name>@v1`: `image`, `disk`, `checkout` and `save-disk`. They call the service through the interface it publishes and keeps compatible, its spec 0005, "The interface for actions", which is all they may rely on. Each works inside the organization and degrades gracefully outside it, so the same workflow runs in a fork, at GitHub's expense, as far as it can without the service.

## Where an action is

Each action decides the same way, and says which in the job's summary:

- **Inside the organization:** `github.repository_owner_id` is the organization's immutable ID, `endpoints.Organization` in the service's interface, never compared by name, and either the run isn't from a fork's pull request, by the event's head repository, or it's a `pull_request_target` run, whose jobs the service accepts only once a maintainer approves them.
- **Outside:** anything else, including a fork's own pushes, another organization, and a fork's pull request into one of the organization's repositories, which runs in the base repository but never on the service's machines.

A run is a fork's pull request's when its event's payload has a pull request, as `pull_request`, `pull_request_review` and `pull_request_target` events do, whose head repository, by ID, isn't the run's repository, or is gone. The owner's ID is `GITHUB_REPOSITORY_OWNER_ID`, or the payload's repository's owner's if that isn't set.

Inside the organization, an action that calls the service also tells whether it's on one of the service's machines, as the service's interface says: by the metadata server's `instance/attributes/lsr-service`, which answers on the service's machines, from the host and from a job's container alike, and nowhere else. It asks with a timeout of a few seconds, so that on any other runner it decides at once.

The decision is a courtesy, not a guard. The service checks everything for itself, from GitHub's records and signed tokens.

## Runners

No action makes runner labels or binds a run. A workflow names the service's machines in `runs-on` itself, as lanescape/runners' spec 0005 (Selectors) gives them: a shape, and optionally Spot, and nothing else. The service works out the rest: the run's scope, at its first job's check, from what GitHub told that job's runner, and the image version its commit defines and its disks' keys, from the files at that commit. Which image a job runs in and which disk family it works on, the job asks for in its steps, with `image` and `disk`; the consumer declares both in `.github/runners.json`, which the service reads; no action reads or checks it.

```yaml
jobs:
  build:
    runs-on: lanescape-80vcpu-linux-amd64-spot
```

A job's `runs-on` is exactly its selector, with no other label. In a fork no runner carries those labels, so a workflow that also runs in forks skips those jobs there, or picks another label, by expressions: `github.repository_owner_id` isn't the organization's ID in a fork's own runs, and in a fork's `pull_request` run into the consumer, which runs in the consumer's repository but never on the service's machines, `github.event.pull_request.head.repo.id` isn't `github.repository_id`. An approved contribution's `pull_request_target` run has a fork's head too, but does run on the service's machines, so a workflow tests `github.event_name` as well and never sends a contribution's jobs elsewhere. A job the service can't serve is stopped by the service before its first step, with the reason in its log, and its run is cancelled; what a job's `image` or `disk` step can't have fails that step, with the service's reason.

## Contributions

An approved contribution's `pull_request_target` run is bound by the service once a maintainer approves it, at the check of its contribution job: the one job that names the environment `contribution`, whose required reviewers are the consumer's maintainers, which every other job of the run on the service's machines needs, and which runs only the default branch's code, checking nothing out. It needs no action of ours, only a step:

```yaml
jobs:
  contribution:
    runs-on: lanescape-2vcpu-linux-amd64
    environment: contribution
    permissions: {}
    steps:
      - run: echo "Approved"
  build:
    needs: contribution
    runs-on: lanescape-80vcpu-linux-amd64-spot
    permissions: {}
    steps:
      - uses: lanescape/actions/disk@v1
        with:
          family: build
      - uses: lanescape/actions/checkout@v1
        with:
          contribution: true
```

Jobs that run the contribution's code never name the environment, check out with `checkout`'s `contribution: true`, and have `permissions: {}`. Only a job that must sign in to the consumer's own cloud resources may have `id-token: write`, and then the contributor's code can mint tokens of the base repository for any audience, naming the default branch's workflow and ref, while the job runs (lanescape/runners' spec 0001, Forks and contributions). So every party that trusts the consumer's tokens, its own cloud identity provider included, must tell a token whose `event_name` is `pull_request_target` apart, and give it only what the consumer means a contribution to have, or refuse it; a workflow that can't rely on that gives the job no `id-token` at all. The runner service itself takes no GitHub token.

A contribution's run may use images: its `image` job, after the contribution job, has the service build the image its head commit defines, in its pull request's scope, and its jobs that run the contribution's code run in it. A job of the run that runs only the default branch's code, such as a trusted final job, never runs in the contribution's image, which is the contributor's code.

## `image`

Has the service build the image the run's commit defines, if it isn't built yet, waits for it, and outputs its reference, for later jobs to run in. It's the step of a job, on one of the service's machines and normally of the smallest shape, that the jobs running in the image need:

```yaml
jobs:
  image:
    runs-on: lanescape-2vcpu-linux-amd64
    outputs:
      ref: ${{ steps.image.outputs.ref }}
    steps:
      - id: image
        uses: lanescape/actions/image@v1
        with:
          name: build
  build:
    needs: image
    runs-on: lanescape-80vcpu-linux-amd64-spot
    container:
      image: ${{ needs.image.outputs.ref }}
      volumes:
        - /work:/work:rslave
    steps:
      - uses: lanescape/actions/disk@v1
        with:
          family: build
```

Its input `name` is the image's name, as `.github/runners.json` declares it at the run's commit; one job may run the action once for each image its run uses, each step with its own outputs. Inside the organization, on the service's machine, the action asks for the image by `POST /v1/images`, signed with the machine's identity token for the service's audience, from the machine's metadata server, got afresh for each request; it needs no GitHub token. Of the architectures the service answers for, it takes a `ready` one as done, fails at once on a `failed` one, and follows each `building` one by `GET /v1/images/builds/<id>`, from offset 0, then from each answer's `next`, every 5 seconds, writing the build's log to the job's log as it comes, in a group named by the architecture and version, until every build has ended, asking for the image again when they have. It succeeds when the service answers the image `ready`, setting its outputs `ref`, the image's reference, pinned by its digest, and `version`, and writing both in the job's summary; and it fails, with the service's reason, as soon as one build fails, so the jobs that need it are skipped; rerunning the job asks for a new build. A build's log comes from scripts in the run's commit, a contributor's in a contribution's run, so the action writes it as plain text: before each part of the log it stops GitHub's workflow commands with `::stop-commands::` and a token it makes at random for that part, and resumes them with the same token after, each on a line of its own, ending a part that doesn't end with a newline with one first, so nothing in the log can mask, annotate, or set anything in the job, and the action's own commands work again after it. It retries a `503` with backoff, and fails only once its retries are spent: eight attempts in all, waiting 2 seconds, then twice as long each time up to a minute, or longer if `Retry-After` asks, up to a minute. It retries the same way when the service can't be reached, or a server in front of it answers 429 or a 5xx; any other answer outside the interface fails it at once, and a refusal fails it with the service's reason. A build's log is the consumer's own scripts' output, and goes into the run's log, which is as public as the repository.

A later job runs in the image by naming the output in `container:` or `container.image`, with no `credentials`: the machine's Docker pulls it from the service's registry with a token the service gives that machine, and the reference's digest makes the job run exactly what the run was answered with. A container job that uses a work disk maps `/work:/work:rslave` in `container.volumes`, so that the disk its `disk` step asks for, mounted after the container started, reaches it.

Outside the organization it does nothing, sets no outputs, and says so; a workflow that also runs in forks gives its container jobs another image there, or skips them. Inside it, on a machine that isn't the service's, it fails, saying that it must run on one of the service's machines.

## `disk`

Asks the service for the job's work disk, of a family, and waits until it's mounted at `/work`:

```yaml
      - uses: lanescape/actions/disk@v1
        with:
          family: build
```

Its input `family` is the family's name, as `.github/runners.json` declares it at the run's commit. It's normally a job's first step, since `/work` is an empty directory nobody may write until then. Inside the organization, on the service's machine, the action asks by `POST /v1/disks`, signed with the machine's identity token as `image` is, and asks again every 5 seconds while the answer is `making`, retrying a `503`, an unreachable service, a 429 or a 5xx as `image` does. The job chooses only the family: the scope, the key and what the disk starts from are the service's to decide, and the action writes where it came from, the answer's `restored`, in the job's summary. Once the answer is `ready`, it waits until `/work`, as its own process sees it, is a mounted disk rather than the empty directory under it, by `/proc/self/mountinfo`, for up to 2 minutes, and then succeeds. In a container job it runs in the job's container, so it sees the disk only if the container maps `/work:/work:rslave`; if `/work` isn't mounted by then, it fails, saying that a container job must map `/work` that way. A refusal, such as a family the run's commit doesn't declare, or a second family for a job that already has a work disk, fails it with the service's reason. A job has one work disk: running the action again with the same family succeeds at once.

Outside the organization, or on a machine that isn't the service's, it does nothing and says so, and `/work` is whatever the runner has there.

## `checkout`

Checks out the repository for a job. With no inputs, it checks out the run's own commit, as `actions/checkout` does. With `contribution: true`, in a `pull_request_target` run, it checks out exactly `github.event.pull_request.head.sha`, the commit the approved run was made for, from the head repository, with no credentials persisted, and refuses in any other event. Outside the organization, `contribution: true` does the same in a fork's own `pull_request_target` runs, which use nothing of the service's.

It checks out with `actions/checkout`, pinned by its commit, in the job's workspace, so on the service's machines too the checkout is on the boot disk, never `/work`, and does the same on any runner. The action's program decides what to check out, and passes `actions/checkout` its own defaults for the run's own commit, credentials persisted as there; for a contribution, the head repository and commit, `persist-credentials: false`, and `allow-unsafe-pr-checkout: true`, which `actions/checkout` asks for before it checks out a fork's commit in a `pull_request_target` run. Afterwards the program confirms that the checkout's `HEAD` is that commit, and that its git config keeps no `extraheader`, `includeIf` or `credential` entry, and fails otherwise. `contribution` is `true` or `false`; a contribution whose head repository is gone is refused.

## `save-disk`

Asks for the job's work disk to be saved when it finishes, by `POST /v1/disks/save`, signed with the machine's identity token as `image` is, with the same retries, so it works the same from a job's container as from the host. The service saves the disk if the job succeeds and its scope saves; the action writes in the job's summary whether the scope saves, and the service's reason if it doesn't, such as a run in the `read` scope, and succeeds either way. A job with no work disk, because it never ran `disk`, is refused, and the action fails with the service's reason. Outside the organization, or on a machine that isn't the service's, it does nothing and says so. It's named `save-disk`, not `save`, so that its name says what it saves.

## How actions run

Each action is a composite action that builds its Go program from the action's own source with `go build`, run in the repository root of the action's own downloaded copy, `$GITHUB_ACTION_PATH/..`, into `$RUNNER_TEMP`, and runs the binary. What runs is exactly the code at the commit the action was referenced at, with no separate release to keep in step, and the job sees the program's own exit status, which `go run` wouldn't give. The service's base image has Go on the host; in a job's container, and on any other runner, the action sets up that version with `actions/setup-go` from `go.mod`. Which applies is told by the Go on the machine: the action uses it if `go env GOVERSION` is exactly `go.mod`'s `toolchain` version, and otherwise sets that exact version up, named as `go-version` so that no `GOTOOLCHAIN` the job has can make `actions/setup-go` pick another, with its cache off, since a consumer's workspace has no `go.sum` of ours. The build fails if Go still isn't that version. `actions/setup-go` puts its Go first on the job's `PATH` for the steps after, so a job that sets up a Go of its own does so after these actions. Builds use Go's build cache, so later steps of a job build in moments. A build runs with `GOENV=off`, `GOWORK=off`, `GOTOOLCHAIN=local`, `GO111MODULE=on` and `CGO_ENABLED=0`, and with `GOOS`, `GOARCH`, `GOAMD64`, `GOARM64`, `GOFLAGS`, `GOEXPERIMENT`, `GOROOT`, `GODEBUG` and `GOFIPS140` cleared, so a job's own Go settings, in its environment or saved with `go env -w`, can't change what's built, into a new directory under `$RUNNER_TEMP`; `actions/setup-go`, which runs Go itself, and the check for Go run with the same settings, and `actions/setup-go`, which prints `go env`, also without the job's module and cache settings, such as a `GOPROXY` that holds a credential, and without `GO_DOWNLOAD_BASE_URL`, so it downloads Go from Go's own site rather than print a mirror's URL; it downloads this module's dependencies through the `GOPROXY` in the job's environment, if any, and otherwise Go's own. A composite action gets no `INPUT_` variables, so each `action.yml` passes its inputs to its program as `INPUT_<NAME>` in the step's environment, and never writes an expression into a script. The programs are pure Go, with CGO off, and CI builds them for every platform runners use: Linux on amd64 and arm64, macOS on arm64, and Windows on amd64.

## Versions

Consumers reference `@v1`, a tag that moves (specs/0002-releases.md). No `v1` has been released yet, so until the first release the actions may still change, as `runners` has given way to `image` and `disk`, and `image` has come to output a reference for a job's container. From then on `v1` stays compatible: its actions keep their inputs, outputs and behaviour. A change that would break them comes as `v2`, while `v1` keeps working, until no consumer references it.
