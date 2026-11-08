# Actions

The `lanescape` organization's repositories use its runner service, `lanescape/runners`, through five actions in this repository, `lanescape/actions/<name>@v1`. They call the service through the interface it publishes and keeps compatible, its spec 0005, "The interface for actions", which is all they may rely on. Each works inside the organization and degrades gracefully outside it, so the same workflow runs in a fork, at GitHub's expense, as far as it can without the service.

## Where an action is

Each action decides the same way, and says which in the job's summary:

- **Inside the organization:** `github.repository_owner_id` is the organization's immutable ID, `endpoints.Organization` in the service's interface, never compared by name, and either the run isn't from a fork's pull request, by the event's head repository, or it's a `pull_request_target` run, whose jobs the service accepts only once a maintainer approves them.
- **Outside:** anything else, including a fork's own pushes, another organization, and a fork's pull request into one of the organization's repositories, which runs in the base repository but never on the service's machines.

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
          disks: '{"build": {"runner": "lanescape-80vcpu-linux-amd64", "spot": true, "key": "3f9c2e7a10b4d5e6f708"}}'
  build:
    needs: runners
    runs-on: ${{ fromJSON(needs.runners.outputs.map)['build'] }}
```

The output `map` has an entry for each name in `.github/runners.json`, and one for each entry in `disks`, named by its family:

- **Inside the organization,** each runner name maps to its selector for this run, with the scope the binding returned: `lanescape-80vcpu-linux-amd64-pr42`. Each disk entry maps to its runner's selector with `-spot` if asked and `-disk-<family>-<key>`: `lanescape-80vcpu-linux-amd64-spot-disk-build-3f9c2e7a10b4d5e6f708-pr42`. The service checks every selector against the run's binding before a job runs. If the binding answers that GitHub couldn't be reached, the action retries with backoff, and fails only once its retries are spent.
- **Outside,** each runner name maps to its fallback, and each disk entry to its runner's fallback. A fork can override any fallback with a repository or organization variable `RUNNERS`, a JSON object in the same shape as `.github/runners.json`, whose values may be a label or a list of labels. Names it doesn't override keep the file's fallback, and names the file doesn't list are ignored. For a fork's pull request into an organization repository, the action ignores `RUNNERS` from the base repository, since it's the organization's.

A job that asks for a name the map lacks gets an empty label, which GitHub refuses; the action's summary lists the names it has.

## `checkout`

Checks out the repository for a job. With no inputs, it checks out the run's own commit, as `actions/checkout` does. With `contribution: true`, in a `pull_request_target` run, it checks out exactly `github.event.pull_request.head.sha`, the commit the approved run was made for, from the head repository, with no credentials persisted, and refuses in any other event. Outside the organization, `contribution: true` does the same in a fork's own `pull_request_target` runs, which use nothing of the service's.

## `save`

Asks for the job's work disk to be saved when it finishes, by writing the service's save request on the machine. The service saves the disk if the job succeeded and its scope and selector allow. Outside the organization, or on a machine that isn't the service's, it does nothing and says so.

## `proxy`

Points a job's reads of upstream sources at the service's proxy. It reads the consumer's `.github/proxy.json` from the default branch through GitHub's API, as the service does. On the service's machines it uses the forwarder the machine already runs, and configures git to fetch the consumer's allowlisted git upstreams through it. On other runners it builds the service's forwarder from the version of `lanescape/runners` this repository's `go.mod` names and starts it, for HTTP reads only, since the proxy's git mirror is reachable only from the service's machines; git there reads straight from upstream. Where the proxy won't serve the job, it does nothing, and reads go straight to upstream: outside the organization, and on runners other than the service's for a `pull_request_target` run, whose GitHub token the proxy refuses. Tools other than git use `fetch`, or the forwarder's address, which the action sets as `LANESCAPE_PROXY`. The job needs `contents: read`, to read `proxy.json`, and `id-token: write`, for the forwarder's token on runners other than the service's; without them the action fails, saying which is missing.

## `fetch`

Downloads a URL through the proxy to a file, with an optional expected SHA-256, which it checks before writing the file. It needs the same permissions as `proxy`, and starts the forwarder the same way if `proxy` hasn't. Outside the organization, or for an upstream the consumer hasn't allowlisted, it downloads straight from upstream, and still checks the SHA-256.

## How actions run

Each action is a composite action that builds its Go program from the action's own source with `go build`, run in the repository root of the action's own downloaded copy, `$GITHUB_ACTION_PATH/..`, into `$RUNNER_TEMP`, and runs the binary. What runs is exactly the code at the commit the action was referenced at, with no separate release to keep in step, and the job sees the program's own exit status, which `go run` wouldn't give. The service's base image has Go; elsewhere, the action sets up that version with `actions/setup-go` from `go.mod`. Builds use Go's build cache, so later steps of a job build in moments. The programs are pure Go, with CGO off, and CI builds them for every platform runners use: Linux on amd64 and arm64, macOS on arm64, and Windows on amd64.

## Versions

Consumers reference `@v1`, a tag that moves (specs/0002-releases.md). `v1` stays compatible: its actions keep their inputs, outputs and behaviour. A change that would break them comes as `v2`, while `v1` keeps working, until no consumer references it.
