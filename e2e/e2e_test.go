// Package e2e runs the actions' programs as GitHub would run them, with a
// job's environment and event, on one of the runner service's machines as
// Compute Engine gives it a metadata server, against lanescape/runners'
// emulator of the service, emulators/service, at the version go.mod
// names, and that repository's own GitHub emulator (specs/0002-releases.md,
// Tests). That repository's conformance suite holds the emulator to the
// real service's answers.
//
// There is one GitHub: each test makes its repository, branches, pull
// requests, runs, jobs and approvals in the GitHub emulator, which serves
// the API and event a job sees, and the service emulator reads the same
// state through service.WithGitHub. Its RepositoryOf, RunOf and JobOf give
// the service's handles for what a test made there.
//
// The programs are built with the e2e tag, which lets them call the
// emulator in place of endpoints.Service, use a test's metadata server and
// stand-in for /proc/self/mountinfo, and wait milliseconds; nothing else
// differs from what the actions build.
package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/emulators/service"
	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/actions/internal/testgithub"
)

const (
	small = "lanescape-2vcpu-linux-amd64"
	large = "lanescape-80vcpu-linux-amd64-spot"
)

// files are the repository's: two families of work disks, build, keyed by
// go.sum, and cache, with one key.
var files = map[string]string{
	".github/runners.json": `{"disks": {"build": {"from": ["go.sum"]}, "cache": {}}}`,
	"go.sum":               "example.com/a v1.0.0 h1:x\n",
}

// mounts are a machine's mounts, as /proc/self/mountinfo shows them, before
// its work disk is mounted, and the line its mount adds.
const (
	mounts = "22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n" +
		"25 22 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw\n"
	workMount = "40 22 8:16 / /work rw,relatime shared:20 - ext4 /dev/sdb rw\n"
)

// bins are the programs, built once.
var bins struct {
	dir                      string
	disk, saveDisk, checkout string
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
	for name, into := range map[string]*string{"disk": &bins.disk, "save-disk": &bins.saveDisk, "checkout": &bins.checkout} {
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
	contributor *ghemu.Account
	// elsewhere is the metadata server of a machine that isn't the
	// service's: it has no attribute lsr-service, and gives no tokens.
	elsewhere string

	mu sync.Mutex
	// requests are the requests that reached the service, by path.
	requests []string
}

// newWorld starts the GitHub emulator, with the organization by its real
// ID and a repository whose commit declares families of work disks, and
// the service emulator, which reads the same GitHub.
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
	elsewhere := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(elsewhere.Close)
	w := &world{t: t, gh: gh, svc: svc, org: org, repo: repo, contributor: gh.User(9001, "contributor"), elsewhere: elsewhere.URL}
	svc.Faults().Add(fault.Rule{PathPrefix: "/v1/", Match: func(r *http.Request) bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.requests = append(w.requests, r.URL.Path)
		return false
	}})
	return w
}

// asked is how many requests reached the service at path.
func (w *world) asked(path string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, p := range w.requests {
		if p == path {
			n++
		}
	}
	return n
}

// start starts a job of a run, naming a selector, on one of the service's
// machines, which binds the run as the service's check would.
func (w *world) start(run *ghemu.Run, selector string) *machine {
	w.t.Helper()
	j, err := w.svc.StartJob(w.svc.RunOf(run), selector)
	if err != nil {
		w.t.Fatalf("the job's check: %v", err)
	}
	return w.on(j)
}

// machine is one of the service's machines, running a job, with the
// mounts its job's steps see.
type machine struct {
	job       service.Job
	mountinfo string
}

func (w *world) on(j service.Job) *machine {
	w.t.Helper()
	m := &machine{job: j, mountinfo: filepath.Join(w.t.TempDir(), "mountinfo")}
	m.mount(w.t, false)
	return m
}

// mount shows the work disk mounted at /work, or not, as the machine
// mounts it on the host.
func (m *machine) mount(t *testing.T, mounted bool) {
	info := mounts
	if mounted {
		info += workMount
	}
	tmp := m.mountinfo + ".new"
	if err := os.WriteFile(tmp, []byte(info), 0o644); err != nil {
		t.Error(err)
		return
	}
	if err := os.Rename(tmp, m.mountinfo); err != nil {
		t.Error(err)
	}
}

