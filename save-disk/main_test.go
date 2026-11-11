package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/emulators/service"
	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/actions/internal/actions"
	"github.com/lanescape/actions/internal/metadata"
	svcclient "github.com/lanescape/actions/internal/service"
	"github.com/lanescape/actions/internal/testgithub"
)

type on bool

func (o on) OnService(context.Context) bool { return bool(o) }

func TestSaveDisk(t *testing.T) {
	gh := testgithub.New(t)
	svc := service.New(service.WithGitHub(gh.Emulator))
	t.Cleanup(svc.Close)
	repo := gh.Repo(gh.Account(endpoints.Organization), gh.NewID(), "app")
	repo.Commit("main", map[string]string{".github/runners.json": `{"disks": {"build": {}}}`})
	repo.Commit("feature", map[string]string{"x": "1"})
	var asked atomic.Int32
	svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks/save", Match: func(*http.Request) bool { asked.Add(1); return false }})
	fork := gh.Fork(repo, gh.User(9001, "contributor"), gh.NewID())

	for _, tc := range []struct {
		name   string
		run    *ghemu.Run
		disk   bool
		on     bool
		says   string
		refuse string
		asks   int32
	}{
		{name: "a scope that saves", run: gh.PushRun(repo, "main"), disk: true, on: true, says: "the run's scope saves, so", asks: 1},
		{name: "the read scope", run: gh.PushRun(repo, "feature"), disk: true, on: true, says: "saves nothing, so it won't be: the run's scope, read", asks: 1},
		{name: "no work disk", run: gh.PushRun(repo, "main"), on: true, refuse: "refused to save the work disk: this job has no work disk", asks: 1},
		{name: "not the service's machine", run: gh.PushRun(repo, "main"), says: "isn't one of the runner service's machines"},
		{name: "a fork's own run", run: gh.PushRun(fork, "main"), on: true, says: "Outside the lanescape organization"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked.Store(0)
			c := svcclient.At(svc.URL(), nil)
			if tc.run.Repo == repo {
				j, err := svc.StartJob(svc.RunOf(tc.run), "lanescape-2vcpu-linux-amd64")
				if err != nil {
					t.Fatal(err)
				}
				c = svcclient.At(svc.URL(), metadata.Server{URL: svc.Metadata(j)})
				if tc.disk {
					if _, err := c.Disk(context.Background(), "build"); err != nil {
						t.Fatal(err)
					}
				}
			}
			env := gh.Env(testgithub.Job{Run: tc.run})
			rt := &actions.Runtime{Getenv: func(k string) string { return env[k] }, Stdout: &bytes.Buffer{}}
			err := run(context.Background(), rt, on(tc.on), c)
			if tc.refuse != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refuse) {
					t.Fatalf("err = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(env["GITHUB_STEP_SUMMARY"])
			if !strings.Contains(string(b), tc.says) {
				t.Fatalf("summary = %s", b)
			}
			if asked.Load() != tc.asks {
				t.Fatalf("asked %d times", asked.Load())
			}
		})
	}
}
