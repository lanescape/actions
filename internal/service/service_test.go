package service

// These tests check how the client reads each answer the interface has,
// and some it doesn't, from a handler that gives them on cue. The
// end-to-end tests (e2e/) run against lanescape/runners' emulator of the
// service.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/actions/internal/backoff"
)

type tokens struct {
	mu        sync.Mutex
	audiences []string
	err       error
}

func (t *tokens) Token(_ context.Context, aud string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.audiences = append(t.audiences, aud)
	return "secret-identity-token", t.err
}

type answer struct {
	code       int
	body       string
	retryAfter string
}

// cue serves answers in turn, recording each request's body and token.
func cue(t *testing.T, answers ...answer) (*httptest.Server, *[]string, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var bodies, auths []string
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/v1/images" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		auths = append(auths, r.Header.Get("Authorization"))
		a := answers[min(n, len(answers)-1)]
		n++
		if a.retryAfter != "" {
			w.Header().Set("Retry-After", a.retryAfter)
		}
		w.WriteHeader(a.code)
		_, _ = io.WriteString(w, a.body)
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies, &auths
}

func client(url string, tk *tokens, waits *[]time.Duration) *Client {
	c := At(url, tk)
	c.Retry = backoff.Policy{Attempts: 4, First: 2 * time.Second, Max: time.Minute, Sleep: func(_ context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return nil
	}}
	return c
}

func TestImage(t *testing.T) {
	srv, bodies, auths := cue(t, answer{200, `{"builds": [{"architecture": "amd64", "version": "0123456789abcdef0123", "state": "building", "build": "b1"}], "other": 1}`, ""})
	tk := &tokens{}
	var waits []time.Duration
	bs, err := client(srv.URL, tk, &waits).Image(context.Background(), "build")
	if err != nil || len(bs) != 1 || bs[0].State != Building || bs[0].Build != "b1" {
		t.Fatalf("builds = %+v, %v", bs, err)
	}
	if (*bodies)[0] != `{"name":"build"}` || (*auths)[0] != "Bearer secret-identity-token" || tk.audiences[0] != srv.URL {
		t.Fatalf("request = %q %q, audience %v", *bodies, *auths, tk.audiences)
	}
}

func TestEveryRequestGetsAFreshToken(t *testing.T) {
	srv, _, _ := cue(t, answer{503, `{"retryable": true}`, "1"}, answer{200, `{"builds": [{"architecture": "amd64", "version": "0123456789abcdef0123", "state": "ready"}]}`, ""})
	tk := &tokens{}
	var waits []time.Duration
	if _, err := client(srv.URL, tk, &waits).Image(context.Background(), "build"); err != nil {
		t.Fatal(err)
	}
	if len(tk.audiences) != 2 {
		t.Fatalf("%d tokens for 2 requests", len(tk.audiences))
	}
}

func TestRetries(t *testing.T) {
	for name, tc := range map[string]struct {
		answers []answer
		waits   []time.Duration
	}{
		"503s, waiting as asked or doubling": {[]answer{{503, `{"retryable": true}`, "5"}, {503, `{"retryable": true}`, ""}, {502, "bad gateway", ""},
			{200, `{"builds": [{"architecture": "amd64", "version": "0123456789abcdef0123", "state": "ready"}]}`, ""}},
			[]time.Duration{5 * time.Second, 4 * time.Second, 8 * time.Second}},
		"a Retry-After over a minute waits a minute": {[]answer{{503, `{"retryable": true}`, "600"},
			{200, `{"builds": [{"architecture": "amd64", "version": "0123456789abcdef0123", "state": "ready"}]}`, ""}},
			[]time.Duration{time.Minute}},
		"429": {[]answer{{429, "", "2"}, {200, `{"builds": [{"architecture": "amd64", "version": "0123456789abcdef0123", "state": "ready"}]}`, ""}},
			[]time.Duration{2 * time.Second}},
	} {
		t.Run(name, func(t *testing.T) {
			srv, _, _ := cue(t, tc.answers...)
			var waits []time.Duration
			if _, err := client(srv.URL, &tokens{}, &waits).Image(context.Background(), "build"); err != nil {
				t.Fatal(err)
			}
			if len(waits) != len(tc.waits) {
				t.Fatalf("waits = %v, want %v", waits, tc.waits)
			}
			for i := range waits {
				if waits[i] != tc.waits[i] {
					t.Fatalf("waits = %v, want %v", waits, tc.waits)
				}
			}
		})
	}
}

func TestRetriesSpent(t *testing.T) {
	srv, bodies, _ := cue(t, answer{503, `{"retryable": true}`, "1"})
	var waits []time.Duration
	_, err := client(srv.URL, &tokens{}, &waits).Image(context.Background(), "build")
	if !errors.Is(err, ErrUnavailable) || len(*bodies) != 4 {
		t.Fatalf("err = %v after %d requests", err, len(*bodies))
	}
}

func TestFinalAnswers(t *testing.T) {
	for name, a := range map[string]answer{
		"a refusal":                  {403, `{"error": "Forbidden", "reason": "the run's commit doesn't declare it"}`, ""},
		"a status it doesn't have":   {418, `{}`, ""},
		"a 503 that isn't retryable": {503, `{"retryable": false}`, ""},
		"a 200 with no builds":       {200, `{"builds": []}`, ""},
		"a 200 with another state":   {200, `{"builds": [{"architecture": "amd64", "version": "x", "state": "pending"}]}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			srv, bodies, _ := cue(t, a)
			var waits []time.Duration
			_, err := client(srv.URL, &tokens{}, &waits).Image(context.Background(), "build")
			if err == nil {
				t.Fatal("no error")
			}
			if name == "a 503 that isn't retryable" {
				// A 503 is a busy server's, retried, unless the retries
				// are spent.
				return
			}
			if len(*bodies) != 1 {
				t.Fatalf("retried a final answer %d times: %v", len(*bodies), err)
			}
			var r *Refused
			if name == "a refusal" && (!errors.As(err, &r) || r.Reason != "the run's commit doesn't declare it") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestFollow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/images/builds/b1" || r.URL.Query().Get("offset") != "7" {
			t.Errorf("%s %s", r.Method, r.URL)
		}
		_, _ = io.WriteString(w, `{"state": "building", "log": "more\n", "next": 12}`)
	}))
	defer srv.Close()
	var waits []time.Duration
	p, err := client(srv.URL, &tokens{}, &waits).Follow(context.Background(), "b1", 7)
	if err != nil || p.State != Building || p.Log != "more\n" || p.Next != 12 {
		t.Fatalf("progress = %+v, %v", p, err)
	}
}

func TestTheTokenIsNeverInAnError(t *testing.T) {
	for _, a := range []answer{{403, `{"reason": "x"}`, ""}, {500, "secret-identity-token", ""}, {418, "secret-identity-token", ""}} {
		srv, _, _ := cue(t, a)
		var waits []time.Duration
		_, err := client(srv.URL, &tokens{}, &waits).Image(context.Background(), "build")
		if err == nil || strings.Contains(err.Error(), "secret-identity-token") {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestTheAudienceIsTheService(t *testing.T) {
	if New(&tokens{}).URL() != endpoints.Service {
		t.Fatal("the service's address isn't endpoints.Service")
	}
}
