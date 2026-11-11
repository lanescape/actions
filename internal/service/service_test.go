package service

// These tests check how the client reads each answer the interface has,
// and some it doesn't, from lanescape/runners' emulator of the service,
// at the version go.mod names, with emulators/fault giving the answers it
// doesn't give on its own.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lanescape/runners/conformance"
	"github.com/lanescape/runners/emulators/fault"
	"github.com/lanescape/runners/emulators/service"
	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/actions/internal/backoff"
	"github.com/lanescape/actions/internal/metadata"
)

var files = map[string]string{
	".github/runners.json": `{"disks": {"build": {"from": ["go.sum"]}, "other": {}}}`,
	"go.sum":               "a v1.0.0 h1:x\n",
}

// env is a job on one of the emulator's machines, and a client that signs
// its requests with the machine's identity token.
type env struct {
	svc   *service.Emulator
	repo  service.Repository
	job   service.Job
	c     *Client
	waits *[]time.Duration
	// tokens counts the identity tokens the client got.
	tokens *counting
}

type counting struct {
	metadata.Server
	mu sync.Mutex
	n  int
	// last is the last token, to look for in errors.
	last string
}

func (c *counting) Token(ctx context.Context, aud string) (string, error) {
	t, err := c.Server.Token(ctx, aud)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	c.last = t
	return t, err
}

func setup(t *testing.T) *env {
	t.Helper()
	svc := service.New()
	t.Cleanup(svc.Close)
	repo := svc.Repository("app")
	svc.Commit(repo, "main", files)
	j, err := svc.StartJob(svc.Push(repo, "main"), "lanescape-2vcpu-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	tk := &counting{Server: metadata.Server{URL: svc.Metadata(j)}}
	var waits []time.Duration
	c := At(svc.URL(), tk)
	c.Retry = backoff.Policy{Attempts: 4, First: 2 * time.Second, Max: time.Minute, Sleep: func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}}
	return &env{svc: svc, repo: repo, job: j, c: c, waits: &waits, tokens: tk}
}

// count counts the requests that reach the service past the faults added
// before it: with a fault for every request to /v1/, the identity tokens
// got, one for each attempt.
func (e *env) count() *int {
	n := new(int)
	var mu sync.Mutex
	e.svc.Faults().Add(fault.Rule{Match: func(*http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		*n++
		return false
	}})
	return n
}

func TestDisk(t *testing.T) {
	e := setup(t)
	var r *Refused
	if _, err := e.c.Disk(context.Background(), "undeclared"); !errors.As(err, &r) || !strings.Contains(r.Reason, "doesn't declare") {
		t.Fatalf("an undeclared family: %v", err)
	}
	e.svc.HoldDisks(true)
	d, err := e.c.Disk(context.Background(), "build")
	if err != nil || d.State != Making || d.Restored != "none" {
		t.Fatalf("disk = %+v, %v", d, err)
	}
	e.svc.HoldDisks(false)
	d, err = e.c.Disk(context.Background(), "build")
	if err != nil || d.State != Ready || d.Restored != "none" {
		t.Fatalf("disk = %+v, %v", d, err)
	}
	if wd, ok := e.svc.WorkDisk(e.job); !ok || wd.Family != "build" || !wd.Ready {
		t.Fatalf("the emulator's work disk = %+v, %v", wd, ok)
	}
	if _, err := e.c.Disk(context.Background(), "other"); !errors.As(err, &r) || !strings.Contains(r.Reason, "one work disk") {
		t.Fatalf("a second family: %v", err)
	}
}

// Where a disk may come from is what the interface says, as
// lanescape/runners' conformance suite holds the service to.
func TestSteps(t *testing.T) {
	if len(Steps) != len(conformance.Steps) {
		t.Fatalf("Steps = %v; the interface has %v", Steps, conformance.Steps)
	}
	for s := range conformance.Steps {
		if _, ok := Steps[s]; !ok {
			t.Errorf("no step %s", s)
		}
	}
}

