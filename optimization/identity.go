package optimization

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/xraph/cortex"
)

// ScopeKey returns an unambiguous exact key supported by every Cortex backend.
func ScopeKey(scope cortex.Scope) (string, error) {
	if len(scope.Levels) < 1 || len(scope.Levels) > 3 {
		return "", fmt.Errorf("%w: scope requires one to three levels", ErrInvalidSpec)
	}
	seen := make(map[string]bool, len(scope.Levels))
	for _, level := range scope.Levels {
		for _, part := range []string{level.Key, level.Value} {
			if part == "" || strings.TrimSpace(part) != part || strings.ContainsAny(part, "/=") || !utf8.ValidString(part) || strings.ContainsFunc(part, unicode.IsControl) {
				return "", fmt.Errorf("%w: malformed scope level", ErrInvalidSpec)
			}
		}
		if seen[level.Key] {
			return "", fmt.Errorf("%w: duplicate scope key", ErrInvalidSpec)
		}
		seen[level.Key] = true
	}
	return scope.Canonical(), nil
}
