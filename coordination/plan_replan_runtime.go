package coordination

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/planning"
)

// PlanReplanFence holds current plan grants and worker incarnation exclusion
// through the entire synchronous operation, including model dispatch. Every host
// actor that can revoke authority or deliver delayed work must share this fence.
type PlanReplanFence interface {
	WithPlanReplanFence(context.Context, PlanReplanControl, func(PlanReplanControl) error) error
}

// PlanReplanAuthorizer authenticates actor, trusted progress, current resources,
// pricing bounds and every quiescence attestation. Commit additionally requires
// permission to supersede the exact graph. Historical retries reauthorize access.
type PlanReplanAuthorizer interface {
	AuthorizePlanReplan(context.Context, PlanReplanControl) error
}

// PlanReplanRuntime executes a fresh admission through the existing prepared
// runtime with its reserved RunID, frozen input and explicit empty tool allowlist.
// It must never restart existing or ambiguous creation and must honor cancellation.
type PlanReplanRuntime interface {
	ExecutePlanReplan(context.Context, PlanReplanAdmission) error
}

// PlanReplannerConfig bounds a fresh model dispatch.
type PlanReplannerConfig struct {
	// ExecutionTimeout defaults to 30 seconds and cannot exceed 30 minutes.
	// The reservation and caller deadlines can only shorten this limit.
	ExecutionTimeout time.Duration
}

// PlanReplanner coordinates guarded admission, observation and graph commit.
type PlanReplanner struct {
	store            PlanReplanStore
	fence            PlanReplanFence
	authorizer       PlanReplanAuthorizer
	runtime          PlanReplanRuntime
	executionTimeout time.Duration
}

// NewPlanReplanner requires explicit authority and the bounded prepared runtime.
func NewPlanReplanner(store PlanReplanStore, fence PlanReplanFence, authorizer PlanReplanAuthorizer, runtime PlanReplanRuntime, configs ...PlanReplannerConfig) (*PlanReplanner, error) {
	if hostport.IsNil(store) || hostport.IsNil(fence) || hostport.IsNil(authorizer) || hostport.IsNil(runtime) {
		return nil, planning.ErrInvalid
	}
	if len(configs) > 1 {
		return nil, planning.ErrInvalid
	}
	config := PlanReplannerConfig{}
	if len(configs) == 1 {
		config = configs[0]
	}
	if config.ExecutionTimeout < 0 || config.ExecutionTimeout > 30*time.Minute {
		return nil, planning.ErrInvalid
	}
	if config.ExecutionTimeout == 0 {
		config.ExecutionTimeout = 30 * time.Second
	}
	return &PlanReplanner{store: store, fence: fence, authorizer: authorizer, runtime: runtime, executionTimeout: config.ExecutionTimeout}, nil
}
func (r *PlanReplanner) guard() taskAuthority[PlanReplanControl] {
	return taskAuthority[PlanReplanControl]{operation: "plan replan", invalid: planning.ErrInvalid, conflict: planning.ErrConflict, scope: func(c PlanReplanControl) cortex.Scope { return c.Request.Progress.Plan.Scope }, fence: r.fence.WithPlanReplanFence, authorize: r.authorizer.AuthorizePlanReplan}
}

// Execute admits and dispatches once. Any error or historical acknowledgment is
// observation-only; callers must never infer permission from a retained receipt.
func (r *PlanReplanner) Execute(ctx context.Context, request PlanReplanRequest) (PlanReplanAdmission, error) {
	bounded, cancel := context.WithTimeout(ctx, r.executionTimeout)
	defer cancel()
	return withTaskAuthority(bounded, PlanReplanControl{Request: request, Operation: "dispatch"}, r.guard(), func(tc context.Context, c PlanReplanControl, _ string) (PlanReplanAdmission, error) {
		a, err := r.store.AdmitPlanReplan(tc, c.Request)
		if err != nil {
			return PlanReplanAdmission{}, err
		}
		_, digest, err := c.Request.Freeze()
		if err != nil || a.Validate() != nil || a.RequestDigest != digest {
			return PlanReplanAdmission{}, planning.ErrConflict
		}
		if !a.Fresh {
			return a, nil
		}
		if checkErr := tc.Err(); checkErr != nil {
			return a, checkErr
		}
		if !time.Now().Before(a.Request.Reservation.Deadline) {
			return a, planning.ErrConflict
		}
		dispatch, stop := context.WithDeadline(tc, a.Request.Reservation.Deadline)
		defer stop()
		return a, r.runtime.ExecutePlanReplan(dispatch, a)
	})
}

// Observe retains terminal output or an authenticated permanent abandonment.
// Nil means pending and never authorizes another invocation.
func (r *PlanReplanner) Observe(ctx context.Context, control PlanReplanControl) (*PlanReplanOutcome, error) {
	if control.Operation != "observe" {
		return nil, planning.ErrInvalid
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return withTaskAuthority(bounded, control, r.guard(), func(tc context.Context, c PlanReplanControl, _ string) (*PlanReplanOutcome, error) {
		out, err := r.store.ObservePlanReplan(tc, c)
		if err != nil || out == nil {
			return out, err
		}
		_, digest, err := c.Request.Freeze()
		if err != nil || out.Validate() != nil || out.AdmissionDigest != digest || out.AttemptID != c.Request.ID {
			return nil, planning.ErrConflict
		}
		frozen, err := SealPlanReplanOutcome(*out)
		return &frozen, err
	})
}

// Commit preserves historical evidence while atomically superseding a graph.
func (r *PlanReplanner) Commit(ctx context.Context, control PlanReplanControl) (planning.Plan, error) {
	if control.Operation != "commit" {
		return planning.Plan{}, planning.ErrInvalid
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return withTaskAuthority(bounded, control, r.guard(), func(tc context.Context, c PlanReplanControl, _ string) (planning.Plan, error) {
		next, err := r.store.CommitPlanReplan(tc, c)
		if err != nil {
			return planning.Plan{}, err
		}
		if planning.Validate(next, planning.Limits{}) != nil || next.ID != c.Request.Progress.Plan.ID || next.PreviousDigest != c.Request.Progress.Plan.Digest || next.Revision != c.Request.Progress.Plan.Revision+1 {
			return planning.Plan{}, planning.ErrConflict
		}
		return next, nil
	})
}
