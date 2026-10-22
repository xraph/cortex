// Package memory provides process-local optimization storage for tests and fixtures.
// It cannot advertise durable recovery readiness.
package memory

import (
	"time"

	"github.com/xraph/cortex/optimization/store/internal/state"
)

// Store keeps detached process-local state behind the shared storage rules.
type Store = state.Store

// Option configures the process-local backend.
type Option = state.Option

// WithClock supplies trusted timestamps for transactions and lease tests.
func WithClock(now func() time.Time) Option { return state.WithClock(now) }

// New returns an empty, non-durable backend.
func New(options ...Option) (*Store, error) {
	return state.New(options...)
}
