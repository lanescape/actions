// Package oidc gets the job's GitHub OIDC token, for an audience, as
// GitHub's toolkit does: from ACTIONS_ID_TOKEN_REQUEST_URL, with
// ACTIONS_ID_TOKEN_REQUEST_TOKEN, which GitHub sets only for a job with
// id-token: write.
//
// Neither the request token nor the OIDC token is ever logged.
package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ErrNoPermission means the job can't get a token: it lacks id-token:
// write.
var ErrNoPermission = errors.New("the job can't get a GitHub OIDC token; give it the permission id-token: write")

// Source gets the job's tokens.
type Source struct {
	url, token string
	http       *http.Client
	// Mask, if set, is told every token got, for the log to hide.
	Mask func(string)
}

// FromEnv is the job's source, or ErrNoPermission.
func FromEnv(getenv func(string) string, client *http.Client) (*Source, error) {
	u, t := getenv("ACTIONS_ID_TOKEN_REQUEST_URL"), getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if u == "" || t == "" {
		return nil, ErrNoPermission
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Source{url: u, token: t, http: client}, nil
}

// Token is a token whose audience is aud.
func (s *Source) Token(ctx context.Context, aud string) (string, error) {
	u, err := url.Parse(s.url)
	if err != nil {
		return "", errors.New("ACTIONS_ID_TOKEN_REQUEST_URL isn't a URL")
	}
	q := u.Query()
	q.Set("audience", aud)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		// Not err itself: its URL is GitHub's, but nothing is gained by
		// showing it.
		return "", errors.New("GitHub's OIDC token endpoint couldn't be reached")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", errors.New("GitHub's OIDC token endpoint's answer couldn't be read")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub's OIDC token endpoint answered %d", resp.StatusCode)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Value == "" {
		return "", errors.New("GitHub's OIDC token endpoint gave no token")
	}
	if s.Mask != nil {
		s.Mask(out.Value)
	}
	return out.Value, nil
}
