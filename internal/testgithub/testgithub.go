// Package testgithub is what the tests need of lanescape/runners' GitHub
// emulator beyond the emulator itself: the jobs' tokens, as GitHub gives a
// job its GITHUB_TOKEN and, with id-token: write, its OIDC tokens, which a
// test may have another of that repository's emulators mint, such as its
// service emulator for a job it knows. It's for tests only.
package testgithub

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	ghemu "github.com/lanescape/runners/emulators/github"
)

// JobsAppID is the ID of the App whose installation tokens stand in for
// jobs' GITHUB_TOKENs, as GitHub's own Actions App's do.
const JobsAppID = 15368

// GitHub is a GitHub emulator and the jobs' token issuers.
type GitHub struct {
	*ghemu.Emulator
	t    *testing.T
	jobs *ghemu.App

	mu        sync.Mutex
	installed map[int64]*ghemu.Installation
	requests  map[string]job

	// TokenURL is ACTIONS_ID_TOKEN_REQUEST_URL for every job.
	TokenURL string
	// Audiences are the audiences jobs asked tokens for, in order.
	Audiences []string
	// Minted are the OIDC and installation tokens handed to jobs, which
	// tests check no log shows.
	Minted []string
}

type job struct {
	run  *ghemu.Run
	job  ghemu.Job
	mint func(audience string) string
}

// New starts an emulator, closed when the test ends.
func New(t *testing.T) *GitHub {
	t.Helper()
	e := ghemu.New()
	t.Cleanup(e.Close)
	g := &GitHub{Emulator: e, t: t, jobs: e.NewApp(JobsAppID), installed: map[int64]*ghemu.Installation{}, requests: map[string]job{}}
	srv := httptest.NewServer(http.HandlerFunc(g.token))
	t.Cleanup(srv.Close)
	// GitHub's own URL has a query already; the toolkit adds audience.
	g.TokenURL = srv.URL + "/token?api-version=2.0"
	return g
}

// JobToken is a GITHUB_TOKEN for a job of a run in a repository of owner:
// an installation token, which reads that owner's repositories and public
// ones.
func (g *GitHub) JobToken(owner *ghemu.Account) string {
	g.t.Helper()
	g.mu.Lock()
	in := g.installed[owner.ID]
	if in == nil {
		in = g.Install(g.jobs, owner)
		g.installed[owner.ID] = in
	}
	g.mu.Unlock()
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/app/installations/%d/access_tokens", g.URL, in.ID), nil)
	req.Header.Set("Authorization", "Bearer "+appJWT(g.jobs))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Token == "" {
		g.t.Fatalf("no installation token: %d %v", resp.StatusCode, err)
	}
	g.mu.Lock()
	g.Minted = append(g.Minted, out.Token)
	g.mu.Unlock()
	return out.Token
}

// appJWT is an App's JWT, RS256, as GitHub asks.
func appJWT(a *ghemu.App) string {
	enc := base64.RawURLEncoding
	now := time.Now()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-30 * time.Second).Unix(), "exp": now.Add(5 * time.Minute).Unix(), "iss": strconv.FormatInt(a.ID, 10)})
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return signing + "." + enc.EncodeToString(sig)
}

// Secrets are every token handed to a job so far.
func (g *GitHub) Secrets() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.Minted...)
}

// RequestToken is ACTIONS_ID_TOKEN_REQUEST_TOKEN for a job of run: the
// tokens got with it are j's, with the audience asked for.
func (g *GitHub) RequestToken(run *ghemu.Run, j ghemu.Job) string {
	return g.requestToken(job{run: run, job: j})
}

// RequestTokenFrom is ACTIONS_ID_TOKEN_REQUEST_TOKEN for a job whose
// tokens mint makes, given the audience asked for, or GitHub's default,
// https://github.com/<owner>, if none was.
func (g *GitHub) RequestTokenFrom(run *ghemu.Run, mint func(audience string) string) string {
	return g.requestToken(job{run: run, mint: mint})
}

func (g *GitHub) requestToken(j job) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	tok := hex.EncodeToString(b[:])
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests[tok] = j
	g.Minted = append(g.Minted, tok)
	return tok
}

