// Command save-disk is the save-disk action's program
// (specs/0001-actions.md, save-disk): it asks the runner service to save
// the job's work disk when the job finishes, and says whether the run's
// scope saves. Whether the disk is saved is still the service's to decide.
package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/lanescape/actions/internal/actions"
	"github.com/lanescape/actions/internal/event"
	"github.com/lanescape/actions/internal/metadata"
	"github.com/lanescape/actions/internal/service"
	"github.com/lanescape/actions/internal/where"
)

func main() {
	md := metadata.Server{URL: service.MetadataServer()}
	if err := run(context.Background(), actions.Default(), md, service.New(md)); err != nil {
		actions.Fail(err)
	}
}

// machine tells whether this is one of the service's machines.
type machine interface {
	OnService(ctx context.Context) bool
}

func run(ctx context.Context, rt *actions.Runtime, m machine, svc *service.Client) error {
	r, err := event.Load(rt.Getenv)
	if err != nil {
		return err
	}
	place := where.Of(r)
	if !place.Inside {
		rt.Logf("save-disk does nothing here: %s.", place.Why)
		return rt.Summary("## Saving the work disk\n\n" + place.Summary() + " save-disk did nothing.\n\n")
	}
	if !m.OnService(ctx) {
		rt.Logf("save-disk does nothing here: this isn't one of the runner service's machines.")
		return rt.Summary("## Saving the work disk\n\n" + place.Summary() +
			" This isn't one of the runner service's machines, so save-disk did nothing.\n\n")
	}
	svc.Logf = rt.Logf
	s, err := svc.Save(ctx)
	if err != nil {
		var refused *service.Refused
		if errors.As(err, &refused) {
			return fmt.Errorf("the runner service refused to save the work disk: %s", refused.Reason)
		}
		return fmt.Errorf("asking to save the work disk: %w", err)
	}
	if !s.Saves {
		rt.Logf("The run's scope saves nothing, so the work disk won't be saved: %s.", s.Reason)
		return rt.Summary(fmt.Sprintf("## Saving the work disk\n\n%s\n\nThe job asked for its work disk to be saved, "+
			"but the run's scope saves nothing, so it won't be: %s.\n\n", place.Summary(), s.Reason))
	}
	rt.Logf("Asked the runner service to save the work disk when the job finishes. It saves it if the job succeeds.")
	return rt.Summary(fmt.Sprintf("## Saving the work disk\n\n%s\n\nThe job asked for its work disk to be saved, "+
		"and the run's scope saves, so the runner service saves it when the job finishes, if the job succeeds.\n\n", place.Summary()))
}
