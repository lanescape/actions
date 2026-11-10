// Package e2e runs the actions' programs as GitHub would run them, with a
// job's environment and event, on one of the runner service's machines as
// Compute Engine gives it a metadata server, against lanescape/runners'
// emulator of the service, emulators/service, at the version go.mod
// names, and that repository's own GitHub emulator (specs/0002-releases.md,
// Tests). That repository's conformance suite holds the emulator to the
// real service's answers.
//
// There is one GitHub: each test makes its repository, branches, pull
// requests, runs and approvals in the GitHub emulator, which serves the
// API and event a job sees, and the service emulator reads the same state
// through service.WithGitHub. Its RepositoryOf, PullRequestOf and RunOf
// give the service's handles for what a test made there.
//
// The programs are built with the e2e tag, which lets them call the
// emulator in place of endpoints.Service, use a test's metadata server and
// machine directory, and wait milliseconds; nothing else differs from what
// the actions build.
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
	"github.com/lanescape/runners/machine"

	"github.com/lanescape/actions/internal/testgithub"
)

const small = "lanescape-2vcpu-linux-amd64"

// files are the repository's: two images, one for both architectures.
var files = map[string]string{
	".github/runners.json":            `{"images": {"build": ".github/images/build", "multi": ".github/images/multi"}}`,
	".github/images/build/image.json": `{"architectures": ["amd64"]}`,
	".github/images/build/init.sh":    "apt-get install -y gcc\n",
	".github/images/multi/image.json": `{"architectures": ["amd64", "arm64"]}`,
	".github/images/multi/init.sh":    "apt-get install -y clang\n",
}

// bins are the programs, built once.
var bins struct {
	dir                       string
	image, saveDisk, checkout string
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
	build := func(name string) (string, error) {
		out := filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-trimpath", "-tags", "e2e", "-o", out, "./"+name)
		cmd.Dir = ".."
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=")
		if b, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("go build %s: %v\n%s", name, err, b)
		}
		return out, nil
	}
	for name, into := range map[string]*string{"image": &bins.image, "save-disk": &bins.saveDisk, "checkout": &bins.checkout} {
		if *into, err = build(name); err != nil {
			return 0, err
		}
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
// ID and a repository whose commit defines images, and the service
// emulator, which reads the same GitHub.
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
	org := gh.Account(endpoints.Organization)
	repo := gh.Repo(org, gh.NewID(), "app")
	repo.Commit("main", files)
	gh.ProtectEnvironment(repo, "contribution")
	return &world{t: t, gh: gh, svc: svc, org: org, repo: repo, srepo: svc.RepositoryOf(repo), member: gh.User(8001, "member"),
		contributor: gh.User(9001, "contributor")}
}

// machine starts a job of a run on one of the service's base-image
// machines, which binds the run as the service's check would.
func (w *world) machine(run *ghemu.Run) service.Job {
	w.t.Helper()
	j, err := w.svc.StartJob(w.svc.RunOf(run), small)
	if err != nil {
		w.t.Fatalf("the job's check: %v", err)
	}
	return j
}

// result is what a step left.
type result struct {
	err     error
	log     string
	outputs map[string]string
	summary string
	dir     string
}

// step runs a program as a step of a job, as GitHub would. On a machine of
// the service's, sj is the job as the service emulator knows it, whose
// metadata server mints the machine's identity token, and the machine has
// the service's directory; without one, it's some other runner.
func (w *world) step(bin string, j testgithub.Job, sj *service.Job, args ...string) result {
	w.t.Helper()
	env := w.gh.Env(j)
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "LANESCAPE_TEST_SERVICE=" + w.svc.URL()}
	dir := filepath.Join(w.t.TempDir(), "lanescape-runners")
	if sj != nil {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			w.t.Fatal(err)
		}
		cmd.Env = append(cmd.Env, "LANESCAPE_TEST_METADATA="+w.svc.Metadata(*sj))
	}
	cmd.Env = append(cmd.Env, "LANESCAPE_TEST_MACHINE_DIR="+dir)
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
	r := result{err: err, log: log.String(), outputs: testgithub.Outputs(w.t, env), summary: testgithub.Summary(w.t, env), dir: dir}
	// No token handed to the job appears in its log, nor the machine's
	// identity token.
	secrets := w.gh.Secrets()
	if sj != nil {
		secrets = append(secrets, w.svc.Token(*sj))
	}
	for _, s := range secrets {
		if strings.Contains(strings.ReplaceAll(r.log, "::add-mask::"+s, ""), s) {
			w.t.Fatalf("a token is in the step's log:\n%s", r.log)
		}
	}
	if strings.Contains(r.log, "eyJ") {
		w.t.Fatalf("something like a token is in the step's log:\n%s", r.log)
	}
	return r
}

