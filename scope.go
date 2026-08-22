package cortex

import (
	"context"
	"errors"
	"strings"
)

type contextKey int

const (
	tenantKey contextKey = iota
	appKey
)

// TenantFromContext extracts the tenant identifier from the context.
// Returns an empty string if no tenant is set.
func TenantFromContext(ctx context.Context) string {
	v, ok := ctx.Value(tenantKey).(string)
	if !ok {
		return ""
	}
	return v
}

// WithTenant returns a copy of ctx with the tenant identifier attached.
func WithTenant(ctx context.Context, tenant string) context.Context {
	return context.WithValue(ctx, tenantKey, tenant)
}

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

// scopeKey is the context key for Scope. It is distinct from the older
// tenantKey/appKey pair, which stays in place until Task 9 removes it.
const scopeKey contextKey = 2

// ErrNoScope is returned when an operation that requires a scope receives
// a zero one. A zero scope means the thread broke somewhere upstream, so
// failing here is preferable to querying across every tenant.
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
// string parses to the zero Scope. Segments that don't contain "=" are
// skipped rather than erroring, since a malformed stored value shouldn't
// make a read fail.
func ParseCanonical(canon string) Scope {
	if canon == "" {
		return Scope{}
	}
	parts := strings.Split(canon, "/")
	levels := make([]Level, 0, len(parts))
	for _, p := range parts {
		key, value, found := strings.Cut(p, "=")
		if !found {
			continue
		}
		levels = append(levels, Level{Key: key, Value: value})
	}
	return Scope{Levels: levels}
}

// WithScope attaches a scope to ctx.
func WithScope(ctx context.Context, s Scope) context.Context {
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