// result is what a step left.
type result struct {
	err     error
	log     string
	outputs map[string]string
	summary string
}

// step runs a program as a step of a job, as GitHub would. On one of the
// service's machines, m, its metadata server answers the attribute
// lsr-service and mints the machine's identity token; without one, it's
// some other runner, whose metadata server has no such attribute.
func (w *world) step(bin string, j testgithub.Job, m *machine, args ...string) result {
	w.t.Helper()
	env := w.gh.Env(j)
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "LANESCAPE_TEST_SERVICE=" + w.svc.URL()}
	if m != nil {
		cmd.Env = append(cmd.Env, "LANESCAPE_TEST_METADATA="+w.svc.Metadata(m.job), "LANESCAPE_TEST_MOUNTINFO="+m.mountinfo)
	} else {
		info := filepath.Join(w.t.TempDir(), "mountinfo")
		if err := os.WriteFile(info, []byte(mounts), 0o644); err != nil {
			w.t.Fatal(err)
		}
		cmd.Env = append(cmd.Env, "LANESCAPE_TEST_METADATA="+w.elsewhere, "LANESCAPE_TEST_MOUNTINFO="+info)
	}
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
	// No token handed to the job appears in its log or summary, nor the
	// machine's identity token.
	secrets := w.gh.Secrets()
	if m != nil {
		secrets = append(secrets, w.svc.Token(m.job))
	}
	for _, s := range secrets {
		if strings.Contains(strings.ReplaceAll(r.log, "::add-mask::"+s, ""), s) || strings.Contains(r.summary, s) {
			w.t.Fatalf("a token is in the step's log or summary:\n%s\n%s", r.log, r.summary)
		}
	}
	if strings.Contains(r.log, "eyJ") || strings.Contains(r.summary, "eyJ") {
		w.t.Fatalf("something like a token is in the step's log or summary:\n%s\n%s", r.log, r.summary)
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

func (r result) failed(t *testing.T, says string) result {
	t.Helper()
	if r.err == nil || !strings.Contains(r.log, "::error::") || !strings.Contains(r.log, says) {
		t.Fatalf("err = %v; want a failure saying %q:\n%s", r.err, says, r.log)
	}
	return r
}

func (r result) says(t *testing.T, texts ...string) result {
	t.Helper()
	for _, s := range texts {
		if !strings.Contains(r.summary, s) {
			t.Fatalf("the summary doesn't say %q:\n%s\nlog:\n%s", s, r.summary, r.log)
		}
	}
	return r
}

func disk(run *ghemu.Run, family string) testgithub.Job {
	return testgithub.Job{Run: run, Inputs: map[string]string{"family": family}}
}

func TestAWorkDiskReadyAtOnce(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	m.mount(t, true)
	w.step(bins.disk, disk(run, "build"), m).ok(t).
		says(t, "Inside the lanescape organization", "family `build`, is mounted at `/work`", "(`none`)")
	if d, ok := w.svc.WorkDisk(m.job); !ok || d.Family != "build" || !d.Ready || d.Scope != "main" {
		t.Fatalf("the service's work disk = %+v, %v", d, ok)
	}
	if n := w.asked("/v1/disks"); n != 1 {
		t.Fatalf("asked %d times for a disk ready at once", n)
	}
}

func TestAWorkDiskMadeWhileTheActionAsks(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	// The disk is made once the program has asked three times, and the
	// machine mounts it a moment after.
	w.svc.HoldDisks(true)
	var asked atomic.Int32
	done := make(chan struct{})
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Match: func(*http.Request) bool {
		if asked.Add(1) == 3 {
			w.svc.HoldDisks(false)
			go func() {
				defer close(done)
				time.Sleep(50 * time.Millisecond)
				m.mount(t, true)
			}()
		}
		return false
	}})
	r := w.step(bins.disk, disk(run, "build"), m).ok(t).says(t, "is mounted at `/work`")
	<-done
	if !strings.Contains(r.log, "is making the work disk") || w.asked("/v1/disks") != 3 {
		t.Fatalf("asked %d times while the disk was made:\n%s", w.asked("/v1/disks"), r.log)
	}
}