func (r result) ok(t *testing.T) result {
	t.Helper()
	if r.err != nil {
		t.Fatalf("the step failed: %v\n%s", r.err, r.log)
	}
	return r
}

func (r result) failed(t *testing.T, says string) {
	t.Helper()
	if r.err == nil || !strings.Contains(r.log, says) {
		t.Fatalf("err = %v; want a failure saying %q:\n%s", r.err, says, r.log)
	}
}

// ask is the build IDs the service started for a run's image, asked for as
// the action does, from another job of the run.
func (w *world) builds(run *ghemu.Run, image string) map[string]string {
	w.t.Helper()
	j := w.machine(run)
	req, _ := http.NewRequest(http.MethodPost, w.svc.URL()+"/v1/images", strings.NewReader(`{"name": "`+image+`"}`))
	req.Header.Set("Authorization", "Bearer "+w.svc.Token(j))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Builds []struct {
			Architecture, Build, State string
		} `json:"builds"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK {
		w.t.Fatalf("asking for %s: %d %v", image, resp.StatusCode, err)
	}
	ids := map[string]string{}
	for _, b := range out.Builds {
		ids[b.Architecture] = b.Build
	}
	return ids
}

func TestAnImageAlreadyBuilt(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	w.svc.FinishBuild(w.srepo, w.builds(run, "build")["amd64"], "built\n")
	j := w.machine(run)
	r := w.step(bins.image, testgithub.Job{Run: run, Inputs: map[string]string{"name": "build"}}, &j).ok(t)
	if !strings.Contains(r.summary, "Inside the lanescape organization") || !strings.Contains(r.summary, "`amd64`") {
		t.Fatalf("summary = %s", r.summary)
	}
	if strings.Contains(r.log, "built") {
		t.Fatalf("a ready image's log was printed:\n%s", r.log)
	}
}

func TestAnImageBuiltWhileTheActionWaits(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	id := w.builds(run, "build")["amd64"]
	go func() {
		time.Sleep(300 * time.Millisecond)
		w.svc.FinishBuild(w.srepo, id, "installing gcc\n::add-mask::secret\n::set-output name=x::y")
	}()
	j := w.machine(run)
	r := w.step(bins.image, testgithub.Job{Run: run, Inputs: map[string]string{"name": "build"}}, &j).ok(t)
	if !strings.Contains(r.log, "installing gcc") || !strings.Contains(r.log, "::group::amd64 ") {
		t.Fatalf("the build's log wasn't printed in its group:\n%s", r.log)
	}
	stopped(t, r.log)
}

// stopped checks that every part of a build's log was written with
// workflow commands stopped, by a token of its own, and resumed after it,
// each on a line of its own.
func stopped(t *testing.T, log string) {
	t.Helper()
	lines := strings.Split(log, "\n")
	var token string
	tokens := map[string]bool{}
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "::stop-commands::"):
			if token != "" {
				t.Fatalf("commands stopped twice:\n%s", log)
			}
			token = strings.TrimPrefix(l, "::stop-commands::")
			if len(token) < 32 || tokens[token] {
				t.Fatalf("a part's token %q isn't new and random", token)
			}
			tokens[token] = true
		case token != "" && l == "::"+token+"::":
			token = ""
		case token == "" && (strings.Contains(l, "::add-mask::") || strings.Contains(l, "::set-output")):
			t.Fatalf("a line of the build's log at %d can act as a command:\n%s", i, log)
		}
	}
	if token != "" || len(tokens) == 0 {
		t.Fatalf("commands weren't stopped and resumed:\n%s", log)
	}
}

func TestOneArchitectureReadyWhileAnotherBuilds(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	ids := w.builds(run, "multi")
	w.svc.FinishBuild(w.srepo, ids["amd64"], "amd64 done\n")
	go func() {
		time.Sleep(200 * time.Millisecond)
		w.svc.FinishBuild(w.srepo, ids["arm64"], "arm64 part one\n")
	}()
	j := w.machine(run)
	r := w.step(bins.image, testgithub.Job{Run: run, Inputs: map[string]string{"name": "multi"}}, &j).ok(t)
	if !strings.Contains(r.log, "arm64 part one") || strings.Contains(r.log, "amd64 done") {
		t.Fatalf("log:\n%s", r.log)
	}
	if !strings.Contains(r.summary, "`amd64`") || !strings.Contains(r.summary, "`arm64`") {
		t.Fatalf("summary = %s", r.summary)
	}
}

func TestALogInPartsWithoutAFinalNewline(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	id := w.builds(run, "build")["amd64"]
	// The log comes in parts while the action follows it, one of them
	// ending mid-line, and tries workflow commands across them.
	go func() {
		w.svc.WriteLog(id, "::warning::first part\n::add-")
		time.Sleep(150 * time.Millisecond)
		w.svc.WriteLog(id, "mask::half\n::error::no newline")
		time.Sleep(150 * time.Millisecond)
		w.svc.FinishBuild(w.srepo, id, "")
	}()
	j := w.machine(run)
	r := w.step(bins.image, testgithub.Job{Run: run, Inputs: map[string]string{"name": "build"}}, &j).ok(t)
	if !strings.Contains(r.log, "first part") || !strings.Contains(r.log, "no newline") {
		t.Fatalf("log:\n%s", r.log)
	}
	stopped(t, r.log)
	// The action's own commands work again after the log.
	if i := strings.LastIndex(r.log, "::endgroup::"); i < 0 || !strings.Contains(r.log[i:], "is ready") {
		t.Fatalf("the action didn't go on after the log:\n%s", r.log)
	}
}

func TestAFailedBuild(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	id := w.builds(run, "build")["amd64"]
	go func() {
		time.Sleep(200 * time.Millisecond)
		w.svc.FailBuild(w.srepo, id, "E: Unable to locate package gcc\n")
	}()
	j := w.machine(run)
	r := w.step(bins.image, testgithub.Job{Run: run, Inputs: map[string]string{"name": "build"}}, &j)
	r.failed(t, "the image build's build for amd64 failed")
	if !strings.Contains(r.log, "Unable to locate package gcc") {
		t.Fatalf("the failed build's log wasn't printed:\n%s", r.log)
	}
	// Asked again in the same attempt, the action fails at once with the
	// service's reason.
	j2 := w.machine(run)
	w.step(bins.image, testgithub.Job{Run: run, Inputs: map[string]string{"name": "build"}}, &j2).failed(t, "scripts failed")
	// A maintainer's rerun asks for a new build, and the action waits for
	// it.
	w.gh.Rerun(run)
	again := w.builds(run, "build")["amd64"]
	if again == id {
		t.Fatal("the rerun got the failed build")
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		w.svc.FinishBuild(w.srepo, again, "built at last\n")
	}()
	j3 := w.machine(run)
	if r := w.step(bins.image, testgithub.Job{Run: run, Inputs: map[string]string{"name": "build"}}, &j3).ok(t); !strings.Contains(r.log, "built at last") {
		t.Fatalf("the rerun's build's log:\n%s", r.log)
	}
}

func TestAnImageTheCommitDoesntDeclare(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	j := w.machine(run)
	w.step(bins.image, testgithub.Job{Run: run, Inputs: map[string]string{"name": "other"}}, &j).
		failed(t, "doesn't declare the image other")
}

func TestOnAMachineThatIsntTheServices(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	w.step(bins.image, testgithub.Job{Run: run, Inputs: map[string]string{"name": "build"}}, nil).
		failed(t, "must run on one of the runner service's machines")
	r := w.step(bins.saveDisk, testgithub.Job{Run: run}, nil).ok(t)
	if !strings.Contains(r.log, "isn't one of the runner service's machines") {
		t.Fatalf("log:\n%s", r.log)
	}
}

func TestAForksOwnRun(t *testing.T) {
	w := newWorld(t)
	fork := w.gh.Fork(w.repo, w.contributor, w.gh.NewID())
	run := w.gh.PushRun(fork, "main")
	r := w.step(bins.image, testgithub.Job{Run: run, Inputs: map[string]string{"name": "build"}}, nil).ok(t)
	if !strings.Contains(r.summary, "Outside the lanescape organization") || !strings.Contains(r.log, "does nothing here") {
		t.Fatalf("summary = %s\nlog:\n%s", r.summary, r.log)
	}
	r = w.step(bins.saveDisk, testgithub.Job{Run: run}, nil).ok(t)
	if !strings.Contains(r.summary, "Outside the lanescape organization") {
		t.Fatalf("summary = %s", r.summary)
	}
}

func TestAForksPullRequestIntoTheOrganization(t *testing.T) {
	w := newWorld(t)
	fork := w.gh.Fork(w.repo, w.contributor, w.gh.NewID())
	fork.Commit("feature", map[string]string{"x": "1"})
	pr := w.gh.PullRequest(w.repo, "main", fork, "feature", w.contributor)
	run := w.gh.PullRequestRun(pr)
	r := w.step(bins.image, testgithub.Job{Run: run, Pull: pr, Inputs: map[string]string{"name": "build"}}, nil).ok(t)
	if !strings.Contains(r.summary, "a fork's pull request") {
		t.Fatalf("summary = %s", r.summary)
	}
}

func TestAnApprovedContributionsImageJob(t *testing.T) {
	w := newWorld(t)
	fork := w.gh.Fork(w.repo, w.contributor, w.gh.NewID())
	fork.Commit("feature", map[string]string{"x": "1"})
	pr := w.gh.PullRequest(w.repo, "main", fork, "feature", w.contributor)
	run := w.gh.PullRequestTargetRun(pr)
	// Before a maintainer approves it, the run's plain job runs unbound,
	// and an image is refused.
	unbound := w.machine(run)
	w.step(bins.image, testgithub.Job{Run: run, Pull: pr, Inputs: map[string]string{"name": "build"}}, &unbound).
		failed(t, "isn't bound")
	// Once approved, the contribution job's check binds it, and the image
	// job, which needs it, has the image built.
	w.svc.Approve(w.svc.RunOf(run))
	w.machine(run)
	if s, ok := w.svc.Binding(w.svc.RunOf(run)); !ok || s != fmt.Sprintf("pr%d", pr.Number) {
		t.Fatalf("the contribution bound %q, %v", s, ok)
	}
	id := w.builds(run, "build")["amd64"]
	w.svc.FinishBuild(w.srepo, id, "")
	j := w.machine(run)
	w.step(bins.image, testgithub.Job{Run: run, Pull: pr, Inputs: map[string]string{"name": "build"}}, &j).ok(t)
}

func TestTheServiceRetriesUntilItCanAnswer(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	id := w.builds(run, "build")["amd64"]
	j := w.machine(run)
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/images", Times: 3, Respond: service.Unavailable(time.Second)})
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/images", Times: 1, Respond: fault.Status(http.StatusBadGateway, nil, "bad gateway")})
	go func() {
		time.Sleep(300 * time.Millisecond)
		w.svc.FinishBuild(w.srepo, id, "")
	}()
	r := w.step(bins.image, testgithub.Job{Run: run, Inputs: map[string]string{"name": "build"}}, &j).ok(t)
	if !strings.Contains(r.log, "trying again") {
		t.Fatalf("no retries were logged:\n%s", r.log)
	}
}

func TestTheActionFailsOnceItsRetriesAreSpent(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	j := w.machine(run)
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/images", Respond: service.Unavailable(time.Second)})
	w.step(bins.image, testgithub.Job{Run: run, Inputs: map[string]string{"name": "build"}}, &j).failed(t, "retries are spent")
}

func TestARefusalFailsWithItsReason(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	j := w.machine(run)
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/images", Respond: service.Refuse("no such thing")})
	w.step(bins.image, testgithub.Job{Run: run, Inputs: map[string]string{"name": "build"}}, &j).failed(t, "no such thing")
}

func TestSaveDiskAsksForASave(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	j := w.machine(run)
	r := w.step(bins.saveDisk, testgithub.Job{Run: run}, &j).ok(t)
	b, err := os.ReadFile(filepath.Join(r.dir, filepath.Base(machine.SaveMarker)))
	if err != nil || string(b) != machine.SaveRequest {
		t.Fatalf("save request = %q, %v", b, err)
	}
}

func TestCheckoutRefusesAContributionOutsidePullRequestTarget(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	r := w.step(bins.checkout, testgithub.Job{Run: run, Inputs: map[string]string{"contribution": "true"}}, nil, "decide")
	if r.err == nil || !strings.Contains(r.log, "only in a pull_request_target run") || len(r.outputs) != 0 {
		t.Fatalf("err = %v, outputs %v\n%s", r.err, r.outputs, r.log)
	}
}

func TestAFinishedBuildsWholeLogIsPrinted(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	id := w.builds(run, "build")["amd64"]
	// More than one answer's worth of log, all written as the build ends,
	// while the action follows it.
	go func() {
		time.Sleep(200 * time.Millisecond)
		w.svc.WriteLog(id, strings.Repeat("a line of output\n", 150_000)+"the last line\n")
		w.svc.FinishBuild(w.srepo, id, "")
	}()
	j := w.machine(run)
	r := w.step(bins.image, testgithub.Job{Run: run, Inputs: map[string]string{"name": "build"}}, &j).ok(t)
	// A line split between two answers is printed in its two halves.
	if n := strings.Count(r.log, "a line of output"); n < 149_999 || !strings.Contains(r.log, "the last line") {
		t.Fatalf("printed %d lines of the log, and its last: %v", n, strings.Contains(r.log, "the last line"))
	}
}

func TestAFailedArchitectureFailsBeforeAnotherIsFollowed(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	ids := w.builds(run, "multi")
	w.svc.FailBuild(w.srepo, ids["arm64"], "")
	followed := 0
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/images/builds/", Respond: func(rw http.ResponseWriter, r *http.Request) {
		followed++
		service.Unavailable(time.Second)(rw, r)
	}})
	j := w.machine(run)
	w.step(bins.image, testgithub.Job{Run: run, Inputs: map[string]string{"name": "multi"}}, &j).failed(t, "build for arm64 failed")
	if followed != 0 {
		t.Fatalf("followed amd64's build %d times before failing on arm64's", followed)
	}
}
