// Package contract exposes Cortex through the Forge dashboard dispatcher.
package contract

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"
	"github.com/xraph/forge/extensions/dashboard/contract/loader"
)

const ContributorName = "cortex"

//go:embed manifest.yaml
var manifest []byte

// Deps lets the host supply its scope mapping and durable audit recorder.
// DefaultScope is only for an explicitly single-scope dashboard deployment.
// A present but invalid claim never falls back to it.
type Deps struct {
	Engine       *engine.Engine
	DefaultScope cortex.Scope
	ResolveScope func(context.Context, dc.Principal) (cortex.Scope, error)
	Audit        func(context.Context, AuditEvent) error
}
type AuditEvent struct {
	At         time.Time    `json:"at"`
	Subject    string       `json:"subject"`
	Scope      cortex.Scope `json:"scope"`
	Action     string       `json:"action"`
	ResourceID string       `json:"resource_id,omitempty"`
	Outcome    string       `json:"outcome"`
}
type service struct {
	deps Deps
	mu   sync.Mutex
}

func Register(d *dispatcher.Dispatcher, reg dc.Registry, wreg dc.WardenRegistry, deps Deps) error {
	if deps.Engine == nil || deps.Engine.Store() == nil {
		return fmt.Errorf("cortex contract requires an engine with a store")
	}
	if !deps.DefaultScope.IsZero() {
		if err := validateScope(deps.DefaultScope); err != nil {
			return err
		}
	}
	m, err := loader.Load(bytes.NewReader(manifest), "cortex/extension/contract/manifest.yaml")
	if err != nil {
		return err
	}
	if err = loader.Validate(m, wreg); err != nil {
		return err
	}
	if err = reg.Register(m); err != nil {
		return err
	}
	s := &service{deps: deps}
	for _, bind := range []func(*dispatcher.Dispatcher) error{s.bindAgent, s.bindPersona, s.bindSkill, s.bindTrait, s.bindBehavior, s.bindOrchestration, s.bindOperations} {
		if err = bind(d); err != nil {
			return err
		}
	}
	return nil
}
func bad(message string) error    { return &dc.Error{Code: dc.CodeBadRequest, Message: message} }
func denied(message string) error { return &dc.Error{Code: dc.CodePermissionDenied, Message: message} }
func validateScope(scope cortex.Scope) error {
	if len(scope.Levels) < 1 || len(scope.Levels) > 3 {
		return denied("Cortex requires one to three scope levels")
	}
	seen := map[string]bool{}
	for _, l := range scope.Levels {
		if strings.TrimSpace(l.Key) == "" || strings.TrimSpace(l.Value) == "" || strings.ContainsAny(l.Key+l.Value, "/=") || seen[l.Key] {
			return denied("Cortex scope contains an invalid level")
		}
		seen[l.Key] = true
	}
	return nil
}
func (s *service) context(ctx context.Context, p dc.Principal, permission string) (context.Context, error) {
	if p.User == nil || !p.User.Authenticated() {
		return ctx, &dc.Error{Code: dc.CodeUnauthenticated, Message: "Sign in to use Cortex"}
	}
	if !p.User.HasScope("cortex." + permission) {
		return ctx, denied("Cortex " + permission + " permission is required")
	}
	var scope cortex.Scope
	var err error
	if s.deps.ResolveScope != nil {
		scope, err = s.deps.ResolveScope(ctx, p)
	} else {
		scope, err = scopeFromClaims(p, s.deps.DefaultScope)
	}
	if err != nil {
		return ctx, err
	}
	if err = validateScope(scope); err != nil {
		return ctx, err
	}
	return cortex.WithPrincipal(cortex.WithScope(ctx, scope), p), nil
}
func scopeFromClaims(p dc.Principal, fallback cortex.Scope) (cortex.Scope, error) {
	// The host may name its hierarchy explicitly. Legacy tenant/app claims
	// are accepted only as an ordered tenant then app pair.
	if raw, ok := p.Claims["cortex_scope"]; ok {
		b, err := json.Marshal(raw)
		if err != nil {
			return cortex.Scope{}, denied("Invalid cortex_scope claim")
		}
		var scope cortex.Scope
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&scope); err != nil {
			return scope, denied("Invalid cortex_scope claim")
		}
		if err = validateScope(scope); err != nil {
			return scope, err
		}
		for _, l := range scope.Levels {
			if v, exists := p.Claims[l.Key+"_id"]; exists && v != l.Value {
				return scope, denied("Conflicting Cortex scope claims")
			}
		}
		// Do not silently ignore a tenant/app claim outside the supplied hierarchy.
		for _, key := range []string{"tenant", "app"} {
			if _, exists := p.Claims[key+"_id"]; exists {
				if _, ok := scope.Get(key); !ok {
					return scope, denied("Conflicting Cortex scope claims")
				}
			}
		}
		return scope, nil
	}
	scope := cortex.Scope{}
	for _, key := range []string{"tenant", "app"} {
		if raw, exists := p.Claims[key+"_id"]; exists {
			v, ok := raw.(string)
			if !ok || strings.TrimSpace(v) == "" {
				return scope, denied("Invalid " + key + "_id claim")
			}
			scope.Levels = append(scope.Levels, cortex.Level{Key: key, Value: v})
		}
	}
	if scope.IsZero() {
		return fallback, nil
	}
	return scope, nil
}
func mapError(err error) error {
	if err == nil {
		return nil
	}
	var ce *dc.Error
	if errors.As(err, &ce) {
		return ce
	}
	for _, e := range []error{cortex.ErrAgentNotFound, cortex.ErrPersonaNotFound, cortex.ErrSkillNotFound, cortex.ErrTraitNotFound, cortex.ErrBehaviorNotFound, cortex.ErrRunNotFound, cortex.ErrCheckpointNotFound, cortex.ErrSessionNotFound, cortex.ErrOrchestrationNotFound, cortex.ErrOverlayNotFound} {
		if errors.Is(err, e) {
			return &dc.Error{Code: dc.CodeNotFound, Message: "Cortex resource was not found in your scope"}
		}
	}
	if errors.Is(err, cortex.ErrAlreadyExists) || errors.Is(err, cortex.ErrInvalidState) || errors.Is(err, cortex.ErrNotSuspended) {
		return &dc.Error{Code: dc.CodeConflict, Message: err.Error()}
	}
	if errors.Is(err, cortex.ErrNoScope) {
		return denied("Cortex scope could not be resolved")
	}
	return &dc.Error{Code: dc.CodeUnavailable, Message: "Cortex could not complete the request", Retryable: true}
}
func (s *service) write(ctx context.Context, p dc.Principal, action, resource string, fn func() error) error {
	if s.deps.Audit == nil {
		return &dc.Error{Code: dc.CodeUnavailable, Message: "A durable audit recorder is required for Cortex commands"}
	}
	event := AuditEvent{At: time.Now().UTC(), Subject: p.User.Subject, Scope: cortex.ScopeFromContext(ctx), Action: action, ResourceID: resource, Outcome: "attempted"}
	if err := s.deps.Audit(ctx, event); err != nil {
		return &dc.Error{Code: dc.CodeUnavailable, Message: "Audit recording failed before the command", Retryable: true}
	}
	err := fn()
	event.At = time.Now().UTC()
	event.Outcome = "succeeded"
	if err != nil {
		event.Outcome = "failed"
	}
	if auditErr := s.deps.Audit(context.WithoutCancel(ctx), event); auditErr != nil {
		return &dc.Error{Code: dc.CodeUnavailable, Message: "The command was attempted but its audit outcome could not be saved. Review the resource before retrying.", Details: map[string]any{"command_attempted": true}}
	}
	return mapError(err)
}
