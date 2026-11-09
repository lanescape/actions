// Package e2e runs the actions' programs as GitHub would run them, with a
// job's environment and event, against the real runner service at the
// version go.mod names, built from lanescape/runners by ../servicetest, and
// that repository's own GitHub emulator and its OIDC issuer
// (specs/0002-releases.md, Tests).
//
// The programs are built with the e2e tag, which lets them call the test
// service in place of endpoints.Service and wait milliseconds between
// retries; nothing else differs from what the actions build.
package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/actions/internal/selector"
	"github.com/lanescape/actions/internal/testgithub"
)

const (
	serviceAppID = 77
	bootstrap    = "lanescape/app/.github/workflows/bootstrap.yml@refs/heads/main"
	runnersJSON  = `{
  "lanescape-80vcpu-linux-amd64": "ubuntu-24.04",
  "lanescape-2vcpu-linux-arm64": ["ubuntu-24.04-arm"],
  "bootstrap": "` + bootstrap + `"
}`
	disksInput = `{"build": {"runner": "lanescape-80vcpu-linux-amd64", "spot": true, "from": ["deps/versions.json", "rust-toolchain.toml"]},
  "cache": {"runner": "lanescape-2vcpu-linux-arm64"}}`
)

// bins are the programs, built once.
var bins struct {
	dir                        string
	runners, checkout, harness string
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
	bins.harness = filepath.Join(dir, "servicetest")
	build := func(wd string, args ...string) error {
		cmd := exec.Command("go", append([]string{"build", "-trimpath"}, args...)...)
		cmd.Dir = wd
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("go build %v: %v\n%s", args, err, out)
		}
		return nil
	}
	if err := build("..", "-tags", "e2e", "-o", bins.runners, "./runners"); err != nil {
		return 0, err
	}
	if err := build("..", "-tags", "e2e", "-o", bins.checkout, "./checkout"); err != nil {
		return 0, err
	}
	if err := build("../servicetest", "-o", bins.harness, "."); err != nil {
		return 0, err
	}
	return m.Run(), nil
}

type world struct {
	t           *testing.T
	gh          *testgithub.GitHub
	org         *ghemu.Account
	repo        *ghemu.Repo
	member      *ghemu.Account
	contributor *ghemu.Account
	fork        *ghemu.Repo
	service     string
}

// newWorld starts the GitHub emulator, with the organization by its real
// ID and a repository, and the service against it.
func newWorld(t *testing.T) *world {
	t.Helper()
	if testing.Short() {
		t.Skip("end-to-end tests run the service")
	}
	gh := testgithub.New(t)
	org := gh.Org(endpoints.Organization, "lanescape")
	gh.SetForkApproval(org, ghemu.ApproveAllExternalContributors)
	app := gh.NewApp(serviceAppID)
	gh.Install(app, org)
	repo := gh.Repo(org, 5001, "app")
	repo.Commit("main", map[string]string{
		".github/runners.json": runnersJSON,
		"deps/versions.json":   "café 1.0\n",
		"rust-toolchain.toml":  "\n",
	})
	member := gh.User(8001, "member")
	contributor := gh.User(9001, "contributor")
	fork := gh.Fork(repo, contributor, 7001)

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(keyFile, app.PrivateKeyPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	addr := filepath.Join(dir, "address")
	var logs bytes.Buffer
	cmd := exec.Command(bins.harness, "-github", gh.URL, "-app-id", strconv.Itoa(serviceAppID), "-app-key", keyFile,
		"-issuer", gh.Actions.URL, "-jwks", gh.Actions.JWKSURL, "-address-file", addr)
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("the service's log:\n%s", logs.String())
		}
	})
	var service string
	for deadline := time.Now().Add(30 * time.Second); service == ""; {
		if b, err := os.ReadFile(addr); err == nil && strings.HasSuffix(string(b), "\n") {
			service = strings.TrimSpace(string(b))
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the service didn't start:\n%s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	return &world{t: t, gh: gh, org: org, repo: repo, member: member, contributor: contributor, fork: fork, service: service}
}

// result is what a step left.
type result struct {
	err     error
	log     string
	outputs map[string]string
	summary string
}

// step runs a program as a step of job, as GitHub would.
func (w *world) step(bin string, j testgithub.Job, args ...string) result {
	w.t.Helper()
	env := w.gh.Env(j)
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "LANESCAPE_TEST_SERVICE=" + w.service}
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
	run := w.gh.PushRun(w.repo, "main")
	r := w.step(bins.runners, testgithub.Job{Run: run, IDToken: true, Inputs: map[string]string{"disks": disksInput}})
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
	// The token's audience is the service.
	if a := w.gh.Audiences; len(a) != 1 || a[0] != w.service {
		t.Fatalf("audiences = %v", a)
	}
	// Binding again, as a rerun's first job does, gives the same binding.
	again := w.step(bins.runners, testgithub.Job{Run: run, IDToken: true, Inputs: map[string]string{"disks": disksInput}})
	if again.outputs["map"] != r.outputs["map"] {
		t.Fatalf("rebinding gave %s", again.outputs["map"])
	}
}

