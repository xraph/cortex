package cortex

import "context"

// TenantFunc names the tenant an external service (a model gateway, a
// safety engine, a knowledge store) should bill and isolate a call under,
// given the scope the call runs in. Adapters that talk to such a service
// take one, so a host can map its own scope to the service's tenant IDs.
// It returns ErrNoScope, or an error wrapping it, when the scope names no
// tenant.
type TenantFunc func(ctx context.Context, s Scope) (string, error)

// RootTenant is the TenantFunc adapters use by default: the value of the
// scope's first level, which for most hosts is the organization or
// workspace. It returns ErrNoScope for a zero scope.
func RootTenant(_ context.Context, s Scope) (string, error) {
	if s.IsZero() || s.Levels[0].Value == "" {
		return "", ErrNoScope
	}
	return s.Levels[0].Value, nil
}
