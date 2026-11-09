//go:build e2e

package service

import (
	"os"
	"time"

	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/actions/internal/backoff"
)

// In a binary built with the e2e tag, for the end-to-end tests alone, the
// service is the test service at LANESCAPE_TEST_SERVICE, and retries wait
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
