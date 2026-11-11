// Package e2e runs the actions' programs as GitHub would run them, with a
// job's environment and event, against lanescape/runners' emulator of the
// service, emulators/service, at the version go.mod names, and that
// repository's own GitHub emulator and its OIDC issuer
// (specs/0002-releases.md, Tests). That repository's conformance suite
// holds the emulator to the real service's answers.
//
// There is one GitHub: each test makes its repository, branches, pull
// requests, runs and jobs in the GitHub emulator, which serves the API,
// event and tokens a job sees, and the service emulator reads the same
// state through service.WithGitHub. Its RepositoryOf, PullRequestOf and
// RunOf give the service's handles for what a test made there.
//
// The programs are built with the e2e tag, which lets them call the
// emulator in place of endpoints.Service and wait milliseconds between
// retries; nothing else differs from what the actions build.
package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/emulators/service"
	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/actions/internal/selector"
	"github.com/lanescape/actions/internal/testgithub"
)

const (
	bootstrap   = "lanescape/app/.github/workflows/bootstrap.yml@refs/heads/main"
	runnersJSON = `{
  "lanescape-80vcpu-linux-amd64": "ubuntu-24.04",
  "lanescape-2vcpu-linux-arm64": ["ubuntu-24.04-arm"],
  "bootstrap": "` + bootstrap + `",
  "branches": {"support/v1": "v1"}
}`
	disksInput = `{"build": {"runner": "lanescape-80vcpu-linux-amd64", "spot": true, "from": ["deps/versions.json", "rust-toolchain.toml"]},
  "cache": {"runner": "lanescape-2vcpu-linux-arm64"}}`
)

// bins are the programs, built once.
var bins struct {
	dir               string
	runners, checkout string
}

func TestMain(m *testing.M) {
	code, err := buildAndRun(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		code = 1
	}
	os.Exit(code)
}

func buildAndRun(m *testing.M) (int, error) {
	dir, err := os.MkdirTemp("", "lanescape-actions-e2e-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	bins.dir = dir
	bins.runners = filepath.Join(dir, "runners")
	bins.checkout = filepath.Join(dir, "checkout")
	build := func(args ...string) error {
		cmd := exec.Command("go", append([]string{"build", "-trimpath", "-tags", "e2e"}, args...)...)
		cmd.Dir = ".."
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("go build %v: %v\n%s", args, err, out)
		}
		return nil
	}
	if err := build("-o", bins.runners, "./runners"); err != nil {
		return 0, err
	}
	if err := build("-o", bins.checkout, "./checkout"); err != nil {
		return 0, err
	}
	return m.Run(), nil
}

type world struct {
	t           *testing.T
	gh          *testgithub.GitHub
	svc         *service.Emulator
	org         *ghemu.Account
	repo        *ghemu.Repo
	srepo       service.Repository
	member      *ghemu.Account
	contributor *ghemu.Account
}

// newWorld starts the GitHub emulator, with the organization by its real
// ID and a repository, and the service emulator, which checks jobs' tokens
// against the GitHub emulator's OIDC issuer and knows the same repository.
func newWorld(t *testing.T) *world {
	t.Helper()
	if testing.Short() {
		t.Skip("end-to-end tests run the service emulator and the built programs")
	}
	gh := testgithub.New(t)
	svc := service.New(service.WithGitHub(gh.Emulator))
	t.Cleanup(svc.Close)
	if svc.Organization() != endpoints.Organization {
		t.Fatalf("the service emulator serves %d, not the organization", svc.Organization())
	}
	org := gh.Org(endpoints.Organization, "lanescape")
	gh.SetForkApproval(org, ghemu.ApproveAllExternalContributors)
	repo := gh.Repo(org, gh.NewID(), "app")
	srepo := svc.RepositoryOf(repo)
	repo.Commit("main", map[string]string{
		".github/runners.json": runnersJSON,
		"deps/versions.json":   "café 1.0\n",
		"rust-toolchain.toml":  "\n",
	})
	member := gh.User(8001, "member")
	contributor := gh.User(9001, "contributor")
	return &world{t: t, gh: gh, svc: svc, org: org, repo: repo, srepo: srepo, member: member, contributor: contributor}
}

// push is a push to a branch, as GitHub and the service see it.
func (w *world) push(branch string) (*ghemu.Run, service.Run) {
	run := w.gh.PushRun(w.repo, branch)
	return run, w.svc.RunOf(run)
}