func TestAPullRequestsRunBindsItsScopeAndKeys(t *testing.T) {
	w := newWorld(t)
	w.repo.Commit("deps", map[string]string{"deps/versions.json": "café 2.0\n"})
	pr := w.gh.PullRequest(w.repo, "main", w.repo, "deps", w.member)
	run := w.gh.PullRequestRun(pr)
	r := w.step(bins.runners, testgithub.Job{Run: run, Pull: pr, IDToken: true, Inputs: map[string]string{"disks": disksInput}})
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
}

func TestAForksRunFallsBack(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.fork, "main")
	r := w.step(bins.runners, testgithub.Job{Run: run, IDToken: true, Inputs: map[string]string{
		"disks":     disksInput,
		"fallbacks": `{"lanescape-2vcpu-linux-arm64": ["self-hosted", "arm64"]}`,
	}})
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
	w.fork.Commit("change", map[string]string{".github/runners.json": `{"lanescape-80vcpu-linux-amd64": "ubuntu-26.04"}`})
	pr := w.gh.PullRequest(w.repo, "main", w.fork, "change", w.contributor)
	run := w.gh.PullRequestRun(pr)
	r := w.step(bins.runners, testgithub.Job{Run: run, Pull: pr, IDToken: true, Inputs: map[string]string{
		"fallbacks": `{"lanescape-80vcpu-linux-amd64": "organization-runner"}`,
	}})
	// The run's commit is the merge of the fork's change, whose file it
	// reads; the organization's RUNNERS is ignored.
	expectMap(t, r.runnersMap(t), map[string]any{"lanescape-80vcpu-linux-amd64": "ubuntu-26.04"})
	if len(w.gh.Audiences) != 0 {
		t.Fatal("a fork's pull request asked for a token for the service")
	}
	if !strings.Contains(r.summary, "a fork's pull request") {
		t.Fatalf("summary = %s", r.summary)
	}
	// Its checkout is the run's own commit, as actions/checkout's.
	c := w.step(bins.checkout, testgithub.Job{Run: run, Pull: pr}, "decide")
	if c.err != nil || c.outputs["repository"] != "" || c.outputs["ref"] != "" || c.outputs["persist-credentials"] != "true" {
		t.Fatalf("checkout = %v %v\n%s", c.err, c.outputs, c.log)
	}
}

