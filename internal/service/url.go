//go:build !e2e

package service

import (
	"time"

	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/actions/internal/backoff"
)

// address is always endpoints.Service in the actions' own builds.
func address() string { return endpoints.Service }

func defaultRetry() backoff.Policy { return DefaultRetry }

// FollowEvery is how often the image action asks how a build goes.
func FollowEvery() time.Duration { return 5 * time.Second }

// MetadataServer is the machine's metadata server.
func MetadataServer() string { return "http://metadata.google.internal" }

// MachineDir is the service's directory on its machines, where a job asks
// for its work disk to be saved (lanescape/runners' spec 0005).
func MachineDir() string { return "/var/lib/lanescape-runners" }
