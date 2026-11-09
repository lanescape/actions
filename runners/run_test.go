package main

// These tests run the program in-process against the GitHub emulator,
// outside the organization, and up to the binding inside it. The
// end-to-end tests (e2e/) run the built program against the real service.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/actions/internal/actions"
	"github.com/lanescape/actions/internal/selector"
	"github.com/lanescape/actions/internal/service"
	"github.com/lanescape/actions/internal/testgithub"
)

const runnersJSON = `{
  "lanescape-80vcpu-linux-amd64": "ubuntu-24.04",
  "lanescape-2vcpu-linux-arm64": ["ubuntu-24.04-arm"]
}`

type world struct {
	gh          *testgithub.GitHub
	org         *ghemu.Account
	repo        *ghemu.Repo
	contributor *ghemu.Account
	fork        *ghemu.Repo
}

func newWorld(t *testing.T) *world {
	gh := testgithub.New(t)
	org := gh.Org(endpoints.Organization, "lanescape")
	repo := gh.Repo(org, 5001, "app")
	repo.Commit("main", map[string]string{".github/runners.json": runnersJSON, "deps/versions.json": "café 1.0\n"})
	contributor := gh.User(9001, "contributor")
	fork := gh.Fork(repo, contributor, 7001)
	return &world{gh: gh, org: org, repo: repo, contributor: contributor, fork: fork}
}

func noService(t *testing.T) newServiceFunc {
	return func(service.Tokens) *service.Client {
		t.Fatal("the service was called")
		return nil
	}
}

// runIt runs the program in env, returning its error, its map and its
// summary.
func runIt(t *testing.T, env map[string]string, svc newServiceFunc) (error, map[string]any, string) {
	t.Helper()
	var log bytes.Buffer
	rt := &actions.Runtime{Getenv: func(k string) string { return env[k] }, Stdout: &log}
	err := run(context.Background(), rt, svc)
	m := map[string]any{}
	if s := testgithub.Outputs(t, env)["map"]; s != "" {
		if jerr := json.Unmarshal([]byte(s), &m); jerr != nil {
			t.Fatalf("map %q isn't JSON", s)
		}
	}
	if strings.Contains(log.String(), env["INPUT_TOKEN"]) && !strings.Contains(log.String(), "::add-mask::"+env["INPUT_TOKEN"]) {
		t.Fatal("the job's token was logged")
	}
	return err, m, testgithub.Summary(t, env)
}

func asJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestAForksOwnPushFallsBack(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.fork, "main")
	env := w.gh.Env(testgithub.Job{Run: run, Inputs: map[string]string{
		"disks": `{"build": {"runner": "lanescape-80vcpu-linux-amd64", "spot": true, "from": ["deps/versions.json"]}}`,
	}})
	err, m, summary := runIt(t, env, noService(t))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"build":"ubuntu-24.04","lanescape-2vcpu-linux-arm64":["ubuntu-24.04-arm"],"lanescape-80vcpu-linux-amd64":"ubuntu-24.04"}`
	if asJSON(m) != want {
		t.Fatalf("map = %s, want %s", asJSON(m), want)
	}
	if !strings.Contains(summary, "Outside the lanescape organization") || !strings.Contains(summary, "| `build` | `ubuntu-24.04` |") {
		t.Fatalf("summary = %s", summary)
	}
}

func TestAForksRunnersVariableOverridesFallbacks(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.fork, "main")
	env := w.gh.Env(testgithub.Job{Run: run, Inputs: map[string]string{
		"disks":     `{"build": {"runner": "lanescape-80vcpu-linux-amd64"}}`,
		"fallbacks": `{"lanescape-80vcpu-linux-amd64": ["self-hosted", "big"], "lanescape-9vcpu-linux-amd64": "ignored"}`,
	}})
	err, m, _ := runIt(t, env, noService(t))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"build":["self-hosted","big"],"lanescape-2vcpu-linux-arm64":["ubuntu-24.04-arm"],"lanescape-80vcpu-linux-amd64":["self-hosted","big"]}`
	if asJSON(m) != want {
		t.Fatalf("map = %s, want %s", asJSON(m), want)
	}
}