// job starts a job of a run on one of GitHub's own runners, as the service
// sees it; workflowRef is its job_workflow_ref, or empty for the run's own
// workflow's.
func (w *world) job(run service.Run, workflowRef string) *service.Job {
	j := w.svc.StartJob(run, workflowRef)
	return &j
}

// pullRequest opens a pull request into main from a new branch, head, of
// the repository, with files changed.
func (w *world) pullRequest(head string, files map[string]string) (*ghemu.Pull, service.PullRequest) {
	w.t.Helper()
	w.repo.Commit(head, files)
	pr := w.gh.PullRequest(w.repo, "main", w.repo, head, w.member)
	return pr, w.svc.PullRequestOf(pr)
}

// forkPullRequest opens a pull request into main from the contributor's
// fork, from its branch head with files changed. The fork is made in the
// GitHub emulator, which the service emulator reads.
func (w *world) forkPullRequest(head string, files map[string]string) (*ghemu.Repo, *ghemu.Pull, service.PullRequest) {
	w.t.Helper()
	fork := w.gh.Fork(w.repo, w.contributor, w.gh.NewID())
	fork.Commit(head, files)
	pr := w.gh.PullRequest(w.repo, "main", fork, head, w.contributor)
	return fork, pr, w.svc.PullRequestOf(pr)
}

// bound checks the run's binding as the service emulator holds it.
func (w *world) bound(run service.Run, want string) {
	w.t.Helper()
	if got, ok := w.svc.Binding(run); !ok || got != want {
		w.t.Fatalf("the service bound %q (%v), not %q", got, ok, want)
	}
}

// result is what a step left.
type result struct {
	err     error
	log     string
	outputs map[string]string
	summary string
}

// step runs a program as a step of job, as GitHub would. sj, if not nil,
// is the same job as the service emulator knows it, which then mints the
// job's OIDC tokens, with the audience the program asks for.
func (w *world) step(bin string, j testgithub.Job, sj *service.Job, args ...string) result {
	w.t.Helper()
	if sj != nil {
		job := *sj
		j.OIDC = func(audience string) string { return w.svc.Token(job, map[string]any{"aud": audience}) }
	}
	env := w.gh.Env(j)
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "LANESCAPE_TEST_SERVICE=" + w.svc.URL()}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	err := cmd.Run()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		w.t.Fatal(err)
	}
	r := result{err: err, log: log.String(), outputs: testgithub.Outputs(w.t, env), summary: testgithub.Summary(w.t, env)}
	// No token handed to the job appears in its log, but to be masked.
	for _, s := range w.gh.Secrets() {
		if strings.Contains(strings.ReplaceAll(r.log, "::add-mask::"+s, ""), s) {
			w.t.Fatalf("a token is in the step's log:\n%s", r.log)
		}
	}
	return r
}

func (r result) runnersMap(t *testing.T) map[string]any {
	t.Helper()
	if r.err != nil {
		t.Fatalf("the step failed: %v\n%s", r.err, r.log)
	}
	m := map[string]any{}
	if err := json.Unmarshal([]byte(r.outputs["map"]), &m); err != nil {
		t.Fatalf("map %q: %v", r.outputs["map"], err)
	}
	return m
}

func key(family, runner string, files ...string) string {
	var fs []selector.File
	for i := 0; i < len(files); i += 2 {
		fs = append(fs, selector.File{Path: files[i], Contents: []byte(files[i+1])})
	}
	return selector.DiskKey(family, runner, fs)
}

func expectMap(t *testing.T, got map[string]any, want map[string]any) {
	t.Helper()
	g, _ := json.Marshal(got)
	wnt, _ := json.Marshal(want)
	if string(g) != string(wnt) {
		t.Fatalf("map = %s\nwant  %s", g, wnt)
	}
}

