// Package github reads files of a repository at a commit through GitHub's
// REST API, with the job's own token, so that no checkout is needed.
package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lanescape/actions/internal/backoff"
)

// ErrNotFound means the file isn't at that commit, or the token can't see
// the repository.
var ErrNotFound = errors.New("not found")

// maxFile is the largest file read, GitHub's own limit for the contents API.
const maxFile = 100 << 20

// Client calls GitHub's REST API.
type Client struct {
	// BaseURL is GITHUB_API_URL, with no trailing slash.
	BaseURL string
	// Token is the job's token. It's sent only to BaseURL.
	Token string
	// HTTP is the client to use; nil means one with a timeout.
	HTTP *http.Client
	// Retry is how transient failures are retried; zero means three
	// attempts.
	Retry backoff.Policy
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: time.Minute}
}

func (c *Client) policy() backoff.Policy {
	if c.Retry.Attempts > 0 {
		return c.Retry
	}
	return backoff.Policy{Attempts: 3, First: time.Second, Max: 10 * time.Second}
}

// contents is the contents API's answer for a path.
type contents struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Size     int64  `json:"size"`
	Content  string `json:"content"`
}

// File returns a file's bytes in repo, owner/name, at ref, a commit SHA.
func (c *Client) File(ctx context.Context, repo, path, ref string) ([]byte, error) {
	if !validPath(path) {
		return nil, fmt.Errorf("%q isn't a path in a repository", path)
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("%q isn't a repository", repo)
	}
	var segs []string
	for s := range strings.SplitSeq(path, "/") {
		segs = append(segs, url.PathEscape(s))
	}
	u := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s", c.BaseURL, url.PathEscape(owner), url.PathEscape(name),
		strings.Join(segs, "/"), url.QueryEscape(ref))

	body, err := c.get(ctx, u, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	var obj contents
	if err := json.Unmarshal(body, &obj); err != nil {
		// A directory is a list.
		return nil, fmt.Errorf("%s isn't a file", path)
	}
	if obj.Type != "file" {
		return nil, fmt.Errorf("%s is a %s, not a file", path, obj.Type)
	}
	switch obj.Encoding {
	case "base64":
		data, err := base64.StdEncoding.DecodeString(strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' {
				return -1
			}
			return r
		}, obj.Content))
		if err != nil {
			return nil, fmt.Errorf("%s: GitHub's base64 can't be read: %w", path, err)
		}
		return data, nil
	case "none", "":
		// Past 1 MB, GitHub gives a file's contents only raw.
		return c.get(ctx, u, "application/vnd.github.raw")
	default:
		return nil, fmt.Errorf("%s: GitHub encoded it as %q", path, obj.Encoding)
	}
}

// validPath is a clean path relative to the repository's root.
func validPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return false
	}
	for s := range strings.SplitSeq(p, "/") {
		if s == "" || s == "." || s == ".." {
			return false
		}
	}
	return true
}

// ValidPath reports whether p is a clean path relative to a repository's
// root: no leading or trailing slash, no empty, . or .. segment, and no
// backslash.
func ValidPath(p string) bool { return validPath(p) }

// Status is an answer GitHub gave that isn't a success.
type Status struct {
	Code    int
	Message string
}

func (s *Status) Error() string {
	if s.Message != "" {
		return fmt.Sprintf("GitHub answered %d: %s", s.Code, s.Message)
	}
	return fmt.Sprintf("GitHub answered %d", s.Code)
}

func (c *Client) get(ctx context.Context, u, accept string) ([]byte, error) {
	p := c.policy()
	var last error
	for n := 1; ; n++ {
		body, asked, retry, err := c.once(ctx, u, accept)
		if err == nil {
			return body, nil
		}
		last = err
		if !retry || n >= p.Attempts {
			return nil, last
		}
		if err := p.Pause(ctx, p.Wait(n, asked)); err != nil {
			return nil, errors.Join(last, err)
		}
	}
}

// once makes one request, and says whether a failure may pass.
func (c *Client) once(ctx context.Context, u, accept string) ([]byte, time.Duration, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, false, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "lanescape-actions")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, false, ctx.Err()
		}
		// The error names the URL, which holds no credential.
		return nil, 0, true, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFile+1))
	if err != nil {
		return nil, 0, true, err
	}
	if resp.StatusCode == http.StatusOK {
		if len(body) > maxFile {
			return nil, 0, false, errors.New("the file is larger than GitHub serves")
		}
		return body, 0, false, nil
	}
	var e struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &e)
	st := &Status{Code: resp.StatusCode, Message: e.Message}
	asked := backoff.RetryAfter(resp.Header, time.Now())
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, 0, false, fmt.Errorf("%w: %w", ErrNotFound, st)
	case resp.StatusCode >= 500, resp.StatusCode == http.StatusTooManyRequests:
		return nil, asked, true, st
	case resp.StatusCode == http.StatusForbidden && asked > 0:
		// A secondary rate limit says when to try again.
		return nil, asked, true, st
	default:
		return nil, 0, false, st
	}
}
