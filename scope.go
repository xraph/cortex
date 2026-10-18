package cortex

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type contextKey int

const (
	scopeKey contextKey = iota
	principalKey
)

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

// Covers reports whether s is other, or an ancestor of it: every level s
// carries appears in other at the same position, with the same key and
// value. A scope covers itself.
//
// This asks "is s somewhere in other's own ancestry", which is a
// different question from the prefix matching the stores do on a list.
// Prefix matching widens DOWNWARD, so a workspace filter reaches every
// project inside it. Covers reads UPWARD, which is what a project-level
// run needs when it wants to know what its workspace configured.
func (s Scope) Covers(other Scope) bool {
	if len(s.Levels) > len(other.Levels) {
		return false
	}
	for i, lvl := range s.Levels {
		if other.Levels[i] != lvl {
			return false
		}
	}
	return true
}

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
	scope := Scope{Levels: levels}
	if err := ValidateScope(scope); err != nil {
		return Scope{}, err
	}
	return scope, nil
}

// maxScopeLevels matches the indexed identity supported by every backend.
// Raising it requires a versioned codec and matching predicates in all stores.
const maxScopeLevels = 3

// WithScope attaches a detached scope, including an invalid request. Invalid
// requests mask any parent authority; RequireScope reports the validation error.
func WithScope(ctx context.Context, s Scope) context.Context {
	return context.WithValue(ctx, scopeKey, Scope{Levels: append([]Level(nil), s.Levels...)})
}

// ScopeFromContext returns detached valid identity, or zero for missing or
// invalid scope. Use RequireScope at boundaries to preserve the typed error.
func ScopeFromContext(ctx context.Context) Scope {
	scope, err := RequireScope(ctx)
	if err != nil {
		return Scope{}
	}
	return scope
}

// WithPrincipal attaches the host's caller identity to ctx. Cortex never
// interprets this value — it only carries it from wherever a host
// authenticates a request through to Subject.Principal, for a
// ToolAuthorizer to interpret however its host defines identity.
//
// It travels on the context, alongside Scope, rather than through
// RunOverrides: both are ambient request identity with the same
// lifetime, and RunOverrides mirrors a JSON wire type that either can't
// carry an uninterpreted any or would force cortex to start interpreting
// it. Dispatch also has no overrides parameter, so a struct field would
// leave direct dispatch permanently principal-less.
func WithPrincipal(ctx context.Context, principal any) context.Context {
	return context.WithValue(ctx, principalKey, principal)
}

// PrincipalFromContext extracts the caller identity WithPrincipal
// attached. A missing principal returns nil, mirroring ScopeFromContext's
// zero-value-on-absence shape rather than panicking.
func PrincipalFromContext(ctx context.Context) any {
	return ctx.Value(principalKey)
}
