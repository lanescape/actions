package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/lanescape/actions/internal/actions"
	"github.com/lanescape/actions/internal/event"
	"github.com/lanescape/actions/internal/github"
	"github.com/lanescape/actions/internal/oidc"
	"github.com/lanescape/actions/internal/runnersfile"
	"github.com/lanescape/actions/internal/selector"
	"github.com/lanescape/actions/internal/service"
	"github.com/lanescape/actions/internal/where"
)

// disk is an entry of the disks input.
type disk struct {
	Family string
	Runner string
	Spot   bool
	// From are the files the disk's contents depend on; nil if not given.
	From []string
	// Key is the key given, or made from From, or selector.NoKey.
	Key string
}

// parseDisks reads the disks input: an object from each family to its
// entry, {"runner", "spot", "from" or "key"}, nothing else.
func parseDisks(s string) ([]disk, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	raw, err := runnersfile.Object([]byte(s))
	if err != nil {
		return nil, fmt.Errorf("the disks input %w", err)
	}
	var out []disk
	var errs []error
	for family, v := range raw {
		if !selector.Family.MatchString(family) {
			errs = append(errs, fmt.Errorf("disks: %q isn't a family: 1 to 32 lowercase letters, digits and hyphens, starting with a letter", family))
			continue
		}
		var e struct {
			Runner string    `json:"runner"`
			Spot   bool      `json:"spot"`
			From   *[]string `json:"from"`
			Key    *string   `json:"key"`
		}
		// Go matches fields without regard to case, so the names are
		// checked first, exactly.
		fields, err := runnersfile.Object(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("disks: %s %w", family, err))
			continue
		}
		bad := false
		for name, raw := range fields {
			if name != "runner" && name != "spot" && name != "from" && name != "key" {
				errs = append(errs, fmt.Errorf("disks: %s: unknown field %q; an entry has runner, spot, and from or key", family, name))
				bad = true
			} else if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				errs = append(errs, fmt.Errorf("disks: %s: %s is null; give it a value, or leave it out", family, name))
				bad = true
			}
		}
		_, hasFrom := fields["from"]
		_, hasKey := fields["key"]
		if hasFrom && hasKey {
			errs = append(errs, fmt.Errorf("disks: %s gives both from and key; give one", family))
			bad = true
		}
		if bad {
			continue
		}
		if err := json.NewDecoder(bytes.NewReader(v)).Decode(&e); err != nil {
			errs = append(errs, fmt.Errorf("disks: %s: %w", family, err))
			continue
		}
		d := disk{Family: family, Runner: e.Runner, Spot: e.Spot, Key: selector.NoKey}
		if e.Runner == "" {
			errs = append(errs, fmt.Errorf("disks: %s names no runner", family))
		}
		switch {
		case e.From != nil:
			if len(*e.From) == 0 {
				errs = append(errs, fmt.Errorf("disks: %s: from lists no files; list some, or leave it out", family))
			}
			seen := map[string]bool{}
			for _, p := range *e.From {
				if !github.ValidPath(p) {
					errs = append(errs, fmt.Errorf("disks: %s: from: %q isn't a path from the repository's root", family, p))
				} else if seen[p] {
					errs = append(errs, fmt.Errorf("disks: %s: from lists %s twice", family, p))
				}
				seen[p] = true
			}
			d.From = *e.From
			d.Key = ""
		case e.Key != nil:
			if !selector.Key.MatchString(*e.Key) {
				errs = append(errs, fmt.Errorf("disks: %s: key %q isn't 20 lowercase hex digits", family, *e.Key))
			}
			d.Key = *e.Key
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Family < out[j].Family })
	return out, errors.Join(errs...)
}

// newServiceFunc makes the service's client from the job's tokens.
type newServiceFunc func(service.Tokens) *service.Client

