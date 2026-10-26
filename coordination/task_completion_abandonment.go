package coordination

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/internal/hostport"
)

// TaskCompletionAbandonmentRequest permanently closes one admitted reviewer.
// The host must prove that no delayed dispatch or provider return can arrive.
// Absence, lease expiry and cancellation alone do not establish this condition.
type TaskCompletionAbandonmentRequest struct {
	Completion TaskCompletion         `json:"completion"`
	Proof      PreparedRunAbandonment `json:"proof"`
	Actor      string                 `json:"actor"`
	Reason     string                 `json:"reason"`
}

// Freeze detaches the exact admission and host attestation for authorization.
func (r TaskCompletionAbandonmentRequest) Freeze() (TaskCompletionAbandonmentRequest, string, error) {
	if r.Completion.Validate() != nil || r.Proof.validate() != nil || r.Proof.AdmissionDigest != r.Completion.Digest || r.Proof.ObservedAt.Before(r.Completion.ObservedAt) || !handoffText(r.Actor, 512) || !handoffText(r.Reason, 4096) {
		return TaskCompletionAbandonmentRequest{}, "", ErrInvalidReview
	}
	r.Completion.Fresh = false
	data, err := json.Marshal(r)
	if err != nil || len(data) > MaxHandoffBytes+96<<10 {
		return TaskCompletionAbandonmentRequest{}, "", ErrInvalidReview
	}
	var frozen TaskCompletionAbandonmentRequest
	if json.Unmarshal(data, &frozen) != nil {
		return TaskCompletionAbandonmentRequest{}, "", ErrInvalidReview
	}
	return frozen, evidence.Digest(data), nil
}

// TaskCompletionAbandonmentStore atomically marks permanent abandonment and
// settles unknown usage conservatively. Call through an authenticated host guard.
type TaskCompletionAbandonmentStore interface {
	AbandonTaskCompletion(context.Context, TaskCompletionAbandonmentRequest) (TaskCompletionSettlement, error)
}

// TaskCompletionAbandonmentAuthority authenticates the proof issuer and permanent
// dispatch/provider exclusion. Its fence must be shared by every delayed worker
// and held synchronously through commit. Historical retries reauthorize access.
type TaskCompletionAbandonmentAuthority interface {
	WithTaskCompletionAbandonmentFence(context.Context, TaskCompletionAbandonmentRequest, func(TaskCompletionAbandonmentRequest) error) error
	AuthorizeTaskCompletionAbandonment(context.Context, TaskCompletionAbandonmentRequest) error
}

// TaskCompletionAbandoner closes proven quiescent reviewers without redispatch.
type TaskCompletionAbandoner interface {
	Abandon(context.Context, TaskCompletionAbandonmentRequest) (TaskCompletionSettlement, error)
}

// TaskCompletionCloser applies current host authority to permanent closure.
type TaskCompletionCloser struct {
	store     TaskCompletionAbandonmentStore
	authority TaskCompletionAbandonmentAuthority
}

// NewTaskCompletionCloser requires an explicit proof authorizer and shared fence.
func NewTaskCompletionCloser(store TaskCompletionAbandonmentStore, authority TaskCompletionAbandonmentAuthority) (*TaskCompletionCloser, error) {
	if hostport.IsNil(store) || hostport.IsNil(authority) {
		return nil, ErrInvalidReview
	}
	return &TaskCompletionCloser{store: store, authority: authority}, nil
}

// Abandon permanently closes the exact reviewer while retaining unknown costs.
func (c *TaskCompletionCloser) Abandon(ctx context.Context, request TaskCompletionAbandonmentRequest) (TaskCompletionSettlement, error) {
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	guard := taskAuthority[TaskCompletionAbandonmentRequest]{operation: "reviewer abandonment", invalid: ErrInvalidReview, conflict: ErrReviewConflict, scope: func(r TaskCompletionAbandonmentRequest) cortex.Scope { return r.Completion.Request.Handoff.Scope }, fence: c.authority.WithTaskCompletionAbandonmentFence, authorize: c.authority.AuthorizeTaskCompletionAbandonment}
	return withTaskAuthority(bounded, request, guard, func(tc context.Context, r TaskCompletionAbandonmentRequest, digest string) (TaskCompletionSettlement, error) {
		out, err := c.store.AbandonTaskCompletion(tc, r)
		if err != nil {
			return TaskCompletionSettlement{}, err
		}
		if out.Validate() != nil || out.Abandonment == nil || out.CompletionDigest != r.Completion.Digest {
			return TaskCompletionSettlement{}, ErrReviewConflict
		}
		_, retained, freezeErr := out.Abandonment.Freeze()
		if freezeErr != nil || retained != digest {
			return TaskCompletionSettlement{}, ErrReviewConflict
		}
		return out, nil
	})
}

// TaskCompletionAbandonmentResolver supplies authenticated proof material only
// when a host has permanently excluded all execution for the reserved reviewer.
type TaskCompletionAbandonmentResolver interface {
	ResolveTaskCompletionAbandonment(context.Context, TaskCompletion) (*TaskCompletionAbandonmentRequest, error)
}
