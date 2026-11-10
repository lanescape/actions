//go:build e2e

package service

import (
	"os"
	"time"

	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/actions/internal/backoff"
)

// In a binary built with the e2e tag, for the end-to-end tests alone, the
// service is the test service at LANESCAPE_TEST_SERVICE, the metadata
// server and the service's directory are the test's, and waits are
// milliseconds rather than seconds. No action.yml builds with that tag.

func address() string {
	if u := os.Getenv("LANESCAPE_TEST_SERVICE"); u != "" {
		return u
	}
	return endpoints.Service
}

func defaultRetry() backoff.Policy {
	p := DefaultRetry
	p.First, p.Max = time.Millisecond, 10*time.Millisecond
	return p
}

// FollowEvery is how often the image action asks how a build goes.
func FollowEvery() time.Duration { return 20 * time.Millisecond }

// MetadataServer is the test's metadata server.
func MetadataServer() string {
	if u := os.Getenv("LANESCAPE_TEST_METADATA"); u != "" {
		return u
	}
	return "http://metadata.google.internal"
}

// MachineDir is the test machine's directory.
func MachineDir() string {
	if d := os.Getenv("LANESCAPE_TEST_MACHINE_DIR"); d != "" {
		return d
	}
	return "/var/lib/lanescape-runners"
}
