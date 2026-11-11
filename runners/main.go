// Command runners is the runners action's program (specs/0001-actions.md,
// runners): it makes the labels a workflow's jobs run on, from the
// consumer's .github/runners.json at the run's commit, binding the run's
// scope with the runner service inside the organization, and falling back
// to the file's labels outside it.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/lanescape/actions/internal/actions"
	"github.com/lanescape/actions/internal/service"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, actions.Default(), service.New); err != nil {
		actions.Fail(err)
	}
}
