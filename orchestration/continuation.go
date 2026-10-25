package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
)

// Continuation freezes the strategy, original input and host execution snapshot.
// Payload is private host data, not authority. Hosts must revalidate it on resume.
type Continuation struct {
	SchemaVersion   int                `json:"schema_version"`
	OrchestrationID id.OrchestrationID `json:"orchestration_id"`
	Scope           cortex.Scope       `json:"scope"`
	Config          Config             `json:"config"`
	Input           string             `json:"input"`
	Payload         []byte             `json:"payload"`
}

// Freeze detaches a bounded continuation and checks the executable roster.
func (c Continuation) Freeze() (Continuation, string, error) {
	if c.SchemaVersion != 1 || c.OrchestrationID.IsNil() || c.OrchestrationID.Prefix() != id.PrefixOrchestration || cortex.ValidateScope(c.Scope) != nil || c.Scope.Canonical() != c.Config.Scope.Canonical() {
		return Continuation{}, "", cortex.ErrInvalidContinuation
	}
	if _, err := ExecutionRoster(&c.Config); err != nil {
		return Continuation{}, "", err
	}
	data, err := json.Marshal(c)
	if err != nil || len(data) > 16<<20 {
		return Continuation{}, "", cortex.ErrInvalidContinuation
	}
	var out Continuation
	if json.Unmarshal(data, &out) != nil {
		return Continuation{}, "", cortex.ErrInvalidContinuation
	}
	return out, evidence.Digest(data), nil
}

// ContinuationStore retains immutable strategy snapshots and completed results.
// Identical snapshot/result writes may succeed; neither grants dispatch rights.
type ContinuationStore interface {
	InvocationStore
	PutOrchestrationContinuation(context.Context, Continuation) error
	GetOrchestrationContinuation(context.Context, id.OrchestrationID) (Continuation, error)
	PutOrchestrationInvocationResult(context.Context, InvocationIntent, AgentResult) error
	GetOrchestrationInvocationResult(context.Context, InvocationIntent) (*AgentResult, error)
}

// ContinuationRunStore claims a waiting parent once before another strategy pass.
// A lost acknowledgment requires reconciliation; a read is not a replacement claim.
type ContinuationRunStore interface {
	RunStore
	ChildRunStore
	ClaimOrchestrationContinuation(context.Context, id.OrchestrationID) (*Run, error)
}

// ChildObserver reads an existing child without starting or resuming it. Return
// ErrOrchestrationWaiting only for a retained nonterminal child; mixed failures
// remain failures. The host authenticates child provenance and current authority.
type ChildObserver interface {
	ObserveOrchestrationChild(context.Context, InvocationRecord) (*AgentResult, error)
}

// NewContinuableService uses reserved execution and durable strategy replay.
// It requires both continuation capabilities from runs before starting work.
func NewContinuableService(runner ReservedAgentRunner, observer ChildObserver, runs RunStore, hooks HookEmitter) *Service {
	s := NewReservedService(runner, runs, hooks)
	s.observer = observer
	return s
}

// StartContinuation persists the frozen snapshot before creating the parent.
func (s *Service) StartContinuation(ctx context.Context, c Continuation) (*Run, error) {
	storage, ok := s.runs.(ContinuationStore)
	_, claims := s.runs.(ContinuationRunStore)
	if !ok || !claims || hostport.IsNil(s.observer) {
		return nil, cortex.ErrOrchestrationContinuationUnavailable
	}
	frozen, _, err := c.Freeze()
	if err != nil {
		return nil, err
	}
	if putErr := storage.PutOrchestrationContinuation(ctx, frozen); putErr != nil {
		return nil, putErr
	}
	return s.RunConfigWithID(ctx, frozen.Config, frozen.Input, frozen.OrchestrationID)
}

