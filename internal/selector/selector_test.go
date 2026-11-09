package selector

import "testing"

// The spec's worked example (specs/0001-actions.md, runners).
func TestDiskKeyExample(t *testing.T) {
	got := DiskKey("build", "lanescape-80vcpu-linux-amd64", []File{
		{Path: "deps/versions.json", Contents: []byte("café 1.0\n")},
		{Path: "rust-toolchain.toml", Contents: nil},
	})
	if got != "a77434447088c41f6e9f" {
		t.Fatalf("key = %s, want a77434447088c41f6e9f", got)
	}
	if !Key.MatchString(got) {
		t.Fatalf("key %s isn't the interface's", got)
	}
}

func TestDiskKeyChangesWithEveryPart(t *testing.T) {
	files := []File{{"a", []byte("1")}, {"b", []byte("2")}}
	base := DiskKey("build", "lanescape-2vcpu-linux-amd64", files)
	for name, k := range map[string]string{
		"family":   DiskKey("build2", "lanescape-2vcpu-linux-amd64", files),
		"runner":   DiskKey("build", "lanescape-4vcpu-linux-amd64", files),
		"contents": DiskKey("build", "lanescape-2vcpu-linux-amd64", []File{{"a", []byte("1")}, {"b", []byte("3")}}),
		"path":     DiskKey("build", "lanescape-2vcpu-linux-amd64", []File{{"a", []byte("1")}, {"c", []byte("2")}}),
		"order":    DiskKey("build", "lanescape-2vcpu-linux-amd64", []File{{"b", []byte("2")}, {"a", []byte("1")}}),
		// Length prefixes keep a part's bytes from moving into the next.
		"boundary": DiskKey("build", "lanescape-2vcpu-linux-amd64", []File{{"a1", nil}, {"b", []byte("2")}}),
	} {
		if k == base {
			t.Errorf("changing the %s left the key %s", name, k)
		}
	}
}

func TestSelectors(t *testing.T) {
	if got := Runner("lanescape-80vcpu-linux-amd64", "pr42"); got != "lanescape-80vcpu-linux-amd64-pr42" {
		t.Errorf("runner selector = %s", got)
	}
	if got := Disk("lanescape-80vcpu-linux-amd64", true, "build", "3f9c2e7a10b4d5e6f708", "pr42"); got != "lanescape-80vcpu-linux-amd64-spot-disk-build-3f9c2e7a10b4d5e6f708-pr42" {
		t.Errorf("disk selector = %s", got)
	}
	if got := Disk("lanescape-2vcpu-linux-arm64", false, "cache", NoKey, "main"); got != "lanescape-2vcpu-linux-arm64-disk-cache-00000000000000000000-main" {
		t.Errorf("disk selector = %s", got)
	}
}

func TestPatterns(t *testing.T) {
	for _, s := range []string{"lanescape-2vcpu-linux-amd64", "lanescape-80vcpu-linux-arm64"} {
		if !Shape.MatchString(s) {
			t.Errorf("%s isn't a shape", s)
		}
	}
	for _, s := range []string{"lanescape-0vcpu-linux-amd64", "lanescape-2vcpu-linux-386", "Lanescape-2vcpu-linux-amd64", "lanescape-2vcpu-linux-amd64-pr1"} {
		if Shape.MatchString(s) {
			t.Errorf("%s is a shape", s)
		}
	}
	for _, s := range []string{"a", "build", "b-1", "a234567890123456789012345678901b"} {
		if !Family.MatchString(s) {
			t.Errorf("%s isn't a family", s)
		}
	}
	for _, s := range []string{"", "1a", "-a", "Build", "a_b", "a2345678901234567890123456789012c"} {
		if Family.MatchString(s) {
			t.Errorf("%q is a family", s)
		}
	}
	for _, s := range []string{"main", "read", "pr1", "pr42"} {
		if !Scope.MatchString(s) {
			t.Errorf("%s isn't a scope", s)
		}
	}
	for _, s := range []string{"pr0", "pr", "pr01", "Main", "dev"} {
		if Scope.MatchString(s) {
			t.Errorf("%s is a scope", s)
		}
	}
}
