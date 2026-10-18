package cortex

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrInvalidScope identifies an ambiguous or unsupported scope identity.
	ErrInvalidScope = errors.New("cortex: invalid scope")
	// ErrUnsupportedScopeDepth means storage cannot enforce every level.
	ErrUnsupportedScopeDepth = errors.New("cortex: scope exceeds three supported levels")
)

// ValidateScope checks the identity the built-in stores can enforce. Keys are
// unique and ordered. The current canonical codec reserves slash and equals.
func ValidateScope(s Scope) error {
	if s.IsZero() {
		return ErrNoScope
	}
	if len(s.Levels) > maxScopeLevels {
		return fmt.Errorf("%w: %w", ErrInvalidScope, ErrUnsupportedScopeDepth)
	}
	seen := make(map[string]bool, len(s.Levels))
	for _, level := range s.Levels {
		if strings.TrimSpace(level.Key) == "" || strings.TrimSpace(level.Value) == "" || strings.ContainsAny(level.Key+level.Value, "/=") || seen[level.Key] {
			return ErrInvalidScope
		}
		seen[level.Key] = true
	}
	return nil
}

// RequireScope returns detached, validated request identity. Invalid attached
// scopes mask their parent and retain a typed error for boundary adapters.
func RequireScope(ctx context.Context) (Scope, error) {
	s, ok := ctx.Value(scopeKey).(Scope)
	if !ok {
		return Scope{}, ErrNoScope
	}
	if err := ValidateScope(s); err != nil {
		return Scope{}, err
	}
	return Scope{Levels: append([]Level(nil), s.Levels...)}, nil
}

// DecodeStoredScope checks that canonical and indexed identity agree. Overflow
// rows must be inventoried and migrated by an operator, never truncated on read.
func DecodeStoredScope(canonical string, indexed [3]string, hasExtra bool) (Scope, error) {
	scope, err := ParseCanonical(canonical)
	if err != nil {
		return Scope{}, err
	}
	if err := ValidateScope(scope); err != nil {
		return Scope{}, err
	}
	if hasExtra {
		return Scope{}, fmt.Errorf("%w: %w", ErrInvalidScope, ErrUnsupportedScopeDepth)
	}
	for i, column := range indexed {
		want := ""
		if i < len(scope.Levels) {
			want = scope.Levels[i].Key + "=" + scope.Levels[i].Value
		}
		if column != want {
			return Scope{}, ErrInvalidScope
		}
	}
	return scope, nil
}