func TestAnApprovedContribution(t *testing.T) {
	w := newWorld(t)
	head := w.fork.Commit("change", map[string]string{"deps/versions.json": "café 3.0\n", ".github/runners.json": `{"lanescape-2vcpu-linux-amd64": "x"}`})
	pr := w.gh.PullRequest(w.repo, "main", w.fork, "change", w.contributor)
	run := w.gh.PullRequestTargetRun(pr)
	// The contributor pushes again once the run was approved: its jobs
	// still build the commit it was approved for.
	w.fork.Commit("change", map[string]string{"deps/versions.json": "café 4.0\n"})
	w.gh.Sync(pr)
	if pr.HeadSHA == head {
		t.Fatal("the pull request didn't move")
	}
	// The bootstrap workflow's job binds the contribution's scope.
	r := w.step(bins.runners, testgithub.Job{Run: run, Pull: pr, IDToken: true, Claims: ghemu.Job{WorkflowRef: bootstrap},
		Inputs: map[string]string{"disks": disksInput}})
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
	// The contribution's jobs check out exactly its head.
	c := w.step(bins.checkout, testgithub.Job{Run: run, Pull: pr, Inputs: map[string]string{"contribution": "true"}}, "decide")
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
	w.fork.Commit("change", map[string]string{"a": "1"})
	pr := w.gh.PullRequest(w.repo, "main", w.fork, "change", w.contributor)
	run := w.gh.PullRequestTargetRun(pr)
	r := w.step(bins.runners, testgithub.Job{Run: run, Pull: pr, IDToken: true})
	expectMap(t, r.runnersMap(t), map[string]any{
		"lanescape-2vcpu-linux-arm64":  "lanescape-2vcpu-linux-arm64-read",
		"lanescape-80vcpu-linux-amd64": "lanescape-80vcpu-linux-amd64-read",
	})
}

func TestTheServiceRetriesWhileGitHubCantBeReached(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	// The service's reads of the repository fail, and the service asks
	// the action to try again.
	w.gh.ServerError("/repositories/", 503, 3)
	r := w.step(bins.runners, testgithub.Job{Run: run, IDToken: true})
	expectMap(t, r.runnersMap(t), map[string]any{
		"lanescape-2vcpu-linux-arm64":  "lanescape-2vcpu-linux-arm64-main",
		"lanescape-80vcpu-linux-amd64": "lanescape-80vcpu-linux-amd64-main",
	})
	if !strings.Contains(r.log, "trying again") || len(w.gh.Audiences) < 2 {
		t.Fatalf("no retry: %d tokens\n%s", len(w.gh.Audiences), r.log)
	}
}

func TestTheActionFailsOnceItsRetriesAreSpent(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	w.gh.ServerError("/repositories/", 503, 1000)
	r := w.step(bins.runners, testgithub.Job{Run: run, IDToken: true})
	if r.err == nil || !strings.Contains(r.log, "retries are spent") || r.outputs["map"] != "" {
		t.Fatalf("err = %v, outputs %v\n%s", r.err, r.outputs, r.log)
	}
}

func TestARefusedBindingFailsWithItsReason(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	w.gh.SetRunStatus(run, "completed")
	r := w.step(bins.runners, testgithub.Job{Run: run, IDToken: true})
	if r.err == nil || !strings.Contains(r.log, "refused to bind the run") || !strings.Contains(r.log, "not in progress") {
		t.Fatalf("err = %v\n%s", r.err, r.log)
	}
}

func TestAMissingListedFileFailsNamingIt(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	r := w.step(bins.runners, testgithub.Job{Run: run, IDToken: true, Inputs: map[string]string{
		"disks": `{"build": {"runner": "lanescape-80vcpu-linux-amd64", "from": ["deps/missing.json"]}}`,
	}})
	if r.err == nil || !strings.Contains(r.log, "deps/missing.json isn't in lanescape/app") {
		t.Fatalf("err = %v\n%s", r.err, r.log)
	}
}

func TestCheckoutRefusesAContributionOutsidePullRequestTarget(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	r := w.step(bins.checkout, testgithub.Job{Run: run, Inputs: map[string]string{"contribution": "true"}}, "decide")
	if r.err == nil || !strings.Contains(r.log, "only in a pull_request_target run") || len(r.outputs) != 0 {
		t.Fatalf("err = %v, outputs %v\n%s", r.err, r.outputs, r.log)
	}
}
