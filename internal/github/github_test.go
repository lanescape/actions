package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lanescape/actions/internal/backoff"
	"github.com/lanescape/actions/internal/testgithub"
)

var fast = backoff.Policy{Attempts: 3, First: time.Millisecond, Max: time.Millisecond}

func TestFileFromTheEmulator(t *testing.T) {
	gh := testgithub.New(t)
	org := gh.Org(303994381, "lanescape")
	repo := gh.Repo(org, 5001, "app")
	big := strings.Repeat("0123456789", 1000)
	first := repo.Head("main")
	sha := repo.Commit("main", map[string]string{"deps/versions.json": "café 1.0\n", "dir/a b.txt": big})
	c := &Client{BaseURL: gh.URL, Token: gh.JobToken(org), Retry: fast}
	ctx := context.Background()

	if b, err := c.File(ctx, "lanescape/app", "deps/versions.json", sha); err != nil || string(b) != "café 1.0\n" {
		t.Fatalf("file = %q, %v", b, err)
	}
	if b, err := c.File(ctx, "lanescape/app", "dir/a b.txt", sha); err != nil || string(b) != big {
		t.Fatalf("file with a space = %d bytes, %v", len(b), err)
	}
	// The file isn't at the repository's first commit.
	if _, err := c.File(ctx, "lanescape/app", "deps/versions.json", first); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, err := c.File(ctx, "lanescape/app", "missing.txt", sha); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, err := (&Client{BaseURL: gh.URL, Token: "wrong", Retry: fast}).File(ctx, "lanescape/app", "deps/versions.json", sha); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want a 401", err)
	}
}

func TestFileRetriesServerErrors(t *testing.T) {
	gh := testgithub.New(t)
	org := gh.Org(303994381, "lanescape")
	repo := gh.Repo(org, 5001, "app")
	sha := repo.Commit("main", map[string]string{"a": "1"})
	c := &Client{BaseURL: gh.URL, Token: gh.JobToken(org), Retry: fast}
	gh.ServerError("/repos/lanescape/app/contents/a", http.StatusBadGateway, 2)
	if b, err := c.File(context.Background(), "lanescape/app", "a", sha); err != nil || string(b) != "1" {
		t.Fatalf("file = %q, %v", b, err)
	}
	gh.ServerError("/repos/lanescape/app/contents/a", http.StatusBadGateway, 3)
	if _, err := c.File(context.Background(), "lanescape/app", "a", sha); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v, want the last 502", err)
	}
}

// Past 1 MB GitHub's contents API gives no content, only raw.
func TestFileReadsLargeFilesRaw(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Query().Get("ref") != "abc" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Accept") == "application/vnd.github.raw" {
			_, _ = w.Write([]byte("raw bytes"))
			return
		}
		_, _ = w.Write([]byte(`{"type": "file", "encoding": "none", "size": 2000000, "content": ""}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "tok", Retry: fast}
	if b, err := c.File(context.Background(), "o/r", "big.bin", "abc"); err != nil || string(b) != "raw bytes" || calls.Load() != 2 {
		t.Fatalf("file = %q, %v after %d calls", b, err, calls.Load())
	}
}

func TestFileRefusesWhatIsntAFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/contents/dir":
			_, _ = w.Write([]byte(`[{"type": "file"}]`))
		case "/repos/o/r/contents/sub":
			_, _ = w.Write([]byte(`{"type": "submodule"}`))
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Retry: fast}
	if _, err := c.File(context.Background(), "o/r", "dir", "abc"); err == nil || !strings.Contains(err.Error(), "isn't a file") {
		t.Errorf("directory: %v", err)
	}
	if _, err := c.File(context.Background(), "o/r", "sub", "abc"); err == nil || !strings.Contains(err.Error(), "submodule") {
		t.Errorf("submodule: %v", err)
	}
}

func TestValidPath(t *testing.T) {
	for _, p := range []string{"a", "a/b.json", ".github/runners.json", "a b/c"} {
		if !ValidPath(p) {
			t.Errorf("%q isn't valid", p)
		}
	}
	for _, p := range []string{"", "/a", "a/", "./a", "a/../b", "a//b", `a\b`, "..", "a/./b"} {
		if ValidPath(p) {
			t.Errorf("%q is valid", p)
		}
	}
}
