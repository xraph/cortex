package coordination

import (
	"slices"

	"github.com/xraph/cortex"
)

func freezeDiscoveryScopes(scopes []cortex.Scope) ([]cortex.Scope, error) {
	if len(scopes) < 1 || len(scopes) > 256 {
		return nil, ErrInvalidScheduler
	}
	detached := make([]cortex.Scope, 0, len(scopes))
	seen := map[string]bool{}
	remaining := 2 << 20
	for _, scope := range scopes {
		if cortex.ValidateScope(scope) != nil {
			return nil, ErrInvalidScheduler
		}
		for _, level := range scope.Levels {
			remaining -= len(level.Key) + len(level.Value) + 32
			if remaining < 0 {
				return nil, ErrInvalidScheduler
			}
		}
		key := scope.Canonical()
		if seen[key] {
			return nil, ErrInvalidScheduler
		}
		seen[key] = true
		detached = append(detached, cortex.Scope{Levels: slices.Clone(scope.Levels)})
	}
	return detached, nil
}