// ResumeContinuation replays saved results and admits only previously absent
// invocation slots. It never invokes an existing reserved child a second time.
func (s *Service) ResumeContinuation(ctx context.Context, team id.OrchestrationID) (*Run, error) {
	storage, ok := s.runs.(ContinuationStore)
	claims, claimOK := s.runs.(ContinuationRunStore)
	if !ok || !claimOK || hostport.IsNil(s.observer) {
		return nil, cortex.ErrOrchestrationContinuationUnavailable
	}
	c, err := storage.GetOrchestrationContinuation(ctx, team)
	if err != nil {
		return nil, err
	}
	frozen, _, freezeErr := c.Freeze()
	if freezeErr != nil || frozen.OrchestrationID != team || frozen.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return nil, cortex.ErrInvalidContinuation
	}
	c = frozen
	rec, err := claims.ClaimOrchestrationContinuation(ctx, team)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, cortex.ErrInvalidContinuation
	}
	if rec.Input != c.Input || rec.Strategy != c.Config.Strategy || rec.ConfigID != c.Config.ID {
		completed := nowUTC()
		rec.Status = StatusFailed
		rec.Error = cortex.ErrInvalidContinuation.Error()
		rec.Output = ""
		rec.CompletedAt = &completed
		return rec, errors.Join(cortex.ErrInvalidContinuation, persistOrchestrationRun(ctx, s.runs, rec))
	}
	return s.executeConfig(ctx, c.Config, c.Input, rec, true)
}

// onlyWaiting refuses to mistake a mixed parallel failure for a suspension.
func onlyWaiting(err error) bool {
	if err == nil {
		return false
	}
	if children, ok := err.(interface{ Unwrap() []error }); ok {
		found := false
		for _, child := range children.Unwrap() {
			if child != nil {
				found = true
				if !onlyWaiting(child) {
					return false
				}
			}
		}
		return found
	}
	if child, ok := err.(interface{ Unwrap() error }); ok {
		return onlyWaiting(child.Unwrap())
	}
	return errors.Is(err, cortex.ErrOrchestrationWaiting)
}

// ValidateResult checks the bounded successful result shape before replay storage.
func (intent InvocationIntent) ValidateResult(result *AgentResult) error {
	if result == nil || result.Err != nil || result.RunID.IsNil() || result.RunID.Prefix() != id.PrefixAgentRun || result.AgentName != intent.AgentName || strings.TrimSpace(result.Output) == "" || len(result.Output) > 4<<20 {
		return cortex.ErrRunIncomplete
	}
	return nil
}

func (r *persistingRunner) replay(ctx context.Context, name, input string, opts *RunOpts) (out *AgentResult, replayed bool, err error) {
	if r.observer == nil {
		return nil, false, nil
	}
	storage, ok := r.runs.(ContinuationStore)
	if !ok {
		return nil, true, cortex.ErrOrchestrationContinuationUnavailable
	}
	intent, err := invocationIntent(ctx, r.record.ID, name, input, opts)
	if err != nil {
		return nil, true, err
	}
	retained, err := storage.GetOrchestrationInvocation(ctx, r.record.ID, intent.Slot)
	if errors.Is(err, cortex.ErrOrchestrationInvocationNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	_, want, err := intent.Freeze()
	_, got, readErr := retained.Intent.Freeze()
	if err != nil || readErr != nil || want != got || retained.RunID.IsNil() {
		return nil, true, cortex.ErrInvalidContinuation
	}
	result, err := storage.GetOrchestrationInvocationResult(ctx, intent)
	if err != nil {
		return nil, true, err
	}
	observed, observeErr := r.observer.ObserveOrchestrationChild(ctx, retained)
	if observeErr != nil {
		return observed, true, observeErr
	}
	if result != nil {
		if intent.ValidateResult(observed) != nil || observed.RunID != result.RunID || observed.AgentName != result.AgentName || observed.Output != result.Output {
			return nil, true, cortex.ErrInvalidContinuation
		}
		if intent.ValidateResult(result) != nil || result.RunID != retained.RunID {
			return nil, true, cortex.ErrInvalidContinuation
		}
		return result, true, nil
	}
	result = observed
	if intent.ValidateResult(result) != nil || result.RunID != retained.RunID {
		return nil, true, cortex.ErrRunIncomplete
	}
	if putErr := storage.PutOrchestrationInvocationResult(ctx, intent, *result); putErr != nil {
		return result, true, putErr
	}
	return result, true, nil
}
