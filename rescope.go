package cortex

import (
	"context"
	"errors"
	"fmt"
)

// ErrNoRescoper is returned when a migration finds rows carrying no scope
// and no Rescoper was supplied. Cortex cannot know what a host's app and
// tenant strings meant, so it refuses to guess and aborts instead.
var ErrNoRescoper = errors.New("cortex: unscoped rows present and no rescoper supplied")

// Rescoper maps a row's legacy identifiers onto a Scope during the v1.8.0
// upgrade. It is called once per distinct (appID, tenantID) pair, not once
// per row, so an expensive implementation is fine.
type Rescoper interface {
	Rescope(ctx context.Context, appID, tenantID string) (Scope, error)
}

// MigrateOptions carries everything Migrate needs beyond a context.
type MigrateOptions struct {
	Rescoper Rescoper
}

// MigrateOption configures a migration run.
type MigrateOption func(*MigrateOptions)

// WithRescoper supplies the mapping from legacy identifiers to a Scope.
func WithRescoper(r Rescoper) MigrateOption {
	return func(o *MigrateOptions) { o.Rescoper = r }
}

// ApplyMigrateOptions folds the variadic options into a value. A zero
// MigrateOptions is valid: a fresh install has no unscoped rows and so
// never calls the rescoper.
func ApplyMigrateOptions(opts ...MigrateOption) MigrateOptions {
	var o MigrateOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// ValidateRescopedScope applies shared validation to an operator's legacy mapping.
func ValidateRescopedScope(s Scope) error {
	if err := ValidateScope(s); err != nil {
		return fmt.Errorf("cortex: invalid rescoped identity: %w", err)
	}
	return nil
}
