package service

// These tests check how the client reads each answer the interface has,
// and some it doesn't, from a handler that gives them on cue. The
// end-to-end tests (e2e/) bind against the real service.

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
	return "secret-oidc-token", t.err
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
		if r.Method != http.MethodPost || r.URL.Path != "/v1/bind" {
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

func TestBind(t *testing.T) {
	srv, bodies, auths := cue(t, answer{200, `{"scope": "pr42", "other": 1}`, ""})
	tk := &tokens{}
	var waits []time.Duration
	scope, err := client(srv.URL, tk, &waits).Bind(context.Background(), nil)
	if err != nil || scope != "pr42" {
		t.Fatalf("scope = %q, %v", scope, err)
	}
	if (*bodies)[0] != "" || (*auths)[0] != "Bearer secret-oidc-token" || tk.audiences[0] != srv.URL {
		t.Fatalf("body %q, authorization %q, audience %v", (*bodies)[0], (*auths)[0], tk.audiences)
	}
}

func TestBindSendsTheContribution(t *testing.T) {
	srv, bodies, _ := cue(t, answer{200, `{"scope": "pr7"}`, ""})
	var waits []time.Duration
	_, err := client(srv.URL, &tokens{}, &waits).Bind(context.Background(), &BindRequest{PullRequest: 7, HeadRepositoryID: 7001, HeadSHA: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if (*bodies)[0] != `{"pull_request":7,"head_repository_id":7001,"head_sha":"abc"}` {
		t.Fatalf("body = %s", (*bodies)[0])
	}
}

func TestBindRefused(t *testing.T) {
	srv, bodies, _ := cue(t, answer{403, `{"error": "Forbidden", "reason": "run 5 is completed, not in progress"}`, ""})
	var waits []time.Duration
	_, err := client(srv.URL, &tokens{}, &waits).Bind(context.Background(), nil)
	var refused *Refused
	if !errors.As(err, &refused) || refused.Reason != "run 5 is completed, not in progress" || len(*bodies) != 1 {
		t.Fatalf("err = %v after %d calls", err, len(*bodies))
	}
}

func TestBindRetriesWhatGitHubCouldntConfirm(t *testing.T) {
	srv, bodies, _ := cue(t,
		answer{503, `{"retryable": true}`, "5"},
		answer{502, `<html>`, ""},
		answer{200, `{"scope": "main"}`, ""})
	tk := &tokens{}
	var waits []time.Duration
	scope, err := client(srv.URL, tk, &waits).Bind(context.Background(), nil)
	if err != nil || scope != "main" || len(*bodies) != 3 {
		t.Fatalf("scope = %q, %v after %d calls", scope, err, len(*bodies))
	}
	// At least Retry-After, then the doubled backoff.
	if len(waits) != 2 || waits[0] != 5*time.Second || waits[1] != 4*time.Second {
		t.Fatalf("waits = %v", waits)
	}
	// A fresh token for every attempt.
	if len(tk.audiences) != 3 {
		t.Fatalf("tokens = %d", len(tk.audiences))
	}
}

func TestBindFailsOnceRetriesAreSpent(t *testing.T) {
	srv, bodies, _ := cue(t, answer{503, `{"retryable": true}`, "1"})
	var waits []time.Duration
	_, err := client(srv.URL, &tokens{}, &waits).Bind(context.Background(), nil)
	if !errors.Is(err, ErrUnavailable) || len(*bodies) != 4 || len(waits) != 3 {
		t.Fatalf("err = %v after %d calls and %d waits", err, len(*bodies), len(waits))
	}
}

func TestBindRefusesAnswersOutsideTheInterface(t *testing.T) {
	for name, a := range map[string]answer{
		"200 without a scope":  {200, `{}`, ""},
		"200 with a bad scope": {200, `{"scope": "dev"}`, ""},
		"200 not JSON":         {200, `scope`, ""},
		"401":                  {401, `{}`, ""},
		"404":                  {404, `{}`, ""},
	} {
		srv, bodies, _ := cue(t, a)
		var waits []time.Duration
		_, err := client(srv.URL, &tokens{}, &waits).Bind(context.Background(), nil)
		if err == nil || errors.Is(err, ErrUnavailable) || len(*bodies) != 1 {
			t.Errorf("%s: err = %v after %d calls", name, err, len(*bodies))
		}
	}
}

func TestBindWithoutAToken(t *testing.T) {
	srv, bodies, _ := cue(t, answer{200, `{"scope": "main"}`, ""})
	var waits []time.Duration
	_, err := client(srv.URL, &tokens{err: errors.New("no id-token: write")}, &waits).Bind(context.Background(), nil)
	if err == nil || len(*bodies) != 0 || len(waits) != 0 {
		t.Fatalf("err = %v after %d calls", err, len(*bodies))
	}
}

// No error says what the token is.
func TestBindNeverShowsTheToken(t *testing.T) {
	for _, a := range []answer{{403, `{"reason": "no"}`, ""}, {503, `{"retryable": true}`, ""}, {418, ``, ""}} {
		srv, _, _ := cue(t, a)
		var waits []time.Duration
		if _, err := client(srv.URL, &tokens{}, &waits).Bind(context.Background(), nil); err == nil || strings.Contains(err.Error(), "secret-oidc-token") {
			t.Errorf("%d: err = %v", a.code, err)
		}
	}
}

func TestTheServiceIsTheInterfaces(t *testing.T) {
	if got := New(&tokens{}).URL(); got != endpoints.Service {
		t.Fatalf("service = %s, want endpoints.Service, %s", got, endpoints.Service)
	}
}