func TestSave(t *testing.T) {
	e := setup(t)
	var r *Refused
	if _, err := e.c.Save(context.Background()); !errors.As(err, &r) || !strings.Contains(r.Reason, "no work disk") {
		t.Fatalf("a save with no work disk: %v", err)
	}
	if _, err := e.c.Disk(context.Background(), "build"); err != nil {
		t.Fatal(err)
	}
	if s, err := e.c.Save(context.Background()); err != nil || !s.Saves || s.Reason != "" {
		t.Fatalf("save = %+v, %v", s, err)
	}
	if wd, _ := e.svc.WorkDisk(e.job); !wd.Save {
		t.Fatal("the emulator wasn't asked to save the disk")
	}
	// A push to a branch the consumer doesn't list is in the read scope,
	// which saves nothing.
	j, err := e.svc.StartJob(e.svc.Push(e.repo, "feature"), "lanescape-2vcpu-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	c := At(e.svc.URL(), metadata.Server{URL: e.svc.Metadata(j)})
	if _, err := c.Disk(context.Background(), "build"); err != nil {
		t.Fatal(err)
	}
	if s, err := c.Save(context.Background()); err != nil || s.Saves || !strings.Contains(s.Reason, "read") {
		t.Fatalf("save in read = %+v, %v", s, err)
	}
}

func TestEveryRequestGetsAFreshToken(t *testing.T) {
	e := setup(t)
	e.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Times: 1, Respond: fault.Unavailable(time.Second)})
	if _, err := e.c.Disk(context.Background(), "build"); err != nil {
		t.Fatal(err)
	}
	if e.tokens.n != 2 {
		t.Fatalf("%d tokens for 2 requests", e.tokens.n)
	}
}

func TestRetries(t *testing.T) {
	for name, tc := range map[string]struct {
		faults []fault.Rule
		waits  []time.Duration
	}{
		"503s, waiting as asked or doubling, and a busy proxy's 502": {[]fault.Rule{
			{PathPrefix: "/v1/disks", Times: 1, Respond: fault.Unavailable(5 * time.Second)},
			{PathPrefix: "/v1/disks", Times: 1, Respond: fault.Unavailable(time.Second)},
			{PathPrefix: "/v1/disks", Times: 1, Respond: fault.Status(http.StatusBadGateway, nil, "bad gateway")},
		}, []time.Duration{5 * time.Second, 4 * time.Second, 8 * time.Second}},
		"a Retry-After over a minute waits a minute": {[]fault.Rule{
			{PathPrefix: "/v1/disks", Times: 1, Respond: fault.Unavailable(10 * time.Minute)},
		}, []time.Duration{time.Minute}},
		"a 503 from a server in front, without retryable": {[]fault.Rule{
			{PathPrefix: "/v1/disks", Times: 1, Respond: fault.Status(http.StatusServiceUnavailable, nil, "<html>busy</html>")},
		}, []time.Duration{2 * time.Second}},
		"a busy proxy's 502 larger than any answer": {[]fault.Rule{
			{PathPrefix: "/v1/disks", Times: 1, Respond: fault.Status(http.StatusBadGateway, nil, strings.Repeat("x", maxAnswer+1))},
		}, []time.Duration{2 * time.Second}},
		"429": {[]fault.Rule{
			{PathPrefix: "/v1/disks", Times: 1, Respond: fault.Status(http.StatusTooManyRequests, http.Header{"Retry-After": {"2"}}, "")},
		}, []time.Duration{2 * time.Second}},
		"a dropped connection": {[]fault.Rule{
			{PathPrefix: "/v1/disks", Times: 2, Respond: fault.Drop},
		}, []time.Duration{2 * time.Second}},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			for _, f := range tc.faults {
				e.svc.Faults().Add(f)
			}
			if _, err := e.c.Disk(context.Background(), "build"); err != nil {
				t.Fatal(err)
			}
			got := *e.waits
			if name == "a dropped connection" {
				// Go's transport may retry a dropped connection itself once.
				if len(got) < 1 {
					t.Fatalf("waits = %v", got)
				}
				return
			}
			if len(got) != len(tc.waits) {
				t.Fatalf("waits = %v, want %v", got, tc.waits)
			}
			for i := range got {
				if got[i] != tc.waits[i] {
					t.Fatalf("waits = %v, want %v", got, tc.waits)
				}
			}
		})
	}
}

func TestEightAttemptsInAll(t *testing.T) {
	if DefaultRetry.Attempts != 8 || DefaultRetry.First != 2*time.Second || DefaultRetry.Max != time.Minute {
		t.Fatalf("DefaultRetry = %+v; the spec gives eight attempts, from 2 seconds, doubling to a minute", DefaultRetry)
	}
	e := setup(t)
	e.c.Retry.Attempts = DefaultRetry.Attempts
	e.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Respond: fault.Unavailable(time.Second)})
	n := e.count()
	_, err := e.c.Disk(context.Background(), "build")
	if !errors.Is(err, ErrUnavailable) || *n != 8 {
		t.Fatalf("err = %v after %d requests, want 8", err, *n)
	}
}