func TestAWorkDiskNeverMounted(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	w.step(bins.disk, disk(run, "build"), m).failed(t, "isn't mounted at /work after")
}

// Where the disk came from, the answer's restored, is in the summary: here
// main's disk for the key, saved by an earlier run's job.
func TestWhereAWorkDiskCameFrom(t *testing.T) {
	w := newWorld(t)
	first := w.gh.PushRun(w.repo, "main")
	m := w.start(first, large)
	m.mount(t, true)
	w.step(bins.disk, disk(first, "build"), m).ok(t)
	w.step(bins.saveDisk, testgithub.Job{Run: first}, m).ok(t)
	w.svc.FinishJob(m.job, "success")
	if saved := w.svc.SavedDisks(w.svc.RepositoryOf(w.repo)); len(saved) != 1 || saved[0].Family != "build" {
		t.Fatalf("saved %+v", saved)
	}
	next := w.gh.PushRun(w.repo, "main")
	m = w.start(next, large)
	m.mount(t, true)
	w.step(bins.disk, disk(next, "build"), m).ok(t).says(t, "main's disk for this key (`main_exact`)")
}

func TestAFamilyTheCommitDoesntDeclare(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	m.mount(t, true)
	w.step(bins.disk, disk(run, "tests"), m).failed(t, "doesn't declare the family tests")
}

// A job has one work disk: a second family is refused, and the same one
// again succeeds at once.
func TestOneWorkDiskForAJob(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	m.mount(t, true)
	w.step(bins.disk, disk(run, "build"), m).ok(t)
	w.step(bins.disk, disk(run, "cache"), m).failed(t, "a job has one work disk")
	w.step(bins.disk, disk(run, "build"), m).ok(t).says(t, "family `build`, is mounted")
}

func TestAFamilyThatIsntAName(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	for _, f := range []string{"", "Build", "build-", "1build", strings.Repeat("a", 33), "a b"} {
		w.step(bins.disk, disk(run, f), m).failed(t, "isn't a family's name")
	}
	if n := w.asked("/v1/disks"); n != 0 {
		t.Fatalf("asked the service %d times", n)
	}
}

func TestSaveDiskInAScopeThatSaves(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	m.mount(t, true)
	w.step(bins.disk, disk(run, "build"), m).ok(t)
	w.step(bins.saveDisk, testgithub.Job{Run: run}, m).ok(t).says(t, "the run's scope saves, so the runner service saves it")
	if d, _ := w.svc.WorkDisk(m.job); !d.Save {
		t.Fatal("the service wasn't asked to save the disk")
	}
}

// A push to a branch the consumer doesn't list is in the read scope, which
// saves nothing; save-disk still succeeds, saying why.
func TestSaveDiskInAScopeThatSavesNothing(t *testing.T) {
	w := newWorld(t)
	w.repo.Commit("feature", map[string]string{"x": "1"})
	run := w.gh.PushRun(w.repo, "feature")
	m := w.start(run, large)
	m.mount(t, true)
	w.step(bins.disk, disk(run, "build"), m).ok(t)
	w.step(bins.saveDisk, testgithub.Job{Run: run}, m).ok(t).says(t, "saves nothing, so it won't be", "read")
	w.svc.FinishJob(m.job, "success")
	if saved := w.svc.SavedDisks(w.svc.RepositoryOf(w.repo)); len(saved) != 0 {
		t.Fatalf("saved %+v", saved)
	}
}

func TestSaveDiskWithNoWorkDisk(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	w.step(bins.saveDisk, testgithub.Job{Run: run}, m).failed(t, "no work disk")
}