func TestAPushToMainBindsMain(t *testing.T) {
	w := newWorld(t)
	run, srun := w.push("main")
	r := w.step(bins.runners, testgithub.Job{Run: run, IDToken: true, Inputs: map[string]string{"disks": disksInput}}, w.job(srun, ""))
	k := key("build", "lanescape-80vcpu-linux-amd64", "deps/versions.json", "café 1.0\n", "rust-toolchain.toml", "\n")
	expectMap(t, r.runnersMap(t), map[string]any{
		"build":                        "lanescape-80vcpu-linux-amd64-spot-disk-build-" + k + "-main",
		"cache":                        "lanescape-2vcpu-linux-arm64-disk-cache-00000000000000000000-main",
		"lanescape-2vcpu-linux-arm64":  "lanescape-2vcpu-linux-arm64-main",
		"lanescape-80vcpu-linux-amd64": "lanescape-80vcpu-linux-amd64-main",
	})
	if !strings.Contains(r.summary, "Inside the lanescape organization") || !strings.Contains(r.summary, "scope is `main`") {
		t.Fatalf("summary = %s", r.summary)
	}
	w.bound(srun, "main")
	// The token's audience is the service.
	if a := w.gh.Audiences; len(a) != 1 || a[0] != w.svc.URL() {
		t.Fatalf("audiences = %v", a)
	}
	// Binding again, as a rerun's first job does, gives the same binding.
	w.gh.Rerun(run)
	srun = w.svc.RunOf(run)
	again := w.step(bins.runners, testgithub.Job{Run: run, IDToken: true, Inputs: map[string]string{"disks": disksInput}}, w.job(srun, ""))
	if again.outputs["map"] != r.outputs["map"] {
		t.Fatalf("rebinding gave %s", again.outputs["map"])
	}
}

func TestAPushToAListedBranchBindsItsScope(t *testing.T) {
	w := newWorld(t)
	w.repo.Commit("support/v1", map[string]string{"deps/versions.json": "café 0.9\n"})
	run, srun := w.push("support/v1")
	r := w.step(bins.runners, testgithub.Job{Run: run, IDToken: true, Inputs: map[string]string{"disks": disksInput}}, w.job(srun, ""))
	// The files are read at the branch's commit.
	k := key("build", "lanescape-80vcpu-linux-amd64", "deps/versions.json", "café 0.9\n", "rust-toolchain.toml", "\n")
	expectMap(t, r.runnersMap(t), map[string]any{
		"build":                        "lanescape-80vcpu-linux-amd64-spot-disk-build-" + k + "-branch-v1",
		"cache":                        "lanescape-2vcpu-linux-arm64-disk-cache-00000000000000000000-branch-v1",
		"lanescape-2vcpu-linux-arm64":  "lanescape-2vcpu-linux-arm64-branch-v1",
		"lanescape-80vcpu-linux-amd64": "lanescape-80vcpu-linux-amd64-branch-v1",
	})
	if !strings.Contains(r.summary, "scope is `branch-v1`") {
		t.Fatalf("summary = %s", r.summary)
	}
	w.bound(srun, "branch-v1")
}

func TestAPullRequestsRunBindsItsScopeAndKeys(t *testing.T) {
	w := newWorld(t)
	pr, _ := w.pullRequest("deps", map[string]string{"deps/versions.json": "café 2.0\n"})
	run := w.gh.PullRequestRun(pr)
	srun := w.svc.RunOf(run)
	r := w.step(bins.runners, testgithub.Job{Run: run, Pull: pr, IDToken: true, Inputs: map[string]string{"disks": disksInput}}, w.job(srun, ""))
	scope := fmt.Sprintf("pr%d", pr.Number)
	// The files are read at the run's commit, so the key is the pull
	// request's, beside main's rather than over it.
	k := key("build", "lanescape-80vcpu-linux-amd64", "deps/versions.json", "café 2.0\n", "rust-toolchain.toml", "\n")
	mainKey := key("build", "lanescape-80vcpu-linux-amd64", "deps/versions.json", "café 1.0\n", "rust-toolchain.toml", "\n")
	if k == mainKey {
		t.Fatal("the keys are the same")
	}
	expectMap(t, r.runnersMap(t), map[string]any{
		"build":                        "lanescape-80vcpu-linux-amd64-spot-disk-build-" + k + "-" + scope,
		"cache":                        "lanescape-2vcpu-linux-arm64-disk-cache-00000000000000000000-" + scope,
		"lanescape-2vcpu-linux-arm64":  "lanescape-2vcpu-linux-arm64-" + scope,
		"lanescape-80vcpu-linux-amd64": "lanescape-80vcpu-linux-amd64-" + scope,
	})
	w.bound(srun, scope)
}

func TestAForksRunFallsBack(t *testing.T) {
	w := newWorld(t)
	fork := w.gh.Fork(w.repo, w.contributor, 7001)
	run := w.gh.PushRun(fork, "main")
	r := w.step(bins.runners, testgithub.Job{Run: run, IDToken: true, Inputs: map[string]string{
		"disks":     disksInput,
		"fallbacks": `{"lanescape-2vcpu-linux-arm64": ["self-hosted", "arm64"]}`,
	}}, nil)
	expectMap(t, r.runnersMap(t), map[string]any{
		"build":                        "ubuntu-24.04",
		"cache":                        []any{"self-hosted", "arm64"},
		"lanescape-2vcpu-linux-arm64":  []any{"self-hosted", "arm64"},
		"lanescape-80vcpu-linux-amd64": "ubuntu-24.04",
	})
	if len(w.gh.Audiences) != 0 {
		t.Fatal("a fork's run asked for a token for the service")
	}
	if !strings.Contains(r.summary, "Outside the lanescape organization") {
		t.Fatalf("summary = %s", r.summary)
	}
}

