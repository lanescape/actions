package service

// These tests check how the client reads each answer the interface has,
// and some it doesn't, from lanescape/runners' emulator of the service,
// at the version go.mod names, with emulators/fault giving the answers it
// doesn't give on its own.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/fault"
	"github.com/lanescape/runners/emulators/service"
	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/actions/internal/backoff"
	"github.com/lanescape/actions/internal/metadata"
)

var image = map[string]string{
	".github/runners.json":            `{"images": {"build": ".github/images/build"}}`,
	".github/images/build/image.json": `{"architectures": ["amd64"]}`,
	".github/images/build/init.sh":    "apt-get install -y gcc\n",
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
	svc.Commit(repo, "main", image)
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

// count counts the requests that reach the service.
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

func TestImage(t *testing.T) {
	e := setup(t)
	bs, err := e.c.Image(context.Background(), "build")
	if err != nil || len(bs) != 1 || bs[0].State != Building || bs[0].Build == "" || bs[0].Architecture != "amd64" {
		t.Fatalf("builds = %+v, %v", bs, err)
	}
	e.svc.FinishBuild(e.repo, bs[0].Build, "built\n")
	p, err := e.c.Follow(context.Background(), bs[0].Build, 0)
	if err != nil || p.State != Ready || p.Log != "built\n" || p.Next != 6 {
		t.Fatalf("progress = %+v, %v", p, err)
	}
	if p, err := e.c.Follow(context.Background(), bs[0].Build, p.Next); err != nil || p.Log != "" || p.Next != 6 {
		t.Fatalf("from the log's end: %+v, %v", p, err)
	}
}

func TestEveryRequestGetsAFreshToken(t *testing.T) {
	e := setup(t)
	e.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/images", Times: 1, Respond: service.Unavailable(time.Second)})
	if _, err := e.c.Image(context.Background(), "build"); err != nil {
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
			{PathPrefix: "/v1/images", Times: 1, Respond: service.Unavailable(5 * time.Second)},
			{PathPrefix: "/v1/images", Times: 1, Respond: service.Unavailable(time.Second)},
			{PathPrefix: "/v1/images", Times: 1, Respond: fault.Status(http.StatusBadGateway, nil, "bad gateway")},
		}, []time.Duration{5 * time.Second, 4 * time.Second, 8 * time.Second}},
		"a Retry-After over a minute waits a minute": {[]fault.Rule{
			{PathPrefix: "/v1/images", Times: 1, Respond: service.Unavailable(10 * time.Minute)},
		}, []time.Duration{time.Minute}},
		"429": {[]fault.Rule{
			{PathPrefix: "/v1/images", Times: 1, Respond: fault.Status(http.StatusTooManyRequests, http.Header{"Retry-After": {"2"}}, "")},
		}, []time.Duration{2 * time.Second}},
		"a dropped connection": {[]fault.Rule{
			{PathPrefix: "/v1/images", Times: 2, Respond: fault.Drop},
		}, []time.Duration{2 * time.Second}},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			for _, f := range tc.faults {
				e.svc.Faults().Add(f)
			}
			if _, err := e.c.Image(context.Background(), "build"); err != nil {
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
	e.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/images", Respond: service.Unavailable(time.Second)})
	n := e.count()
	_, err := e.c.Image(context.Background(), "build")
	if !errors.Is(err, ErrUnavailable) || *n != 8 {
		t.Fatalf("err = %v after %d requests, want 8", err, *n)
	}
}

func TestTheServiceUnreachable(t *testing.T) {
	e := setup(t)
	e.c.url = "http://127.0.0.1:1"
	_, err := e.c.Image(context.Background(), "build")
	if !errors.Is(err, ErrUnavailable) || len(*e.waits) != 3 {
		t.Fatalf("err = %v after waits %v", err, *e.waits)
	}
}

func TestFinalAnswers(t *testing.T) {
	for name, f := range map[string]func(http.ResponseWriter, *http.Request){
		"a refusal":                      service.Refuse("the run's commit doesn't declare it"),
		"a status it doesn't have":       fault.Status(http.StatusTeapot, nil, `{}`),
		"a redirect":                     fault.Status(http.StatusTemporaryRedirect, http.Header{"Location": {"/v1/images/other"}}, ""),
		"a 200 with no builds":           fault.Status(http.StatusOK, nil, `{"builds": []}`),
		"a 200 with another state":       fault.Status(http.StatusOK, nil, `{"builds": [{"architecture": "amd64", "version": "x", "state": "pending"}]}`),
		"an answer larger than any":      fault.Status(http.StatusOK, nil, `{"builds": [], "x": "`+strings.Repeat("x", maxAnswer)+`"}`),
		"a 200 that isn't the interface": fault.Status(http.StatusOK, nil, `not json`),
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/images", Respond: f})
			n := e.count()
			_, err := e.c.Image(context.Background(), "build")
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
				service.Refuse("you sent "+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))(w, r)
			}
		},
		"a refusal's reason, escaped": func(tok string) func(http.ResponseWriter, *http.Request) {
			return func(w http.ResponseWriter, r *http.Request) {
				t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				escaped := `\u` + fmt.Sprintf("%04x", t[0]) + t[1:]
				fault.Status(http.StatusForbidden, nil, `{"error": "Forbidden", "reason": "you sent `+escaped+`"}`)(w, r)
			}
		},
		"a build it doesn't have": func(string) func(http.ResponseWriter, *http.Request) {
			return func(w http.ResponseWriter, r *http.Request) {
				tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				fault.Status(http.StatusOK, nil, `{"builds": [{"architecture": "`+tok+`", "version": "x", "state": "odd"}]}`)(w, r)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/images", Respond: echo("")})
			_, err := e.c.Image(context.Background(), "build")
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
	if FollowEvery() != 5*time.Second || MetadataServer() != "http://metadata.google.internal" || MachineDir() != "/var/lib/lanescape-runners" {
		t.Fatalf("the actions' own builds poll every %s, at %s, in %s", FollowEvery(), MetadataServer(), MachineDir())
	}
}
