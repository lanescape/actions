package oidc

import (
	"context"
	"errors"
	"strings"
	"testing"

	ghemu "github.com/lanescape/runners/emulators/github"

	"github.com/lanescape/actions/internal/testgithub"
)

func TestToken(t *testing.T) {
	gh := testgithub.New(t)
	org := gh.Org(303994381, "lanescape")
	repo := gh.Repo(org, 5001, "app")
	run := gh.PushRun(repo, "main")
	req := gh.RequestToken(run, ghemu.Job{})
	env := map[string]string{"ACTIONS_ID_TOKEN_REQUEST_URL": gh.TokenURL, "ACTIONS_ID_TOKEN_REQUEST_TOKEN": req}
	s, err := FromEnv(func(k string) string { return env[k] }, nil)
	if err != nil {
		t.Fatal(err)
	}
	var masked []string
	s.Mask = func(v string) { masked = append(masked, v) }
	tok, err := s.Token(context.Background(), "https://service.example?x=1")
	if err != nil {
		t.Fatal(err)
	}
	if len(gh.Audiences) != 1 || gh.Audiences[0] != "https://service.example?x=1" {
		t.Fatalf("audiences = %v", gh.Audiences)
	}
	if len(masked) != 1 || masked[0] != tok || strings.Count(tok, ".") != 2 {
		t.Fatalf("the token wasn't masked: %d masks", len(masked))
	}

	env["ACTIONS_ID_TOKEN_REQUEST_TOKEN"] = "wrong"
	s, _ = FromEnv(func(k string) string { return env[k] }, nil)
	if _, err := s.Token(context.Background(), "aud"); err == nil || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("err = %v", err)
	}
}

func TestFromEnvWithoutThePermission(t *testing.T) {
	if _, err := FromEnv(func(string) string { return "" }, nil); !errors.Is(err, ErrNoPermission) || !strings.Contains(err.Error(), "id-token: write") {
		t.Fatalf("err = %v", err)
	}
}
