package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/emulators/service"
	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/actions/internal/actions"
	"github.com/lanescape/actions/internal/backoff"
	"github.com/lanescape/actions/internal/metadata"
	svcclient "github.com/lanescape/actions/internal/service"
	"github.com/lanescape/actions/internal/testgithub"
)

// on is whether the machine is the service's, as its metadata server would
// say.
type on bool

func (o on) OnService(context.Context) bool { return bool(o) }

type world struct {
	gh   *testgithub.GitHub
	svc  *service.Emulator
	repo *ghemu.Repo
	// asked counts the requests for a disk.
	asked atomic.Int32
}

func newWorld(t *testing.T) *world {
	gh := testgithub.New(t)
	svc := service.New(service.WithGitHub(gh.Emulator))
	t.Cleanup(svc.Close)
	repo := gh.Repo(gh.Account(endpoints.Organization), gh.NewID(), "app")
	repo.Commit("main", map[string]string{".github/runners.json": `{"disks": {"build": {}}}`})
	w := &world{gh: gh, svc: svc, repo: repo}
	svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Match: func(*http.Request) bool { w.asked.Add(1); return false }})
	return w
}

// job is a step of a job of run on one of the service's machines, with
// its client, and mounts that show /work mounted or not.
func (w *world) job(t *testing.T, run *ghemu.Run, family string, mounted bool) (*actions.Runtime, *svcclient.Client, timing, *bytes.Buffer) {
	t.Helper()
	j, err := w.svc.StartJob(w.svc.RunOf(run), "lanescape-2vcpu-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	env := w.gh.Env(testgithub.Job{Run: run, Inputs: map[string]string{"family": family}})
	var log bytes.Buffer
	rt := &actions.Runtime{Getenv: func(k string) string { return env[k] }, Stdout: &log}
	c := svcclient.At(w.svc.URL(), metadata.Server{URL: w.svc.Metadata(j)})
	c.Retry = backoff.Policy{Attempts: 3, First: time.Millisecond, Max: time.Millisecond}
	info := filepath.Join(t.TempDir(), "mountinfo")
	lines := "22 1 8:1 / / rw - ext4 /dev/sda1 rw\n"
	if mounted {
		lines += "40 22 8:16 / /work rw - ext4 /dev/sdb rw\n"
	}
	if err := os.WriteFile(info, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	return rt, c, timing{making: 10 * time.Millisecond, mountWait: 100 * time.Millisecond, mountEvery: 5 * time.Millisecond, mountinfo: info}, &log
}

func summary(rt *actions.Runtime) string {
	b, _ := os.ReadFile(rt.Getenv("GITHUB_STEP_SUMMARY"))
	return string(b)
}

func TestReady(t *testing.T) {
	w := newWorld(t)
	rt, c, tm, _ := w.job(t, w.gh.PushRun(w.repo, "main"), "build", true)
	if err := run(context.Background(), rt, on(true), c, tm); err != nil {
		t.Fatal(err)
	}
	if s := summary(rt); !strings.Contains(s, "family `build`, is mounted at `/work`") || !strings.Contains(s, "(`none`)") {
		t.Fatalf("summary = %s", s)
	}
}

// While the disk is being made, the program asks again, every tm.making.
func TestMaking(t *testing.T) {
	w := newWorld(t)
	rt, c, tm, log := w.job(t, w.gh.PushRun(w.repo, "main"), "build", true)
	w.svc.HoldDisks(true)
	time.AfterFunc(100*time.Millisecond, func() { w.svc.HoldDisks(false) })
	start := time.Now()
	if err := run(context.Background(), rt, on(true), c, tm); err != nil {
		t.Fatal(err)
	}
	n := int(w.asked.Load())
	if elapsed := time.Since(start); n < 3 || time.Duration(n-1)*tm.making > elapsed {
		t.Fatalf("asked %d times in %s, every %s", n, elapsed, tm.making)
	}
	if strings.Count(log.String(), "is making the work disk") != 1 {
		t.Fatalf("log:\n%s", log)
	}
}

func TestNeverMounted(t *testing.T) {
	w := newWorld(t)
	rt, c, tm, _ := w.job(t, w.gh.PushRun(w.repo, "main"), "build", false)
	start := time.Now()
	err := run(context.Background(), rt, on(true), c, tm)
	if err == nil || !strings.Contains(err.Error(), "isn't mounted at /work after 100ms") || time.Since(start) < tm.mountWait {
		t.Fatalf("err = %v after %s", err, time.Since(start))
	}
}

func TestNotTheServicesMachine(t *testing.T) {
	w := newWorld(t)
	rt, c, tm, _ := w.job(t, w.gh.PushRun(w.repo, "main"), "build", false)
	if err := run(context.Background(), rt, on(false), c, tm); err != nil {
		t.Fatal(err)
	}
	if s := summary(rt); !strings.Contains(s, "isn't one of the runner service's machines") || w.asked.Load() != 0 {
		t.Fatalf("summary = %s, asked %d", s, w.asked.Load())
	}
}

func TestOutside(t *testing.T) {
	w := newWorld(t)
	fork := w.gh.Fork(w.repo, w.gh.User(9001, "contributor"), w.gh.NewID())
	rt, c, tm, _ := w.job(t, w.gh.PushRun(w.repo, "main"), "build", false)
	env := w.gh.Env(testgithub.Job{Run: w.gh.PushRun(fork, "main"), Inputs: map[string]string{"family": "build"}})
	rt.Getenv = func(k string) string { return env[k] }
	if err := run(context.Background(), rt, on(true), c, tm); err != nil {
		t.Fatal(err)
	}
	if s := summary(rt); !strings.Contains(s, "Outside the lanescape organization") || w.asked.Load() != 0 {
		t.Fatalf("summary = %s, asked %d", s, w.asked.Load())
	}
}

func TestRefused(t *testing.T) {
	w := newWorld(t)
	rt, c, tm, _ := w.job(t, w.gh.PushRun(w.repo, "main"), "other", true)
	if err := run(context.Background(), rt, on(true), c, tm); err == nil ||
		!strings.Contains(err.Error(), "refused the work disk of the family other") || !strings.Contains(err.Error(), "doesn't declare") {
		t.Fatalf("err = %v", err)
	}
}

func TestFamilyNames(t *testing.T) {
	for f, ok := range map[string]bool{"a": true, "build": true, "go-1": true, strings.Repeat("a", 32): true,
		"": false, "1a": false, "a-": false, "-a": false, "A": false, "a_b": false, strings.Repeat("a", 33): false} {
		if family.MatchString(f) != ok {
			t.Errorf("%q: %v", f, !ok)
		}
	}
}
