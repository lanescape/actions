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
	"strings"
	"time"
	"unicode"

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

// maxAnswer bounds an answer: every answer the interface has is a small
// JSON object, a reason at most, so anything larger isn't one of them.
const maxAnswer = 64 << 10

// redact replaces the token wherever it appears in an answer, in its
// JSON strings once decoded, however escaped, and reports whether the
// answer is one JSON value. An answer that isn't is never decoded: it
// comes back with the token replaced in its raw bytes, for nothing but
// its status to be read.
func redact(data []byte, tok string) ([]byte, bool) {
	raw := bytes.ReplaceAll(data, []byte(tok), []byte("***"))
	dec := json.NewDecoder(bytes.NewReader(data))
	// Numbers stay as they're written, so none too large for a float
	// makes the answer undecodable here and decodable later.
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil || dec.Decode(new(any)) != io.EOF {
		return raw, false
	}
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case string:
			return strings.ReplaceAll(t, tok, "***")
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
		case map[string]any:
			out := make(map[string]any, len(t))
			for k, e := range t {
				out[strings.ReplaceAll(k, tok, "***")] = walk(e)
			}
			return out
		}
		return x
	}
	b, err := json.Marshal(walk(v))
	if err != nil {
		return raw, false
	}
	return b, true
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
	hc := &http.Client{Timeout: 2 * time.Minute}
	if c.HTTP != nil {
		copied := *c.HTTP
		hc = &copied
	}
	// A redirect isn't the interface's answer: it fails as one.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, errors.New("the runner service couldn't be reached")
	}
	defer resp.Body.Close()
	asked := backoff.RetryAfter(resp.Header, time.Now())
	// A 429 or 5xx is retried, whatever its body: what a server in front
	// of the service says when it's busy or restarting isn't the
	// interface's answer.
	busy := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		// A body cut short is a connection lost, but the status still
		// says what to do: a refusal stays one, and only an answer
		// worth asking again for is asked for again.
		switch {
		case resp.StatusCode == http.StatusForbidden:
			return 0, &Refused{Reason: "its reason couldn't be read"}
		case busy, resp.StatusCode == http.StatusOK:
			return asked, errors.New("its answer couldn't be read")
		default:
			return 0, final{fmt.Errorf("the runner service answered %d, which its interface doesn't have", resp.StatusCode)}
		}
	}
	if len(data) > maxAnswer {
		if busy {
			return asked, fmt.Errorf("it answered %d", resp.StatusCode)
		}
		return 0, final{errors.New("the runner service's answer is larger than any its interface gives")}
	}
	// Nothing the service sends back reaches the job's log with the
	// token in it, whatever it says, however it's escaped; an answer
	// that isn't JSON is never decoded.
	data, isJSON := redact(data, tok)
	var answer struct {
		Reason    string `json:"reason"`
		Retryable bool   `json:"retryable"`
	}
	if isJSON && json.Unmarshal(data, &answer) != nil {
		answer.Reason, answer.Retryable = "", false
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		if !isJSON || json.Unmarshal(data, out) != nil {
			return 0, final{errors.New("the runner service answered 200 with a body that isn't its interface's")}
		}
		return 0, nil
	case resp.StatusCode == http.StatusForbidden:
		if answer.Reason == "" {
			answer.Reason = "it gave no reason"
		}
		return 0, &Refused{Reason: oneLine(answer.Reason)}
	case resp.StatusCode == http.StatusServiceUnavailable && answer.Retryable:
		return asked, errors.New("it couldn't confirm an answer")
	case busy:
		return asked, fmt.Errorf("it answered %d", resp.StatusCode)
	default:
		return 0, final{fmt.Errorf("the runner service answered %d, which its interface doesn't have", resp.StatusCode)}
	}
}

// The states of a work disk's answer.
const (
	Making = "making"
	Ready  = "ready"
)

// Steps are where a work disk may come from, as its answer's restored
// says: a resume image, or a step of the restore order
// (lanescape/runners' specs/0005-interface.md, Work disks).
var Steps = map[string]string{
	"resume":              "a resume image of this job's own disk",
	"pull_request_exact":  "the pull request's own disk for this key",
	"pull_request_newest": "the pull request's newest disk",
	"main_exact":          "main's disk for this key",
	"main_newest":         "main's newest disk",
	"branch_exact":        "the branch's disk for this key",
	"branch_newest":       "the branch's newest disk",
	"none":                "nothing: a new, empty disk",
}

// Disk is the service's answer to a work disk request.
type Disk struct {
	// State is Making or Ready.
	State string `json:"state"`
	// Restored is where the disk came from, one of Steps.
	Restored string `json:"restored"`
}

// Disk asks for the job's work disk, of a family (POST /v1/disks), once,
// retrying only what the interface says to retry.
func (c *Client) Disk(ctx context.Context, family string) (Disk, error) {
	body, _ := json.Marshal(map[string]string{"family": family})
	var out Disk
	if err := c.do(ctx, http.MethodPost, "/v1/disks", body, &out); err != nil {
		return Disk{}, err
	}
	if out.State != Making && out.State != Ready {
		return Disk{}, fmt.Errorf("the runner service answered with a work disk's state its interface doesn't have: %q", out.State)
	}
	if _, ok := Steps[out.Restored]; !ok {
		return Disk{}, fmt.Errorf("the runner service answered that the work disk came from %q, which its interface doesn't have", out.Restored)
	}
	return out, nil
}

// Save is the service's answer to a save request.
type Save struct {
	// Saves is whether the run's scope saves, so the disk is saved if the
	// job succeeds.
	Saves bool
	// Reason is why it doesn't, if it doesn't.
	Reason string
}

// Save asks for the job's work disk to be saved when the job finishes
// (POST /v1/disks/save).
func (c *Client) Save(ctx context.Context) (Save, error) {
	var out struct {
		Saves  *bool  `json:"saves"`
		Reason string `json:"reason"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/disks/save", []byte(`{"save":true}`), &out); err != nil {
		return Save{}, err
	}
	if out.Saves == nil || *out.Saves == (out.Reason != "") {
		return Save{}, errors.New("the runner service answered a save with a body its interface doesn't have")
	}
	return Save{Saves: *out.Saves, Reason: oneLine(out.Reason)}, nil
}

// oneLine is a reason of the service's, as the job's log may show it: on
// one line, with no control characters, so it can't start a workflow
// command of its own.
func oneLine(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }), " ")
}
