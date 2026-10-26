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

	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"
	"github.com/xraph/forge/extensions/dashboard/contract/loader"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/learning"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
)

const ContributorName = "cortex"

//go:embed manifest.yaml
var manifest []byte

// Deps lets the host supply its scope mapping and durable audit recorder.
// DefaultScope is only for an explicitly single-scope dashboard deployment.
// A present but invalid claim never falls back to it.
type Deps struct {
	Engine         *engine.Engine
	AdaptiveReview *engine.AdaptiveReviewer
	Learning       *learning.Guard
	// ExecutionBudget supplies host-owned cumulative limits before preparation.
	// The callback receives a detached effective agent. Client limits can only
	// narrow its result; clients cannot supply their own deadline authority.
	ExecutionBudget func(context.Context, *agent.Config) (execution.Budget, error)
	// AgentMutationFence serializes dashboard agent writes with host policy reads.
	// The host must hold current authority until its synchronous callback returns.
	// Other agent writers must participate in the same host fence.
	AgentMutationFence func(context.Context, func(context.Context) error) error
	Optimization       *optimization.Optimizer
	DefaultScope       cortex.Scope
	ResolveScope       func(context.Context, dc.Principal) (cortex.Scope, error)
	Audit              func(context.Context, AuditEvent) error
	ExecutionLabel     string
	Catalogs           map[string]CatalogQuery
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
	deps     Deps
	mu       sync.Mutex
	streamMu sync.Mutex
	streams  map[string]*liveRun
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
	if checkErr := loader.Validate(m, wreg); checkErr != nil {
		return checkErr
	}
	if checkErr := reg.Register(m); checkErr != nil {
		return checkErr
	}
	s := &service{deps: deps}
	for _, bind := range []func(*dispatcher.Dispatcher) error{s.bindAgent, s.bindPersona, s.bindSkill, s.bindTrait, s.bindBehavior, s.bindOrchestration, s.bindOperations, s.bindExecution, s.bindRuntime, s.bindStream, s.bindReplay, s.bindResultContent, s.bindAdvanced, s.bindReferences, s.bindStats, s.bindToolUsage, s.bindOptimization, s.bindAdaptive} {
		if checkErr := bind(d); checkErr != nil {
			return checkErr
		}
	}
	return nil
}
func bad(message string) error    { return &dc.Error{Code: dc.CodeBadRequest, Message: message} }
func denied(message string) error { return &dc.Error{Code: dc.CodePermissionDenied, Message: message} }
func validateScope(scope cortex.Scope) error {
	if cortex.ValidateScope(scope) != nil {
		return denied("Cortex scope is invalid or unsupported")
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
	if checkErr := validateScope(scope); checkErr != nil {
		return ctx, checkErr
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
		if checkErr := validateScope(scope); checkErr != nil {
			return scope, checkErr
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
	if failure, ok := run.DescribeVerificationFailure(err, nil); ok {
		return verificationError(failure)
	}
	var ce *dc.Error
	if errors.As(err, &ce) {
		return ce
	}
	switch {
	case errors.Is(err, cortex.ErrMaxStepsReached), errors.Is(err, cortex.ErrBudgetExhausted):
		return &dc.Error{Code: dc.CodeConflict, Message: "The run exhausted its execution budget. Review its saved outcome."}
	case errors.Is(err, safety.ErrBlocked):
		return &dc.Error{Code: dc.CodePermissionDenied, Message: safety.ErrBlocked.Error()}
	case errors.Is(err, safety.ErrLimit):
		return &dc.Error{Code: dc.CodeBadRequest, Message: safety.ErrLimit.Error()}
	case errors.Is(err, safety.ErrUnavailable):
		return &dc.Error{Code: dc.CodeUnavailable, Message: safety.ErrUnavailable.Error(), Retryable: true}
	}
	if errors.Is(err, cortex.ErrRunConflict) {
		return &dc.Error{Code: dc.CodeConflict, Message: cortex.ErrRunConflict.Error()}
	}
	for _, e := range []error{cortex.ErrAgentNotFound, cortex.ErrPersonaNotFound, cortex.ErrSkillNotFound, cortex.ErrTraitNotFound, cortex.ErrBehaviorNotFound, cortex.ErrRunNotFound, cortex.ErrCheckpointNotFound, cortex.ErrSessionNotFound, cortex.ErrOrchestrationNotFound, cortex.ErrOverlayNotFound, cortex.ErrOrchestrationRunNotFound, a2a.ErrConversationNotFound, a2a.ErrMessageNotFound} {
		if errors.Is(err, e) {
			return &dc.Error{Code: dc.CodeNotFound, Message: "Cortex resource was not found in your scope"}
		}
	}
	if errors.Is(err, cortex.ErrAlreadyExists) || errors.Is(err, cortex.ErrInvalidState) || errors.Is(err, cortex.ErrNotSuspended) || errors.Is(err, cortex.ErrRequiresApproval) || errors.Is(err, engine.ErrNotAgentReplyResumable) {
		return &dc.Error{Code: dc.CodeConflict, Message: err.Error()}
	}
	if errors.Is(err, cortex.ErrResultsMismatch) {
		return bad(err.Error())
	}
	if errors.Is(err, cortex.ErrNoScope) || errors.Is(err, cortex.ErrInvalidScope) || errors.Is(err, cortex.ErrUnsupportedScopeDepth) {
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