// token serves ACTIONS_ID_TOKEN_REQUEST_URL, as GitHub does.
func (g *GitHub) token(w http.ResponseWriter, r *http.Request) {
	h := r.Header.Get("Authorization")
	g.mu.Lock()
	j, ok := g.requests[stripBearer(h)]
	aud := r.URL.Query().Get("audience")
	if ok {
		g.Audiences = append(g.Audiences, aud)
	}
	g.mu.Unlock()
	if !ok || r.URL.Query().Get("api-version") != "2.0" {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	var tok string
	if j.mint != nil {
		if aud == "" {
			aud = "https://github.com/" + j.run.Repo.Owner.Login
		}
		tok = j.mint(aud)
	} else {
		jj := j.job
		if aud != "" {
			jj.Audience = aud
		}
		tok = g.ActionsToken(j.run, jj, nil)
	}
	g.mu.Lock()
	g.Minted = append(g.Minted, tok)
	g.mu.Unlock()
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(map[string]string{"value": tok})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(buf.Bytes())
}

func stripBearer(h string) string {
	if len(h) > 7 && (h[:7] == "Bearer " || h[:7] == "bearer ") {
		return h[7:]
	}
	return ""
}

// Job is a job of a run, as a step sees it.
type Job struct {
	Run *ghemu.Run
	// Pull is the event's pull request, for pull_request and
	// pull_request_target runs.
	Pull *ghemu.Pull
	// Claims is what tells the job's OIDC tokens from others in the run,
	// such as a bootstrap workflow's job_workflow_ref.
	Claims ghemu.Job
	// OIDC, if set, mints the job's OIDC tokens in place of the GitHub
	// emulator, given the audience asked for; Claims is then unused.
	OIDC func(audience string) string
	// IDToken is whether the job has id-token: write.
	IDToken bool
	// Inputs are the action's inputs, as INPUT_<NAME>. The token input is
	// the job's GITHUB_TOKEN unless set.
	Inputs map[string]string
}

// Env is the job's environment, as GitHub gives a step: its GITHUB_
// variables, its event's payload, output and summary files, and its token
// endpoint if it may get OIDC tokens.
func (g *GitHub) Env(j Job) map[string]string {
	g.t.Helper()
	dir := g.t.TempDir()
	run := j.Run
	payload := map[string]any{
		"repository": map[string]any{"id": run.Repo.ID, "full_name": run.Repo.FullName(), "owner": map[string]any{"id": run.Repo.Owner.ID, "login": run.Repo.Owner.Login}},
	}
	if p := j.Pull; p != nil {
		// The event is what it was when the run was made, as GitHub keeps
		// it, though the pull request has moved on since.
		number, sha := p.Number, p.HeadSHA
		if ep := run.EventPull; ep != nil {
			number, sha = ep.Number, ep.HeadSHA
		}
		var head any
		if p.Head != nil {
			head = map[string]any{"id": p.Head.ID, "full_name": p.Head.FullName()}
		}
		payload["number"] = number
		payload["pull_request"] = map[string]any{"number": number, "head": map[string]any{"sha": sha, "ref": p.HeadRef, "repo": head}}
	}
	b, _ := json.Marshal(payload)
	eventPath := filepath.Join(dir, "event.json")
	if err := os.WriteFile(eventPath, b, 0o644); err != nil {
		g.t.Fatal(err)
	}
	ws := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		g.t.Fatal(err)
	}
	env := map[string]string{
		"GITHUB_ACTIONS":             "true",
		"GITHUB_EVENT_NAME":          run.Event,
		"GITHUB_EVENT_PATH":          eventPath,
		"GITHUB_REPOSITORY":          run.Repo.FullName(),
		"GITHUB_REPOSITORY_ID":       strconv.FormatInt(run.Repo.ID, 10),
		"GITHUB_REPOSITORY_OWNER":    run.Repo.Owner.Login,
		"GITHUB_REPOSITORY_OWNER_ID": strconv.FormatInt(run.Repo.Owner.ID, 10),
		"GITHUB_SHA":                 run.SHA,
		"GITHUB_REF":                 run.Ref,
		"GITHUB_RUN_ID":              strconv.FormatInt(run.ID, 10),
		"GITHUB_API_URL":             g.URL,
		"GITHUB_OUTPUT":              filepath.Join(dir, "output"),
		"GITHUB_STEP_SUMMARY":        filepath.Join(dir, "summary"),
		"GITHUB_WORKSPACE":           ws,
		"RUNNER_TEMP":                dir,
		"INPUT_TOKEN":                g.JobToken(run.Repo.Owner),
	}
	if j.IDToken {
		env["ACTIONS_ID_TOKEN_REQUEST_URL"] = g.TokenURL
		if j.OIDC != nil {
			env["ACTIONS_ID_TOKEN_REQUEST_TOKEN"] = g.RequestTokenFrom(run, j.OIDC)
		} else {
			env["ACTIONS_ID_TOKEN_REQUEST_TOKEN"] = g.RequestToken(run, j.Claims)
		}
	}
	for k, v := range j.Inputs {
		env["INPUT_"+strings.ToUpper(strings.ReplaceAll(k, "-", "_"))] = v
	}
	return env
}

// Outputs reads a step's outputs from its output file.
func Outputs(t *testing.T, env map[string]string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(env["GITHUB_OUTPUT"])
	if os.IsNotExist(err) {
		return map[string]string{}
	}
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	lines := strings.Split(string(data), "\n")
	for i := 0; i < len(lines); i++ {
		name, delim, ok := strings.Cut(lines[i], "<<")
		if !ok {
			continue
		}
		var v []string
		for i++; i < len(lines) && lines[i] != delim; i++ {
			v = append(v, lines[i])
		}
		out[name] = strings.Join(v, "\n")
	}
	return out
}

// Summary reads a step's summary.
func Summary(t *testing.T, env map[string]string) string {
	t.Helper()
	b, _ := os.ReadFile(env["GITHUB_STEP_SUMMARY"])
	return string(b)
}