func TestTheServiceUnreachable(t *testing.T) {
	e := setup(t)
	e.c.url = "http://127.0.0.1:1"
	_, err := e.c.Disk(context.Background(), "build")
	if !errors.Is(err, ErrUnavailable) || len(*e.waits) != 3 {
		t.Fatalf("err = %v after waits %v", err, *e.waits)
	}
}

func TestFinalAnswers(t *testing.T) {
	for name, f := range map[string]func(http.ResponseWriter, *http.Request){
		"a refusal":                      fault.Refuse("the run's commit doesn't declare it"),
		"a status it doesn't have":       fault.Status(http.StatusTeapot, nil, `{}`),
		"a redirect":                     fault.Status(http.StatusTemporaryRedirect, http.Header{"Location": {"/v1/disks/other"}}, ""),
		"a 200 with no state":            fault.Status(http.StatusOK, nil, `{"restored": "none"}`),
		"a 200 with another state":       fault.Status(http.StatusOK, nil, `{"state": "pending", "restored": "none"}`),
		"a 200 from another step":        fault.Status(http.StatusOK, nil, `{"state": "ready", "restored": "elsewhere"}`),
		"a 200 from no step":             fault.Status(http.StatusOK, nil, `{"state": "ready"}`),
		"an answer larger than any":      fault.Status(http.StatusOK, nil, `{"state": "ready", "restored": "none", "x": "`+strings.Repeat("x", maxAnswer)+`"}`),
		"a 200 that isn't the interface": fault.Status(http.StatusOK, nil, `not json`),
		"a 401":                          fault.Status(http.StatusUnauthorized, nil, `{"error": "Unauthorized"}`),
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Respond: f})
			n := e.count()
			_, err := e.c.Disk(context.Background(), "build")
			if err == nil {
				t.Fatal("no error")
			}
			if *n != 1 {
				t.Fatalf("retried a final answer, %d requests: %v", *n, err)
			}
			var r *Refused
			if name == "a refusal" && (!errors.As(err, &r) || r.Reason != "the run's commit doesn't declare it") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// An answer that echoes the token never brings it into an error.