func TestOnAMachineThatIsntTheServices(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	r := w.step(bins.disk, disk(run, "build"), nil).ok(t).says(t, "Inside the lanescape organization", "isn't one of the runner service's machines")
	if !strings.Contains(r.log, "does nothing here") {
		t.Fatalf("log:\n%s", r.log)
	}
	w.step(bins.saveDisk, testgithub.Job{Run: run}, nil).ok(t).says(t, "isn't one of the runner service's machines")
	if len(w.requests) != 0 {
		t.Fatalf("asked the service %v", w.requests)
	}
}

func TestAForksOwnRun(t *testing.T) {
	w := newWorld(t)
	fork := w.gh.Fork(w.repo, w.contributor, w.gh.NewID())
	run := w.gh.PushRun(fork, "main")
	w.step(bins.disk, disk(run, "build"), nil).ok(t).says(t, "Outside the lanescape organization", "disk did nothing")
	w.step(bins.saveDisk, testgithub.Job{Run: run}, nil).ok(t).says(t, "Outside the lanescape organization", "save-disk did nothing")
	if len(w.requests) != 0 {
		t.Fatalf("asked the service %v", w.requests)
	}
}

func TestAForksPullRequestIntoTheOrganization(t *testing.T) {
	w := newWorld(t)
	fork := w.gh.Fork(w.repo, w.contributor, w.gh.NewID())
	fork.Commit("feature", map[string]string{"x": "1"})
	pr := w.gh.PullRequest(w.repo, "main", fork, "feature", w.contributor)
	run := w.gh.PullRequestRun(pr)
	// The service gives such a run none of its machines.
	var refused *service.Refused
	if _, err := w.svc.StartJob(w.svc.RunOf(run), large); !errors.As(err, &refused) {
		t.Fatalf("the service gave a fork's pull request's job a machine: %v", err)
	}
	job := testgithub.Job{Run: run, Pull: pr, Inputs: map[string]string{"family": "build"}}
	w.step(bins.disk, job, nil).ok(t).says(t, "Outside the lanescape organization", "a fork's pull request")
	w.step(bins.saveDisk, testgithub.Job{Run: run, Pull: pr}, nil).ok(t).says(t, "a fork's pull request")
	if len(w.requests) != 0 {
		t.Fatalf("asked the service %v", w.requests)
	}
}

// An approved contribution's pull_request_target run is bound at its
// contribution job's check, once a maintainer approves it, and its build
// job, which needs that job, gets its work disk in the pull request's
// scope, and checks out the contribution's head.
func TestAnApprovedContributionsDiskJob(t *testing.T) {
	w := newWorld(t)
	fork := w.gh.Fork(w.repo, w.contributor, w.gh.NewID())
	fork.Commit("feature", map[string]string{"x": "1"})
	pr := w.gh.PullRequest(w.repo, "main", fork, "feature", w.contributor)
	run := w.gh.PullRequestTargetRun(pr)
	contribution := w.gh.Queue(run, ghemu.JobSpec{Name: "contribution", Labels: []string{small}, Environment: "contribution"})
	if _, err := w.svc.StartWorkflowJob(contribution); !errors.Is(err, service.ErrWaiting) {
		t.Fatalf("the contribution job before its approval: %v", err)
	}
	// Until then the run is unbound, and a job of it on the smallest
	// shape gets no work disk.
	unbound := w.start(run, small)
	job := testgithub.Job{Run: run, Pull: pr, Inputs: map[string]string{"family": "build"}}
	w.step(bins.disk, job, unbound).failed(t, "isn't bound")

	w.svc.Approve(w.svc.RunOf(run))
	if _, err := w.svc.StartWorkflowJob(contribution); err != nil {
		t.Fatalf("the approved contribution job's check: %v", err)
	}
	scope := fmt.Sprintf("pr%d", pr.Number)
	if s, ok := w.svc.Binding(w.svc.RunOf(run)); !ok || s != scope {
		t.Fatalf("the contribution bound %q, %v", s, ok)
	}
	m := w.start(run, large)
	m.mount(t, true)
	w.step(bins.disk, job, m).ok(t).says(t, "Inside the lanescape organization", "pull_request_target", "is mounted at `/work`")
	if d, _ := w.svc.WorkDisk(m.job); d.Scope != scope {
		t.Fatalf("the work disk is in %q", d.Scope)
	}
	r := w.step(bins.checkout, testgithub.Job{Run: run, Pull: pr, Inputs: map[string]string{"contribution": "true"}}, m, "decide").ok(t)
	if r.outputs["ref"] != run.EventPull.HeadSHA || r.outputs["persist-credentials"] != "false" {
		t.Fatalf("checkout outputs %v", r.outputs)
	}
	w.step(bins.saveDisk, testgithub.Job{Run: run, Pull: pr}, m).ok(t).says(t, "the run's scope saves")
}

