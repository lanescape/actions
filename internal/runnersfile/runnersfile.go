// Package runnersfile reads a consumer's .github/runners.json, and the
// RUNNERS variable that overrides its fallbacks, in the format of
// lanescape/runners' specs/0005-interface.md (A consumer's files).
package runnersfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/lanescape/actions/internal/selector"
)

// Path is the file's path in the repository.
const Path = ".github/runners.json"

// BootstrapKey is the entry naming the bootstrap workflow, and
// BranchesKey the one listing long-lived branches; neither is a runner.
const (
	BootstrapKey = "bootstrap"
	BranchesKey  = "branches"
)

// Labels is a fallback: one label, or a list of them, as written.
type Labels struct {
	list []string
	one  bool
}

// Label is a single label.
func Label(l string) Labels { return Labels{list: []string{l}, one: true} }

// List is a list of labels.
func List(ls ...string) Labels { return Labels{list: slices.Clone(ls)} }

// Strings are the labels.
func (l Labels) Strings() []string { return slices.Clone(l.list) }

// MarshalJSON writes the labels as they were written: a string, or a list.
func (l Labels) MarshalJSON() ([]byte, error) {
	if l.one {
		return json.Marshal(l.list[0])
	}
	return json.Marshal(l.list)
}

// String is how a summary shows the labels.
func (l Labels) String() string {
	return strings.Join(l.list, ", ")
}

// UnmarshalJSON reads a label or a non-empty list of labels.
func (l *Labels) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		if !validLabel(one) {
			return fmt.Errorf("%q isn't a runner label", one)
		}
		*l = Label(one)
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil || len(list) == 0 {
		return errors.New("a fallback must be a label or a non-empty list of labels")
	}
	for _, s := range list {
		if !validLabel(s) {
			return fmt.Errorf("%q isn't a runner label", s)
		}
	}
	*l = List(list...)
	return nil
}

func validLabel(s string) bool {
	return s != "" && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\n\r\x00")
}

// File is a parsed .github/runners.json.
type File struct {
	// Fallbacks are each runner's labels outside the organization.
	Fallbacks map[string]Labels
	// Bootstrap is the bootstrap workflow's reference, if named.
	Bootstrap string
	// Branches are the long-lived branches listed, each with the name of
	// its scope, branch-<name>.
	Branches map[string]string
}

// Names are the file's runner names, sorted.
func (f File) Names() []string {
	names := make([]string, 0, len(f.Fallbacks))
	for n := range f.Fallbacks {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Parse reads the file: an object from each runner name, a shape such as
// lanescape-80vcpu-linux-amd64, to its fallback; and optionally
// "bootstrap", a string, and "branches", an object from each long-lived
// branch's name to its scope's, which no two branches share.
func Parse(data []byte) (File, error) {
	raw, err := object(data)
	if err != nil {
		return File{}, fmt.Errorf("%s %w", Path, err)
	}
	f := File{Fallbacks: map[string]Labels{}}
	var errs []error
	for name, v := range raw {
		if name == BootstrapKey {
			if err := json.Unmarshal(v, &f.Bootstrap); err != nil || f.Bootstrap == "" {
				errs = append(errs, fmt.Errorf("%s: bootstrap must be a workflow reference", Path))
			}
			continue
		}
		if name == BranchesKey {
			f.Branches, err = branches(v)
			if err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if !selector.Shape.MatchString(name) {
			errs = append(errs, fmt.Errorf("%s: %q isn't a runner name, such as lanescape-2vcpu-linux-amd64", Path, name))
			continue
		}
		var l Labels
		if err := json.Unmarshal(v, &l); err != nil {
			errs = append(errs, fmt.Errorf("%s: %s: %w", Path, name, err))
			continue
		}
		f.Fallbacks[name] = l
	}
	if err := errors.Join(errs...); err != nil {
		return File{}, err
	}
	if len(f.Fallbacks) == 0 {
		return File{}, fmt.Errorf("%s names no runners", Path)
	}
	return f, nil
}

// branches reads "branches": an object from each branch's name to the
// name of its scope, 1 to 16 lowercase letters and digits, starting with a
// letter, which no other branch's shares.
func branches(v json.RawMessage) (map[string]string, error) {
	raw, err := object(v)
	if err != nil {
		return nil, fmt.Errorf("%s: branches %w", Path, err)
	}
	out := map[string]string{}
	byName := map[string]string{}
	var errs []error
	for branch, rv := range raw {
		var name string
		switch {
		case branch == "":
			errs = append(errs, fmt.Errorf("%s: branches: a branch's name is empty", Path))
		case json.Unmarshal(rv, &name) != nil || !selector.ScopeName.MatchString(name):
			errs = append(errs, fmt.Errorf("%s: branches: %s's scope must be named by 1 to 16 lowercase letters and digits, starting with a letter", Path, branch))
		case byName[name] != "":
			first, second := byName[name], branch
			if second < first {
				first, second = second, first
			}
			errs = append(errs, fmt.Errorf("%s: branches: %s and %s both name the scope %s", Path, first, second, name))
		default:
			byName[name] = branch
			out[branch] = name
		}
	}
	return out, errors.Join(errs...)
}

// ParseOverrides reads the RUNNERS variable: an object in the file's shape,
// whose values are a label or a list of labels. Any name may appear; those
// the file doesn't list are ignored later. Empty is no overrides.
func ParseOverrides(s string) (map[string]Labels, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	raw, err := object([]byte(s))
	if err != nil {
		return nil, fmt.Errorf("RUNNERS %w", err)
	}
	out := map[string]Labels{}
	var errs []error
	for name, v := range raw {
		var l Labels
		if err := json.Unmarshal(v, &l); err != nil {
			errs = append(errs, fmt.Errorf("RUNNERS: %s: %w", name, err))
			continue
		}
		out[name] = l
	}
	return out, errors.Join(errs...)
}

// object reads a JSON object, refusing a key given twice.
func object(data []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("isn't a JSON object")
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, errors.New("isn't a JSON object")
		}
		key := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, errors.New("isn't a JSON object")
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("names %q twice", key)
		}
		out[key] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, errors.New("isn't a JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("has more after its object")
	}
	return out, nil
}

// Object reads a JSON object as object does, for other inputs in the same
// style.
func Object(data []byte) (map[string]json.RawMessage, error) { return object(data) }