func TestTheTokenIsNeverInAnError(t *testing.T) {
	for name, echo := range map[string]func(tok string) func(http.ResponseWriter, *http.Request){
		"a refusal's reason": func(tok string) func(http.ResponseWriter, *http.Request) {
			return func(w http.ResponseWriter, r *http.Request) {
				fault.Refuse("you sent "+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))(w, r)
			}
		},
		"a refusal's reason, escaped, beside a number too large for a float": func(tok string) func(http.ResponseWriter, *http.Request) {
			return func(w http.ResponseWriter, r *http.Request) {
				t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				escaped := `\u` + fmt.Sprintf("%04x", t[0]) + t[1:]
				fault.Status(http.StatusForbidden, nil, `{"unused": 1e1000, "reason": "you sent `+escaped+`"}`)(w, r)
			}
		},
		"a refusal's reason, escaped, with something after the answer": func(tok string) func(http.ResponseWriter, *http.Request) {
			return func(w http.ResponseWriter, r *http.Request) {
				t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				escaped := `\u` + fmt.Sprintf("%04x", t[0]) + t[1:]
				fault.Status(http.StatusForbidden, nil, `{"reason": "you sent `+escaped+`"} x`)(w, r)
			}
		},
		"a state, escaped, beside a number too large for a float": func(tok string) func(http.ResponseWriter, *http.Request) {
			return func(w http.ResponseWriter, r *http.Request) {
				t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				escaped := `\u` + fmt.Sprintf("%04x", t[0]) + t[1:]
				fault.Status(http.StatusOK, nil, `{"unused": -1e1000, "state": "`+escaped+`", "restored": "none"}`)(w, r)
			}
		},
		"a refusal's reason, escaped": func(tok string) func(http.ResponseWriter, *http.Request) {
			return func(w http.ResponseWriter, r *http.Request) {
				t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				escaped := `\u` + fmt.Sprintf("%04x", t[0]) + t[1:]
				fault.Status(http.StatusForbidden, nil, `{"error": "Forbidden", "reason": "you sent `+escaped+`"}`)(w, r)
			}
		},
		"a state it doesn't have": func(string) func(http.ResponseWriter, *http.Request) {
			return func(w http.ResponseWriter, r *http.Request) {
				tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				fault.Status(http.StatusOK, nil, `{"state": "`+tok+`", "restored": "none"}`)(w, r)
			}
		},
		"a step it doesn't have, escaped": func(string) func(http.ResponseWriter, *http.Request) {
			return func(w http.ResponseWriter, r *http.Request) {
				t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				escaped := `\u` + fmt.Sprintf("%04x", t[0]) + t[1:]
				fault.Status(http.StatusOK, nil, `{"state": "ready", "restored": "`+escaped+`"}`)(w, r)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Respond: echo("")})
			_, err := e.c.Disk(context.Background(), "build")
			if err == nil || e.tokens.last == "" || strings.Contains(err.Error(), e.tokens.last) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestTheAudienceIsTheService(t *testing.T) {
	if New(nil).URL() != endpoints.Service {
		t.Fatal("the service's address isn't endpoints.Service")
	}
}

// The actions' own builds ask again every 5 seconds while a disk is being
// made, wait up to 2 minutes for it to be mounted, as their own process
// sees its mounts, and ask Compute Engine's metadata server.
func TestTheActionsOwnTimings(t *testing.T) {
	if MakingEvery() != 5*time.Second || MountWait() != 2*time.Minute || MountEvery() > 5*time.Second ||
		MountInfo() != "/proc/self/mountinfo" || MetadataServer() != "http://metadata.google.internal" {
		t.Fatalf("the actions' own builds ask every %s, wait %s for %s, looking every %s, and ask %s",
			MakingEvery(), MountWait(), MountInfo(), MountEvery(), MetadataServer())
	}
}

// A reason the service gives reaches the log on one line, so it can't
// start a workflow command.
func TestAReasonIsOneLine(t *testing.T) {
	e := setup(t)
	e.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Respond: fault.Refuse("no\n::add-mask::x\r\n::error::y\tz")})
	_, err := e.c.Disk(context.Background(), "build")
	var r *Refused
	if !errors.As(err, &r) || r.Reason != "no ::add-mask::x ::error::y z" {
		t.Fatalf("err = %v", err)
	}
	e.svc.Faults().Reset()
	e.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks/save", Respond: fault.Status(http.StatusOK, nil, `{"saves": false, "reason": "a\nb"}`)})
	if s, err := e.c.Save(context.Background()); err != nil || s.Reason != "a b" {
		t.Fatalf("save = %+v, %v", s, err)
	}
}

func TestSaveAnswersOutsideTheInterface(t *testing.T) {
	for _, body := range []string{`{}`, `{"saves": true, "reason": "why"}`, `{"saves": false}`, `{"saves": "yes"}`} {
		e := setup(t)
		e.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks/save", Respond: fault.Status(http.StatusOK, nil, body)})
		n := e.count()
		if _, err := e.c.Save(context.Background()); err == nil || *n != 1 {
			t.Fatalf("%s: err = %v after %d requests", body, err, *n)
		}
	}
}

// Every request sends the identity token as a bearer token, and a save
// sends exactly what the interface asks.
func TestRequests(t *testing.T) {
	e := setup(t)
	var got []string
	var mu sync.Mutex
	e.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/", Match: func(r *http.Request) bool {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		mu.Lock()
		defer mu.Unlock()
		auth, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		got = append(got, fmt.Sprintf("%s %s %s %s %v", r.Method, r.URL.Path, r.Header.Get("Content-Type"), b, audience(auth)))
		return false
	}})
	if _, err := e.c.Disk(context.Background(), "build"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	aud := e.svc.URL() + " full"
	want := []string{`POST /v1/disks application/json {"family":"build"} ` + aud, `POST /v1/disks/save application/json {"save":true} ` + aud}
	if !slices.Equal(got, want) {
		t.Fatalf("requests = %q, want %q", got, want)
	}
}

// audience is a machine identity token's audience, and whether it's in the
// full format, which names the machine.
func audience(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "not a token"
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "not a token"
	}
	var c struct {
		Aud    string `json:"aud"`
		Google struct {
			ComputeEngine struct {
				InstanceName string `json:"instance_name"`
			} `json:"compute_engine"`
		} `json:"google"`
	}
	if json.Unmarshal(b, &c) != nil {
		return "not a token"
	}
	format := "standard"
	if c.Google.ComputeEngine.InstanceName != "" {
		format = "full"
	}
	return c.Aud + " " + format
}