func TestTheServiceRetriesUntilGitHubConfirms(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	m.mount(t, true)
	// GitHub confirms nothing until the program has asked twice.
	w.svc.Unavailable(true)
	var asked atomic.Int32
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Match: func(*http.Request) bool {
		if asked.Add(1) == 3 {
			w.svc.Unavailable(false)
		}
		return false
	}})
	r := w.step(bins.disk, disk(run, "build"), m).ok(t)
	if !strings.Contains(r.log, "trying again") || w.asked("/v1/disks") != 3 {
		t.Fatalf("no retries were logged:\n%s", r.log)
	}
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks/save", Times: 2, Respond: fault.Unavailable(time.Second)})
	if r := w.step(bins.saveDisk, testgithub.Job{Run: run}, m).ok(t); !strings.Contains(r.log, "trying again") {
		t.Fatalf("no retries were logged:\n%s", r.log)
	}
}

func TestTheActionsFailOnceTheirRetriesAreSpent(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	m.mount(t, true)
	w.svc.Unavailable(true)
	w.step(bins.disk, disk(run, "build"), m).failed(t, "retries are spent")
	if n := w.asked("/v1/disks"); n != 8 {
		t.Fatalf("asked %d times, not 8", n)
	}
	w.step(bins.saveDisk, testgithub.Job{Run: run}, m).failed(t, "retries are spent")
}

// A server in front of the service that's busy is retried too; any other
// answer outside the interface fails at once.
func TestAnswersFromInFront(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	m.mount(t, true)
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Times: 1, Respond: fault.Status(http.StatusBadGateway, nil, "<html>bad gateway</html>")})
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Times: 1, Respond: fault.Status(http.StatusTooManyRequests, nil, "")})
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Times: 1, Respond: fault.Drop})
	w.step(bins.disk, disk(run, "build"), m).ok(t)
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Times: 1,
		Respond: fault.Status(http.StatusFound, http.Header{"Location": {w.svc.URL() + "/v1/disks"}}, "")})
	before := w.asked("/v1/disks")
	w.step(bins.disk, disk(run, "build"), m).failed(t, "answered 302, which its interface doesn't have")
	if n := w.asked("/v1/disks") - before; n != 1 {
		t.Fatalf("asked %d times, following a redirect or retrying it", n)
	}
}

// A refusal fails the step with the service's reason, on one line, and
// nothing the service says can bring the machine's token into the log.
func TestARefusalFailsWithItsReason(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Respond: func(rw http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		escaped := `\u` + fmt.Sprintf("%04x", tok[0]) + tok[1:]
		fault.Status(http.StatusForbidden, nil, `{"reason": "no such thing\n::add-mask::x for `+escaped+`"}`)(rw, r)
	}})
	r := w.step(bins.disk, disk(run, "build"), m).failed(t, "no such thing ::add-mask::x for ***")
	if strings.Contains(r.log, "\n::add-mask::x") {
		t.Fatalf("the reason started a workflow command:\n%s", r.log)
	}
	w.step(bins.saveDisk, testgithub.Job{Run: run}, m).failed(t, "no such thing")
}

func TestCheckoutRefusesAContributionOutsidePullRequestTarget(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	r := w.step(bins.checkout, testgithub.Job{Run: run, Inputs: map[string]string{"contribution": "true"}}, nil, "decide")
	if r.err == nil || !strings.Contains(r.log, "only in a pull_request_target run") || len(r.outputs) != 0 {
		t.Fatalf("err = %v, outputs %v\n%s", r.err, r.outputs, r.log)
	}
}
