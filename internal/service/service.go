// Package service calls the runner service through its interface for
// actions, lanescape/runners' specs/0005-interface.md, at the version
// go.mod names: nothing else of the service's is relied on.
//
// Every request carries the job's GitHub OIDC token, whose audience is the
// service's address, endpoints.Service. Tokens, and what the service
// answers beyond a scope or a refusal's reason, are never logged.
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/actions/internal/backoff"
)

// Tokens gets the job's OIDC token for an audience.
type Tokens interface {
	Token(ctx context.Context, audience string) (string, error)
}

// Client calls the service.
type Client struct {
	// Tokens are the job's.
	Tokens Tokens
	// HTTP is the client to use; nil means one with a timeout above the
	// service's own.
	HTTP *http.Client
	// Retry is how answers that ask to try again are retried; zero means
	// DefaultRetry.
	Retry backoff.Policy
	// Logf, if set, is told of each retry, with nothing the service sent
	// but its status.
	Logf func(format string, args ...any)

	// url is the service's address and the tokens' audience: always
	// endpoints.Service, except in a binary built for the end-to-end
	// tests (url_e2e.go).
	url string
}

// DefaultRetry is how long a bind waits for GitHub to answer the service:
// eight attempts, the waits doubling from 2 seconds to a minute, or what
// Retry-After asks if longer.
var DefaultRetry = backoff.Policy{Attempts: 8, First: 2 * time.Second, Max: time.Minute}

// New is a client for the service at endpoints.Service.
func New(tokens Tokens) *Client {
	return &Client{Tokens: tokens, url: address()}
}

// URL is the service's address, which is also its tokens' audience.
func (c *Client) URL() string {
	if c.url == "" {
		return endpoints.Service
	}
	return c.url
}

// At is a client for a service elsewhere, for tests in this module.
func At(url string, tokens Tokens) *Client { return &Client{Tokens: tokens, url: url} }

// BindRequest is the body a bootstrap workflow sends in a
// pull_request_target run: what its own event says.
type BindRequest struct {
	PullRequest      int    `json:"pull_request"`
	HeadRepositoryID int64  `json:"head_repository_id"`
	HeadSHA          string `json:"head_sha"`
}

// Refused is the service's 403: the token or its run isn't one it binds,
// or the body isn't a binding request.
type Refused struct{ Reason string }

func (r *Refused) Error() string { return "the runner service refused to bind the run: " + r.Reason }

// ErrUnavailable means the service kept answering that GitHub couldn't
// confirm the run until the retries were spent.
var ErrUnavailable = errors.New("the runner service couldn't bind the run, since GitHub couldn't confirm it, and the retries are spent")

var scopePattern = regexp.MustCompile(`^(main|read|pr[1-9][0-9]*)$`)

// Bind binds the run's scope, or returns the binding it has (POST
// /v1/bind). req is nil except from a bootstrap workflow in a
// pull_request_target run.
func (c *Client) Bind(ctx context.Context, req *BindRequest) (string, error) {
	var body []byte
	if req != nil {
		var err error
		if body, err = json.Marshal(req); err != nil {
			return "", err
		}
	}
	p := c.Retry
	if p.Attempts <= 0 {
		p = defaultRetry()
	}
	var last error
	for n := 1; ; n++ {
		scope, asked, err := c.bindOnce(ctx, body)
		if err == nil {
			return scope, nil
		}
		var refused *Refused
		if errors.As(err, &refused) || errors.Is(err, errFinal) || ctx.Err() != nil {
			return "", err
		}
		last = err
		if n >= p.Attempts {
			return "", fmt.Errorf("%w: %w", ErrUnavailable, last)
		}
		wait := p.Wait(n, asked)
		if c.Logf != nil {
			c.Logf("The runner service can't bind the run yet (%v); trying again in %s.", err, wait.Round(time.Second))
		}
		if err := p.Pause(ctx, wait); err != nil {
			return "", errors.Join(last, err)
		}
	}
}

// errFinal marks a failure that trying again won't mend.
var errFinal = errors.New("final")

type final struct{ error }

func (f final) Unwrap() []error { return []error{f.error, errFinal} }

func (c *Client) bindOnce(ctx context.Context, body []byte) (string, time.Duration, error) {
	tok, err := c.Tokens.Token(ctx, c.URL())
	if err != nil {
		return "", 0, final{err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL()+"/v1/bind", bytes.NewReader(body))
	if err != nil {
		return "", 0, final{err}
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", 0, ctx.Err()
		}
		return "", 0, errors.New("the runner service couldn't be reached")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", 0, errors.New("the runner service's answer couldn't be read")
	}
	var out struct {
		Scope     string `json:"scope"`
		Reason    string `json:"reason"`
		Retryable bool   `json:"retryable"`
	}
	jsonErr := json.Unmarshal(data, &out)
	asked := backoff.RetryAfter(resp.Header, time.Now())
	switch {
	case resp.StatusCode == http.StatusOK:
		if jsonErr != nil || !scopePattern.MatchString(out.Scope) {
			return "", 0, final{errors.New("the runner service answered 200 without a scope")}
		}
		return out.Scope, 0, nil
	case resp.StatusCode == http.StatusForbidden:
		if out.Reason == "" {
			out.Reason = "it gave no reason"
		}
		return "", 0, &Refused{Reason: out.Reason}
	case resp.StatusCode == http.StatusServiceUnavailable && jsonErr == nil && out.Retryable:
		return "", asked, errors.New("GitHub couldn't confirm the run")
	case resp.StatusCode >= 500, resp.StatusCode == http.StatusTooManyRequests:
		// Not the interface's answer, but what a server in front of it
		// says when it's busy or restarting.
		return "", asked, fmt.Errorf("the runner service answered %d", resp.StatusCode)
	default:
		return "", 0, final{fmt.Errorf("the runner service answered %d, which its interface doesn't have", resp.StatusCode)}
	}
}
