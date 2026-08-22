package cortex

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type contextKey int

const (
	appKey contextKey = iota
	scopeKey
)

// AppFromContext extracts the app identifier from the context.
// Returns an empty string if no app is set.
func AppFromContext(ctx context.Context) string {
	v, ok := ctx.Value(appKey).(string)
	if !ok {
		return ""
	}
	return v
}

// WithApp returns a copy of ctx with the app identifier attached.
func WithApp(ctx context.Context, app string) context.Context {
	return context.WithValue(ctx, appKey, app)
}

// ErrNoScope is returned when an operation that requires a scope receives
// a zero one. A zero scope means the thread broke somewhere upstream, so
// failing here is preferable to querying across every host-defined level.
var ErrNoScope = errors.New("cortex: no scope on context")

// Level is one rung of a host-defined scope hierarchy. Cortex never
// interprets Key; it only matches on it.
type Level struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Scope is an ordered hierarchy the host defines. TwinOS uses
// workspace then project; another host might use org, team, environment.
// Order is significant: it determines which indexed column each level
// lands in, so a host must keep its ordering stable across releases.
type Scope struct {
	Levels []Level `json:"levels"`
}

func (s Scope) IsZero() bool { return len(s.Levels) == 0 }

// Get returns the value for key, and whether the level was present.
func (s Scope) Get(key string) (string, bool) {
	for _, l := range s.Levels {
		if l.Key == key {
			return l.Value, true
		}
	}
	return "", false
}

// Canonical renders the scope as "key=value/key=value", preserving order.
// It is stored alongside the indexed columns so an exact-match lookup does
// not need to compare three columns separately.
func (s Scope) Canonical() string {
	if len(s.Levels) == 0 {
		return ""
	}
	parts := make([]string, 0, len(s.Levels))
	for _, l := range s.Levels {
		parts = append(parts, l.Key+"="+l.Value)
	}
	return strings.Join(parts, "/")
}

// ParseCanonical parses a Canonical() string back into a Scope, preserving
// level order. It is the inverse of Canonical: stores that persist only
// the canonical string (or the flattened scope_l0/l1/l2 columns plus a
// scope_canon column) use this to reconstruct Scope on read. An empty
// string parses to the zero Scope.
//
// A segment that doesn't contain "=" is an error rather than something to
// skip: silently dropping it would reconstruct a scope NARROWER than what
// was actually stored, which a caller comparing it against the row's own
// scope_l0/l1/l2 columns (or using it to authorize a write) would never
// notice. Callers should treat a non-nil error here as a corrupt row —
// the same as any other scan failure — rather than falling back to
// whatever levels did parse.
func ParseCanonical(canon string) (Scope, error) {
	if canon == "" {
		return Scope{}, nil
	}
	parts := strings.Split(canon, "/")
	levels := make([]Level, 0, len(parts))
	for _, p := range parts {
		key, value, found := strings.Cut(p, "=")
		if !found {
			return Scope{}, fmt.Errorf("cortex: malformed scope_canon segment %q in %q", p, canon)
		}
		levels = append(levels, Level{Key: key, Value: value})
	}
	return Scope{Levels: levels}, nil
}

// maxScopeLevels is how many levels a Scope may carry. It mirrors
// indexedLevels in store/{postgres,sqlite,mongo}: levels beyond this land
// in the scope_extra overflow and are never matched as a predicate, even
// in exact mode, so a deeper scope would have its trailing levels
// silently accepted and then ignored by every store.
const maxScopeLevels = 3

// WithScope attaches a scope to ctx. Two shapes are refused rather than
// stored:
//
//   - A Level with an empty Key or empty Value. Either flattens to a
//     "key=" predicate that matches every row sharing that partial key —
//     a shared bucket, the exact cross-tenant hazard this phase exists to
//     close.
//   - A scope deeper than maxScopeLevels. Levels past the indexed columns
//     are written to scope_extra but never read back as a predicate, so a
//     value the caller supplied would be accepted and then silently
//     ignored on every query.
//
// Both cases return ctx unchanged rather than panicking or erroring:
// ScopeFromContext then yields the zero scope, and every store guard
// already refuses that with ErrNoScope. This mirrors the deleted
// scopeFromTenant bridge, which returned ctx unchanged for an absent
// tenant instead of manufacturing a scope for it.
func WithScope(ctx context.Context, s Scope) context.Context {
	if len(s.Levels) > maxScopeLevels {
		return ctx
	}
	for _, l := range s.Levels {
		if l.Key == "" || l.Value == "" {
			return ctx
		}
	}
	return context.WithValue(ctx, scopeKey, s)
}

// ScopeFromContext extracts the scope. A missing scope returns the zero
// value, which callers must treat as an error rather than as "match all".
func ScopeFromContext(ctx context.Context) Scope {
	v, ok := ctx.Value(scopeKey).(Scope)
	if !ok {
		return Scope{}
	}
	return v
}