func TestAForksPullRequestIgnoresTheOrganizationsVariable(t *testing.T) {
	w := newWorld(t)
	w.fork.Commit("change", map[string]string{"README.md": "changed"})
	pr := w.gh.PullRequest(w.repo, "main", w.fork, "change", w.contributor)
	run := w.gh.PullRequestRun(pr)
	env := w.gh.Env(testgithub.Job{Run: run, Pull: pr, IDToken: true, Inputs: map[string]string{
		"fallbacks": `{"lanescape-80vcpu-linux-amd64": "lanescape-org-runner"}`,
	}})
	err, m, summary := runIt(t, env, noService(t))
	if err != nil {
		t.Fatal(err)
	}
	if m["lanescape-80vcpu-linux-amd64"] != "ubuntu-24.04" {
		t.Fatalf("map = %s", asJSON(m))
	}
	if !strings.Contains(summary, "a fork's pull request") || !strings.Contains(summary, "`fallbacks` input is ignored") {
		t.Fatalf("summary = %s", summary)
	}
}

func TestAnotherOrganizationFallsBack(t *testing.T) {
	w := newWorld(t)
	other := w.gh.Org(4242, "lanescape-copy")
	repo := w.gh.Repo(other, 6001, "app")
	repo.Commit("main", map[string]string{".github/runners.json": runnersJSON})
	env := w.gh.Env(testgithub.Job{Run: w.gh.PushRun(repo, "main"), IDToken: true})
	if err, m, _ := runIt(t, env, noService(t)); err != nil || m["lanescape-80vcpu-linux-amd64"] != "ubuntu-24.04" {
		t.Fatalf("map = %s, %v", asJSON(m), err)
	}
}

func TestReadsTheFileAtTheRunsCommit(t *testing.T) {
	w := newWorld(t)
	w.fork.Commit("main", map[string]string{".github/runners.json": `{"lanescape-4vcpu-linux-amd64": "ubuntu-26.04"}`})
	run := w.gh.PushRun(w.fork, "main")
	// main moves on after the run's commit.
	w.fork.Commit("main", map[string]string{".github/runners.json": runnersJSON})
	err, m, _ := runIt(t, w.gh.Env(testgithub.Job{Run: run}), noService(t))
	if err != nil || asJSON(m) != `{"lanescape-4vcpu-linux-amd64":"ubuntu-26.04"}` {
		t.Fatalf("map = %s, %v", asJSON(m), err)
	}
}

func TestFailures(t *testing.T) {
	w := newWorld(t)
	for name, tc := range map[string]struct{ disks, files, want string }{
		"missing listed file": {`{"build": {"runner": "lanescape-80vcpu-linux-amd64", "from": ["deps/versions.json", "rust-toolchain.toml"]}}`, "",
			"rust-toolchain.toml isn't in contributor/app"},
		"unknown runner": {`{"build": {"runner": "lanescape-8vcpu-linux-amd64"}}`, "",
			`"lanescape-8vcpu-linux-amd64", isn't in .github/runners.json`},
		"family is a runner": {`{"lanescape-80vcpu-linux-amd64": {"runner": "lanescape-80vcpu-linux-amd64"}}`, "",
			"also a runner's name"},
		"no runners.json":  {``, ".github/runners.json", ".github/runners.json isn't in contributor/app"},
		"bad runners.json": {``, "{", "isn't a JSON object"},
	} {
		repo := w.fork
		branch := strings.ReplaceAll(name, " ", "-")
		if tc.files == ".github/runners.json" {
			repo.Commit(branch, map[string]string{".github/runners.json": ""})
		} else if tc.files != "" {
			repo.Commit(branch, map[string]string{".github/runners.json": tc.files})
		} else {
			repo.Commit(branch, map[string]string{"x": branch})
		}
		run := w.gh.EventRun(repo, "workflow_dispatch", branch)
		err, _, _ := runIt(t, w.gh.Env(testgithub.Job{Run: run, Inputs: map[string]string{"disks": tc.disks}}), noService(t))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one saying %q", name, err, tc.want)
		}
	}
}

