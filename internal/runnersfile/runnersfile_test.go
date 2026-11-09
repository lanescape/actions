package runnersfile

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	f, err := Parse([]byte(`{
		"lanescape-80vcpu-linux-amd64": "ubuntu-24.04",
		"lanescape-2vcpu-linux-arm64": ["self-hosted", "arm64"],
		"bootstrap": "lanescape/app/.github/workflows/bootstrap.yml@refs/heads/main"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Names(); !slices.Equal(got, []string{"lanescape-2vcpu-linux-arm64", "lanescape-80vcpu-linux-amd64"}) {
		t.Errorf("names = %v", got)
	}
	if f.Bootstrap != "lanescape/app/.github/workflows/bootstrap.yml@refs/heads/main" {
		t.Errorf("bootstrap = %q", f.Bootstrap)
	}
	b, _ := json.Marshal(f.Fallbacks)
	if string(b) != `{"lanescape-2vcpu-linux-arm64":["self-hosted","arm64"],"lanescape-80vcpu-linux-amd64":"ubuntu-24.04"}` {
		t.Errorf("fallbacks = %s", b)
	}
}

func TestParseRefuses(t *testing.T) {
	for name, tc := range map[string]struct{ data, want string }{
		"not an object":   {`["a"]`, "isn't a JSON object"},
		"not JSON":        {`{`, "isn't a JSON object"},
		"trailing":        {`{"lanescape-2vcpu-linux-amd64": "a"} {}`, "has more"},
		"no runners":      {`{"bootstrap": "x"}`, "names no runners"},
		"empty":           {`{}`, "names no runners"},
		"not a shape":     {`{"big-runner": "ubuntu-24.04"}`, `"big-runner" isn't a runner name`},
		"upper case":      {`{"Lanescape-2vcpu-linux-amd64": "ubuntu-24.04"}`, "isn't a runner name"},
		"empty list":      {`{"lanescape-2vcpu-linux-amd64": []}`, "non-empty list"},
		"number":          {`{"lanescape-2vcpu-linux-amd64": 3}`, "non-empty list"},
		"empty label":     {`{"lanescape-2vcpu-linux-amd64": ""}`, "isn't a runner label"},
		"label in a list": {`{"lanescape-2vcpu-linux-amd64": ["a", " b"]}`, "isn't a runner label"},
		"twice":           {`{"lanescape-2vcpu-linux-amd64": "a", "lanescape-2vcpu-linux-amd64": "b"}`, "twice"},
		"bad bootstrap":   {`{"lanescape-2vcpu-linux-amd64": "a", "bootstrap": 1}`, "bootstrap must be"},
	} {
		_, err := Parse([]byte(tc.data))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one saying %q", name, err, tc.want)
		}
	}
}

func TestParseOverrides(t *testing.T) {
	o, err := ParseOverrides(` {"lanescape-2vcpu-linux-amd64": ["self-hosted", "x64"], "anything": "ubuntu-26.04"} `)
	if err != nil {
		t.Fatal(err)
	}
	if got := o["lanescape-2vcpu-linux-amd64"].Strings(); !slices.Equal(got, []string{"self-hosted", "x64"}) {
		t.Errorf("override = %v", got)
	}
	if got := o["anything"].Strings(); !slices.Equal(got, []string{"ubuntu-26.04"}) {
		t.Errorf("override = %v", got)
	}
	if o, err := ParseOverrides("  "); err != nil || o != nil {
		t.Errorf("empty = %v, %v", o, err)
	}
	if _, err := ParseOverrides(`"ubuntu"`); err == nil || !strings.Contains(err.Error(), "RUNNERS isn't a JSON object") {
		t.Errorf("err = %v", err)
	}
	if _, err := ParseOverrides(`{"a": {}}`); err == nil {
		t.Error("an object as a fallback was taken")
	}
}