func TestAForksPullRequestIntoTheOrganizationFallsBack(t *testing.T) {
	w := newWorld(t)
	_, pr, _ := w.forkPullRequest("change", map[string]string{".github/runners.json": `{"lanescape-80vcpu-linux-amd64": "ubuntu-26.04"}`})
	run := w.gh.PullRequestRun(pr)
	srun := w.svc.RunOf(run)
	r := w.step(bins.runners, testgithub.Job{Run: run, Pull: pr, IDToken: true, Inputs: map[string]string{
		"fallbacks": `{"lanescape-80vcpu-linux-amd64": "organization-runner"}`,
	}}, w.job(srun, ""))
	// The run's commit is the merge of the fork's change, whose file it
	// reads; the organization's RUNNERS is ignored.
	expectMap(t, r.runnersMap(t), map[string]any{"lanescape-80vcpu-linux-amd64": "ubuntu-26.04"})
	if len(w.gh.Audiences) != 0 {
		t.Fatal("a fork's pull request asked for a token for the service")
	}
	if _, ok := w.svc.Binding(srun); ok {
		t.Fatal("a fork's pull request bound its run")
	}
	if !strings.Contains(r.summary, "a fork's pull request") {
		t.Fatalf("summary = %s", r.summary)
	}
	// Its checkout is the run's own commit, as actions/checkout's.
	c := w.step(bins.checkout, testgithub.Job{Run: run, Pull: pr}, nil, "decide")
	if c.err != nil || c.outputs["repository"] != "" || c.outputs["ref"] != "" || c.outputs["persist-credentials"] != "true" {
		t.Fatalf("checkout = %v %v\n%s", c.err, c.outputs, c.log)
	}
}

func TestAnApprovedContribution(t *testing.T) {
	w := newWorld(t)
	fork, pr, _ := w.forkPullRequest("change", map[string]string{"deps/versions.json": "café 3.0\n", ".github/runners.json": `{"lanescape-2vcpu-linux-amd64": "x"}`})
	head := pr.HeadSHA
	run := w.gh.PullRequestTargetRun(pr)
	srun := w.svc.RunOf(run)
	// The contributor pushes again once the run was approved: its jobs
	// still build the commit it was approved for, from the run's own event,
	// and the service binds that commit as one of the pull request's.
	fork.Commit("change", map[string]string{"deps/versions.json": "café 4.0\n"})
	w.gh.Sync(pr)
	if pr.HeadSHA == head {
		t.Fatal("the pull request didn't move")
	}
	// The bootstrap workflow's job binds the contribution's scope.
	r := w.step(bins.runners, testgithub.Job{Run: run, Pull: pr, IDToken: true, Inputs: map[string]string{"disks": disksInput}},
		w.job(srun, w.svc.Bootstrap(w.srepo)))
	scope := fmt.Sprintf("pr%d", pr.Number)
	// runners.json is the default branch's; the disks' files are the
	// contribution's.
	k := key("build", "lanescape-80vcpu-linux-amd64", "deps/versions.json", "café 3.0\n", "rust-toolchain.toml", "\n")
	expectMap(t, r.runnersMap(t), map[string]any{
		"build":                        "lanescape-80vcpu-linux-amd64-spot-disk-build-" + k + "-" + scope,
		"cache":                        "lanescape-2vcpu-linux-arm64-disk-cache-00000000000000000000-" + scope,
		"lanescape-2vcpu-linux-arm64":  "lanescape-2vcpu-linux-arm64-" + scope,
		"lanescape-80vcpu-linux-amd64": "lanescape-80vcpu-linux-amd64-" + scope,
	})
	w.bound(srun, scope)
	// The contribution's jobs check out exactly its head.
	c := w.step(bins.checkout, testgithub.Job{Run: run, Pull: pr, Inputs: map[string]string{"contribution": "true"}}, nil, "decide")
	if c.err != nil {
		t.Fatalf("checkout: %v\n%s", c.err, c.log)
	}
	want := map[string]string{"repository": "contributor/app", "ref": head, "persist-credentials": "false", "allow-unsafe-pr-checkout": "true", "expect": head}
	for k, v := range want {
		if c.outputs[k] != v {
			t.Fatalf("checkout outputs = %v, want %v", c.outputs, want)
		}
	}
}