func run(ctx context.Context, rt *actions.Runtime, newService newServiceFunc) error {
	r, err := event.Load(rt.Getenv)
	if err != nil {
		return err
	}
	place := where.Of(r)
	rt.Logf("%s", place.Summary())

	disks, err := parseDisks(rt.Input("disks"))
	if err != nil {
		return err
	}
	token := rt.Input("token")
	if token == "" {
		return errors.New("the token input is empty; it defaults to the job's own token, github.token")
	}
	rt.Mask(token)
	gh := &github.Client{BaseURL: r.APIURL, Token: token}

	data, err := gh.File(ctx, r.Repository, runnersfile.Path, r.SHA)
	if errors.Is(err, github.ErrNotFound) {
		return fmt.Errorf("%s isn't in %s at the run's commit, %s, or the job can't read it; it needs contents: read", runnersfile.Path, r.Repository, r.SHA)
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", runnersfile.Path, err)
	}
	file, err := runnersfile.Parse(data)
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range disks {
		if _, ok := file.Fallbacks[d.Runner]; !ok {
			errs = append(errs, fmt.Errorf("disks: %s's runner, %q, isn't in %s", d.Family, d.Runner, runnersfile.Path))
		}
		if _, ok := file.Fallbacks[d.Family]; ok {
			errs = append(errs, fmt.Errorf("disks: the family %s is also a runner's name, and the map can't have both", d.Family))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}

	// The commit the job builds: the run's own, or a contribution's.
	var contribution *service.BindRequest
	buildRepo, buildSHA := r.Repository, r.SHA
	if r.Name == "pull_request_target" {
		n, head, sha, err := r.Contribution()
		if err != nil {
			return err
		}
		contribution = &service.BindRequest{PullRequest: n, HeadRepositoryID: head.ID, HeadSHA: sha}
		buildRepo, buildSHA = head.FullName, sha
	}
	for i, d := range disks {
		if d.From == nil {
			continue
		}
		files := make([]selector.File, 0, len(d.From))
		for _, p := range d.From {
			b, err := gh.File(ctx, buildRepo, p, buildSHA)
			if errors.Is(err, github.ErrNotFound) {
				return fmt.Errorf("disks: %s: %s isn't in %s at %s", d.Family, p, buildRepo, buildSHA)
			}
			if err != nil {
				return fmt.Errorf("disks: %s: reading %s: %w", d.Family, p, err)
			}
			files = append(files, selector.File{Path: p, Contents: b})
		}
		disks[i].Key = selector.DiskKey(d.Family, d.Runner, files)
	}

	out := map[string]runnersfile.Labels{}
	var notes []string
	if place.Inside {
		tokens, err := oidc.FromEnv(rt.Getenv, nil)
		if err != nil {
			return err
		}
		tokens.Mask = rt.Mask
		svc := newService(tokens)
		svc.Logf = rt.Logf
		scope, err := svc.Bind(ctx, contribution)
		if err != nil {
			return err
		}
		notes = append(notes, fmt.Sprintf("The run's scope is `%s`.", scope))
		for name := range file.Fallbacks {
			out[name] = runnersfile.Label(selector.Runner(name, scope))
		}
		for _, d := range disks {
			out[d.Family] = runnersfile.Label(selector.Disk(d.Runner, d.Spot, d.Family, d.Key, scope))
		}
	} else {
		var overrides map[string]runnersfile.Labels
		switch fallbacks := rt.Input("fallbacks"); {
		case place.ForkIntoOrganization:
			if fallbacks != "" {
				notes = append(notes, "The `fallbacks` input is ignored: in a fork's pull request it's the organization's `RUNNERS`, not the fork's.")
			}
		default:
			if overrides, err = runnersfile.ParseOverrides(fallbacks); err != nil {
				return fmt.Errorf("the fallbacks input, the RUNNERS variable: %w", err)
			}
		}
		for name, l := range file.Fallbacks {
			if o, ok := overrides[name]; ok {
				l = o
			}
			out[name] = l
		}
		for _, d := range disks {
			out[d.Family] = out[d.Runner]
		}
	}

	m, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if err := rt.SetOutput("map", string(m)); err != nil {
		return err
	}
	return rt.Summary(summary(place, notes, out))
}

func summary(place where.Place, notes []string, out map[string]runnersfile.Labels) string {
	var b strings.Builder
	b.WriteString("## Runners\n\n")
	b.WriteString(place.Summary() + "\n")
	for _, n := range notes {
		b.WriteString("\n" + n + "\n")
	}
	names := make([]string, 0, len(out))
	for n := range out {
		names = append(names, n)
	}
	sort.Strings(names)
	b.WriteString("\n| Name | Runs on |\n| --- | --- |\n")
	for _, n := range names {
		fmt.Fprintf(&b, "| `%s` | `%s` |\n", n, strings.ReplaceAll(out[n].String(), "|", `\|`))
	}
	b.WriteString("\nThese are the map's only names. A job that asks for any other gets an empty label, which GitHub refuses.\n\n")
	return b.String()
}