func TestBadFallbacksFailOutside(t *testing.T) {
	w := newWorld(t)
	env := w.gh.Env(testgithub.Job{Run: w.gh.PushRun(w.fork, "main"), Inputs: map[string]string{"fallbacks": `["x"]`}})
	if err, _, _ := runIt(t, env, noService(t)); err == nil || !strings.Contains(err.Error(), "RUNNERS") {
		t.Fatalf("err = %v", err)
	}
}

func TestInsideNeedsTheIDTokenPermission(t *testing.T) {
	w := newWorld(t)
	env := w.gh.Env(testgithub.Job{Run: w.gh.PushRun(w.repo, "main")})
	if err, _, _ := runIt(t, env, noService(t)); err == nil || !strings.Contains(err.Error(), "id-token: write") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseDisks(t *testing.T) {
	ds, err := parseDisks(`{
		"build": {"runner": "lanescape-80vcpu-linux-amd64", "spot": true, "from": ["a", "b/c"]},
		"cache": {"runner": "lanescape-2vcpu-linux-amd64", "key": "0123456789abcdef0123"},
		"plain": {"runner": "lanescape-2vcpu-linux-amd64"}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 3 || ds[0].Family != "build" || !ds[0].Spot || len(ds[0].From) != 2 || ds[0].Key != "" ||
		ds[1].Key != "0123456789abcdef0123" || ds[2].Key != selector.NoKey || ds[2].Spot {
		t.Fatalf("disks = %+v", ds)
	}
	if ds, err := parseDisks(" "); err != nil || ds != nil {
		t.Fatalf("empty = %v, %v", ds, err)
	}
	for name, tc := range map[string]struct{ in, want string }{
		"not an object":  {`[]`, "isn't a JSON object"},
		"bad family":     {`{"Build": {"runner": "r"}}`, "isn't a family"},
		"long family":    {`{"a234567890123456789012345678901bc": {"runner": "r"}}`, "isn't a family"},
		"no runner":      {`{"build": {}}`, "names no runner"},
		"unknown field":  {`{"build": {"runner": "r", "Spot": true}}`, "unknown field"},
		"both":           {`{"build": {"runner": "r", "from": ["a"], "key": "0123456789abcdef0123"}}`, "both from and key"},
		"empty from":     {`{"build": {"runner": "r", "from": []}}`, "lists no files"},
		"absolute path":  {`{"build": {"runner": "r", "from": ["/etc/passwd"]}}`, "isn't a path"},
		"dot path":       {`{"build": {"runner": "r", "from": ["./a"]}}`, "isn't a path"},
		"parent path":    {`{"build": {"runner": "r", "from": ["../a"]}}`, "isn't a path"},
		"twice":          {`{"build": {"runner": "r", "from": ["a", "a"]}}`, "twice"},
		"upper key":      {`{"build": {"runner": "r", "key": "0123456789ABCDEF0123"}}`, "isn't 20 lowercase hex"},
		"short key":      {`{"build": {"runner": "r", "key": "0123"}}`, "isn't 20 lowercase hex"},
		"spot string":    {`{"build": {"runner": "r", "spot": "yes"}}`, "spot"},
		"family twice":   {`{"build": {"runner": "r"}, "build": {"runner": "r"}}`, "twice"},
		"null from":      {`{"build": {"runner": "r", "from": null}}`, "from is null"},
		"null key":       {`{"build": {"runner": "r", "key": null}}`, "key is null"},
		"null spot":      {`{"build": {"runner": "r", "spot": null}}`, "spot is null"},
		"null runner":    {`{"build": {"runner": null}}`, "runner is null"},
		"both, one null": {`{"build": {"runner": "r", "from": ["a"], "key": null}}`, "both from and key"},
		"null entry":     {`{"build": null}`, "isn't a JSON object"},
	} {
		if _, err := parseDisks(tc.in); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one saying %q", name, err, tc.want)
		}
	}
}
