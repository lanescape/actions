//go:build !e2e

package service

import (
	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/actions/internal/backoff"
)

// address is always endpoints.Service in the actions' own builds.
func address() string { return endpoints.Service }

func defaultRetry() backoff.Policy { return DefaultRetry }