// Run directly in a pull_request_target workflow, not from the bootstrap,
// the action gets the read scope.
func TestAPullRequestTargetRunOutsideTheBootstrapReads(t *testing.T) {
	w := newWorld(t)
	_, pr, _ := w.forkPullRequest("change", map[string]string{"a": "1"})
	run := w.gh.PullRequestTargetRun(pr)
	srun := w.svc.RunOf(run)
	r := w.step(bins.runners, testgithub.Job{Run: run, Pull: pr, IDToken: true}, w.job(srun, ""))
	expectMap(t, r.runnersMap(t), map[string]any{
		"lanescape-2vcpu-linux-arm64":  "lanescape-2vcpu-linux-arm64-read",
		"lanescape-80vcpu-linux-amd64": "lanescape-80vcpu-linux-amd64-read",
	})
	w.bound(srun, "read")
}

func TestTheServiceRetriesWhileGitHubCantBeReached(t *testing.T) {
	w := newWorld(t)
	run, srun := w.push("main")
	// GitHub can't be reached for the service's first three answers,
	// which ask the action to try again, as the service's do.
	w.svc.Faults().Add(fault.Rule{Method: http.MethodPost, PathPrefix: "/v1/bind", Times: 3, Respond: service.Unavailable(time.Second)})
	r := w.step(bins.runners, testgithub.Job{Run: run, IDToken: true}, w.job(srun, ""))
	expectMap(t, r.runnersMap(t), map[string]any{
		"lanescape-2vcpu-linux-arm64":  "lanescape-2vcpu-linux-arm64-main",
		"lanescape-80vcpu-linux-amd64": "lanescape-80vcpu-linux-amd64-main",
	})
	// Each attempt gets a token afresh.
	if !strings.Contains(r.log, "trying again") || len(w.gh.Audiences) != 4 {
		t.Fatalf("no retry: %d tokens\n%s", len(w.gh.Audiences), r.log)
	}
	w.bound(srun, "main")
}

func TestTheActionFailsOnceItsRetriesAreSpent(t *testing.T) {
	w := newWorld(t)
	run, srun := w.push("main")
	w.svc.Unavailable(true)
	r := w.step(bins.runners, testgithub.Job{Run: run, IDToken: true}, w.job(srun, ""))
	if r.err == nil || !strings.Contains(r.log, "retries are spent") || r.outputs["map"] != "" {
		t.Fatalf("err = %v, outputs %v\n%s", r.err, r.outputs, r.log)
	}
	if _, ok := w.svc.Binding(srun); ok {
		t.Fatal("the run was bound while GitHub couldn't be reached")
	}
}

func TestARefusedBindingFailsWithItsReason(t *testing.T) {
	w := newWorld(t)
	run, srun := w.push("main")
	// The run's one job has finished, so GitHub shows the run completed.
	j := w.job(srun, "")
	w.svc.FinishJob(*j, "success")
	w.gh.SetRunStatus(run, "completed")
	r := w.step(bins.runners, testgithub.Job{Run: run, IDToken: true}, j)
	if r.err == nil || !strings.Contains(r.log, "refused to bind the run") || !strings.Contains(r.log, "the run isn't in progress") {
		t.Fatalf("err = %v\n%s", r.err, r.log)
	}
}

func TestAMissingListedFileFailsNamingIt(t *testing.T) {
	w := newWorld(t)
	run, srun := w.push("main")
	r := w.step(bins.runners, testgithub.Job{Run: run, IDToken: true, Inputs: map[string]string{
		"disks": `{"build": {"runner": "lanescape-80vcpu-linux-amd64", "from": ["deps/missing.json"]}}`,
	}}, w.job(srun, ""))
	if r.err == nil || !strings.Contains(r.log, "deps/missing.json isn't in lanescape/app") {
		t.Fatalf("err = %v\n%s", r.err, r.log)
	}
}

func TestCheckoutRefusesAContributionOutsidePullRequestTarget(t *testing.T) {
	w := newWorld(t)
	run, _ := w.push("main")
	r := w.step(bins.checkout, testgithub.Job{Run: run, Inputs: map[string]string{"contribution": "true"}}, nil, "decide")
	if r.err == nil || !strings.Contains(r.log, "only in a pull_request_target run") || len(r.outputs) != 0 {
		t.Fatalf("err = %v, outputs %v\n%s", r.err, r.outputs, r.log)
	}
}
