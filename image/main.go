// Command image is the image action's program (specs/0001-actions.md,
// image): it has the runner service build the image the run's commit
// defines, if it isn't built, waits for it, writing each build's log as it
// comes, and fails at once if a build fails.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/lanescape/actions/internal/actions"
	"github.com/lanescape/actions/internal/event"
	"github.com/lanescape/actions/internal/metadata"
	"github.com/lanescape/actions/internal/service"
	"github.com/lanescape/actions/internal/where"
)

func main() {
	rt := actions.Default()
	if err := run(context.Background(), rt, service.New(metadata.Server{URL: service.MetadataServer()}), service.MachineDir(),
		service.FollowEvery()); err != nil {
		actions.Fail(err)
	}
}

// name is an image's name, as lanescape/runners' spec 0005 has it.
var name = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func run(ctx context.Context, rt *actions.Runtime, svc *service.Client, machineDir string, every time.Duration) error {
	image := rt.Input("name")
	if !name.MatchString(image) || strings.HasSuffix(image, "-") {
		return fmt.Errorf("input name is %q, which isn't an image's name", image)
	}
	r, err := event.Load(rt.Getenv)
	if err != nil {
		return err
	}
	place := where.Of(r)
	if !place.Inside {
		rt.Logf("The image action does nothing here: %s.", place.Why)
		return rt.Summary(place.Summary() + " The image action did nothing.\n")
	}
	if fi, err := os.Stat(machineDir); err != nil || !fi.IsDir() {
		return errors.New("the image action must run on one of the runner service's machines, and this isn't one: " +
			"name one of its selectors in the job's runs-on")
	}
	svc.Logf = rt.Logf
	builds, err := svc.Image(ctx, image)
	if err != nil {
		return explain(image, err)
	}
	slices.SortFunc(builds, func(a, b service.Build) int { return strings.Compare(a.Architecture, b.Architecture) })
	offsets := map[string]int64{}
	// A build already failed fails the action at once, before any other
	// is followed.
	for _, b := range builds {
		if b.State == service.Failed {
			return fmt.Errorf("the image %s's build for %s failed: %s", image, b.Architecture, b.Reason)
		}
	}
	for {
		pending, more := false, false
		for i, b := range builds {
			switch b.State {
			case service.Failed:
				return fmt.Errorf("the image %s's build for %s failed: %s", image, b.Architecture, b.Reason)
			case service.Ready:
				continue
			}
			p, err := svc.Follow(ctx, b.Build, offsets[b.Build])
			if err != nil {
				return explain(image, err)
			}
			if p.Log != "" {
				writeLog(rt, fmt.Sprintf("%s %s", b.Architecture, b.Version), p.Log)
			}
			offsets[b.Build] = p.Next
			builds[i].State, builds[i].Reason = p.State, p.Reason
			if p.State == service.Failed && p.Log == "" {
				return fmt.Errorf("the image %s's build for %s failed: %s", image, b.Architecture, p.Reason)
			}
			// A build is followed until it has ended and its log is
			// drained: one that ended behind its output has more to give.
			if p.State == service.Building || p.Log != "" {
				pending = true
				builds[i].State = service.Building
			}
			more = more || p.Log != ""
		}
		if !pending {
			break
		}
		if more {
			// Its log has more to give now: no wait.
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
	var lines []string
	for _, b := range builds {
		lines = append(lines, fmt.Sprintf("- `%s`: `%s`", b.Architecture, b.Version))
		rt.Logf("The image %s is ready for %s, at version %s.", image, b.Architecture, b.Version)
	}
	return rt.Summary(fmt.Sprintf("%s\n\nThe image `%s` is ready:\n\n%s\n", place.Summary(), image, strings.Join(lines, "\n")))
}

// explain is an error of the service's, for the job's log.
func explain(image string, err error) error {
	var refused *service.Refused
	if errors.As(err, &refused) {
		return fmt.Errorf("the runner service refused the image %s: %s", image, refused.Reason)
	}
	return fmt.Errorf("the image %s: %w", image, err)
}

// writeLog writes a part of a build's log as plain text, in a group named
// by the architecture and version: workflow commands are stopped with a
// token made at random for the part, and resumed after it, so nothing the
// consumer's scripts printed can mask, annotate or set anything in the
// job.
func writeLog(rt *actions.Runtime, group, part string) {
	var b [16]byte
	_, _ = rand.Read(b[:])
	token := "lanescape-" + hex.EncodeToString(b[:])
	fmt.Fprintf(rt.Stdout, "::group::%s\n::stop-commands::%s\n", group, token)
	fmt.Fprint(rt.Stdout, part)
	if !strings.HasSuffix(part, "\n") {
		fmt.Fprintln(rt.Stdout)
	}
	fmt.Fprintf(rt.Stdout, "::%s::\n::endgroup::\n", token)
}
