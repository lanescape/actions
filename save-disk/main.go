// Command save-disk is the save-disk action's program
// (specs/0001-actions.md, save-disk): it asks for the job's work disk to be
// saved when the job finishes, by writing the service's save request on
// the machine (lanescape/runners' spec 0005, Asking to save a work disk).
// Whether the disk is saved is the service's to decide.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/lanescape/runners/machine"

	"github.com/lanescape/actions/internal/actions"
	"github.com/lanescape/actions/internal/event"
	"github.com/lanescape/actions/internal/service"
	"github.com/lanescape/actions/internal/where"
)

func main() {
	if err := run(actions.Default(), service.MachineDir()); err != nil {
		actions.Fail(err)
	}
}

func run(rt *actions.Runtime, dir string) error {
	r, err := event.Load(rt.Getenv)
	if err != nil {
		return err
	}
	place := where.Of(r)
	if !place.Inside {
		rt.Logf("save-disk does nothing here: %s.", place.Why)
		return rt.Summary(place.Summary() + " save-disk did nothing.\n")
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		rt.Logf("save-disk does nothing here: this isn't one of the runner service's machines.")
		return rt.Summary(place.Summary() + " This isn't one of the runner service's machines, so save-disk did nothing.\n")
	}
	path := filepath.Join(dir, filepath.Base(machine.SaveMarker))
	// A link in its place would be followed; the request replaces it.
	_ = os.Remove(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("asking to save the work disk: %w", err)
	}
	if _, err := f.WriteString(machine.SaveRequest); err != nil {
		f.Close()
		return fmt.Errorf("asking to save the work disk: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("asking to save the work disk: %w", err)
	}
	rt.Logf("Asked the runner service to save the work disk when the job finishes. It saves it if the job succeeds and its scope and selector allow.")
	return rt.Summary(place.Summary() + " The job asked for its work disk to be saved when it finishes.\n")
}
