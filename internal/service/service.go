// Package service calls the runner service through its interface for
// actions, lanescape/runners' specs/0005-interface.md, at the version
// go.mod names: nothing else of the service's is relied on.
//
// Every request is signed with the machine's identity token, for the
// service's address, endpoints.Service, got afresh for each request from
// the machine's metadata server. Tokens are never logged.
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/actions/internal/backoff"
)

// Tokens gets the machine's identity token for an audience.
type Tokens interface {
	Token(ctx context.Context, audience string) (string, error)
}

// Client calls the service.
type Client struct {
	// Tokens are the machine's.
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

// DefaultRetry is how long a request waits for the service: eight attempts
// in all, the waits doubling from 2 seconds to a minute, or what
// Retry-After asks if longer, up to a minute.
var DefaultRetry = backoff.Policy{Attempts: 8, First: 2 * time.Second, Max: time.Minute}

// New is a client for the service at endpoints.Service.
func New(tokens Tokens) *Client { return &Client{Tokens: tokens, url: address()} }

// URL is the service's address, which is also its tokens' audience.
func (c *Client) URL() string {
	if c.url == "" {
		return endpoints.Service
	}
	return c.url
}

// At is a client for a service elsewhere, for tests in this module.
func At(url string, tokens Tokens) *Client { return &Client{Tokens: tokens, url: url} }

// Refused is the service's 403, with its reason.
type Refused struct{ Reason string }

func (r *Refused) Error() string { return "the runner service refused: " + r.Reason }

// ErrUnavailable means the service kept answering that it couldn't confirm
// an answer, or couldn't be reached, until the retries were spent.
var ErrUnavailable = errors.New("the runner service couldn't answer, and the retries are spent")

// errFinal marks a failure that trying again won't mend.
var errFinal = errors.New("final")

type final struct{ error }

func (f final) Unwrap() []error { return []error{f.error, errFinal} }

// do sends a request, signed afresh each attempt, and decodes a 200's body
// into out, retrying what the interface says to retry.
func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	p := c.Retry
	if p.Attempts <= 0 {
		p = defaultRetry()
	}
	var last error
	for n := 1; ; n++ {
		asked, err := c.once(ctx, method, path, body, out)
		if err == nil {
			return nil
		}
		var refused *Refused
		if errors.As(err, &refused) || errors.Is(err, errFinal) || ctx.Err() != nil {
			return err
		}
		last = err
		if n >= p.Attempts {
			return fmt.Errorf("%w: %w", ErrUnavailable, last)
		}
		wait := p.Wait(n, asked)
		if c.Logf != nil {
			c.Logf("The runner service can't answer yet (%v); trying again in %s.", err, wait.Round(time.Second))
		}
		if err := p.Pause(ctx, wait); err != nil {
			return errors.Join(last, err)
		}
	}
}

func (c *Client) once(ctx context.Context, method, path string, body []byte, out any) (time.Duration, error) {
	tok, err := c.Tokens.Token(ctx, c.URL())
	if err != nil {
		return 0, final{fmt.Errorf("the machine's identity token: %w", err)}
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL()+path, r)
	if err != nil {
		return 0, final{err}
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, errors.New("the runner service couldn't be reached")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, errors.New("the runner service's answer couldn't be read")
	}
	var answer struct {
		Reason    string `json:"reason"`
		Retryable bool   `json:"retryable"`
	}
	jsonErr := json.Unmarshal(data, &answer)
	asked := backoff.RetryAfter(resp.Header, time.Now())
	switch {
	case resp.StatusCode == http.StatusOK:
		if err := json.Unmarshal(data, out); err != nil {
			return 0, final{errors.New("the runner service answered 200 with a body that isn't its interface's")}
		}
		return 0, nil
	case resp.StatusCode == http.StatusForbidden:
		if answer.Reason == "" {
			answer.Reason = "it gave no reason"
		}
		return 0, &Refused{Reason: answer.Reason}
	case resp.StatusCode == http.StatusServiceUnavailable && jsonErr == nil && answer.Retryable:
		return asked, errors.New("it couldn't confirm an answer")
	case resp.StatusCode >= 500, resp.StatusCode == http.StatusTooManyRequests:
		// Not the interface's answer, but what a server in front of it
		// says when it's busy or restarting.
		return asked, fmt.Errorf("it answered %d", resp.StatusCode)
	default:
		return 0, final{fmt.Errorf("the runner service answered %d, which its interface doesn't have", resp.StatusCode)}
	}
}

// Build is one architecture's state of an image the run asked for.
type Build struct {
	Architecture string `json:"architecture"`
	Version      string `json:"version"`
	State        string `json:"state"`
	Build        string `json:"build"`
	Reason       string `json:"reason"`
}

// The states of a build.
const (
	Ready    = "ready"
	Building = "building"
	Failed   = "failed"
)

// Image asks for an image of the run's commit, by name (POST /v1/images).
func (c *Client) Image(ctx context.Context, name string) ([]Build, error) {
	body, _ := json.Marshal(map[string]string{"name": name})
	var out struct {
		Builds []Build `json:"builds"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/images", body, &out); err != nil {
		return nil, err
	}
	if len(out.Builds) == 0 {
		return nil, errors.New("the runner service answered with no builds")
	}
	for _, b := range out.Builds {
		if b.Architecture == "" || b.Version == "" || (b.State != Ready && b.State != Building && b.State != Failed) ||
			(b.State == Building && b.Build == "") {
			return nil, fmt.Errorf("the runner service answered with a build its interface doesn't have: %+v", b)
		}
	}
	return out.Builds, nil
}

// Progress is a build's state, and its log from an offset.
type Progress struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
	Log    string `json:"log"`
	Next   int64  `json:"next"`
}

// Follow is a build's state and its log from offset (GET
// /v1/images/builds/<id>).
func (c *Client) Follow(ctx context.Context, build string, offset int64) (Progress, error) {
	var out Progress
	path := "/v1/images/builds/" + url.PathEscape(build) + "?offset=" + strconv.FormatInt(offset, 10)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return Progress{}, err
	}
	if out.State != Ready && out.State != Building && out.State != Failed || out.Next < offset {
		return Progress{}, fmt.Errorf("the runner service answered with a build's state its interface doesn't have: %q", out.State)
	}
	return out, nil
}
