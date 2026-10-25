package coordination

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/run"
)

var (
	ErrTaskUsageConflict = cortex.ErrTaskUsageConflict
	ErrTaskUsageClosed   = cortex.ErrTaskUsageClosed
)

// TaskUsageTermination binds accounting closure to a failed, cancelled or
// incomplete run. It does not release ownership or grant permission to retry.
type TaskUsageTermination struct {
	RunRevision int64     `json:"run_revision"`
	State       run.State `json:"state"`
	CompletedAt time.Time `json:"completed_at"`
}

func (t TaskUsageTermination) valid() bool {
	return t.RunRevision > 0 && !t.CompletedAt.IsZero() && (t.State == run.StateFailed || t.State == run.StateCancelled || t.State == run.StateIncomplete)
}

// TaskUsageClosureRequest permanently closes new verification admission against
// an exact usage observation. It does not authorize refunds or assert final bills.
type TaskUsageClosureRequest struct {
	SchemaVersion int                   `json:"schema_version"`
	Usage         TaskUsageRequest      `json:"usage"`
	UsageDigest   string                `json:"usage_digest"`
	Actor         string                `json:"actor"`
	Reason        string                `json:"reason"`
	Termination   *TaskUsageTermination `json:"termination,omitempty"`
}

// Freeze validates and detaches closure input without authenticating its actor.
func (r TaskUsageClosureRequest) Freeze() (TaskUsageClosureRequest, string, error) {
	usage, err := r.Usage.Freeze()
	if err != nil || r.SchemaVersion != 1 || !evidence.ValidDigest(r.UsageDigest) || !handoffText(r.Actor, 512) || !handoffText(r.Reason, 4096) {
		return TaskUsageClosureRequest{}, "", ErrInvalidTaskUsage
	}
	if r.Termination != nil {
		if !r.Termination.valid() {
			return TaskUsageClosureRequest{}, "", ErrInvalidTaskUsage
		}
		detached := *r.Termination
		r.Termination = &detached
	}
	r.Usage = usage
	data, err := json.Marshal(r)
	if err != nil || len(data) > 32<<10 {
		return TaskUsageClosureRequest{}, "", ErrInvalidTaskUsage
	}
	return r, evidence.Digest(data), nil
}

// MatchesClaim checks ownership identity and closure mode. It does not establish
// current storage state, host authorization or the stated terminal run revision.
func (r TaskUsageClosureRequest) MatchesClaim(c Claim) bool {
	if c.Token() != r.Usage.Claim || c.RunID != r.Usage.Seal.Request.RunID || c.Scope.Canonical() != r.Usage.Seal.Request.Scope.Canonical() {
		return false
	}
	return (c.State == ClaimCompleted && r.Termination == nil) || (c.State == ClaimRecovering && r.Termination != nil && r.Termination.valid())
}

// TaskUsageClosure retains permanent local admission closure. Fresh is true
// only for the first confirmed write. A retry is a historical acknowledgment.
type TaskUsageClosure struct {
	SchemaVersion int                     `json:"schema_version"`
	Request       TaskUsageClosureRequest `json:"request"`
	RequestDigest string                  `json:"request_digest"`
	At            time.Time               `json:"at"`
	Digest        string                  `json:"digest"`
	Fresh         bool                    `json:"-"`
}

// SealTaskUsageClosure validates and detaches a closure receipt.
func SealTaskUsageClosure(c TaskUsageClosure) (TaskUsageClosure, error) {
	r, digest, err := c.Request.Freeze()
	if err != nil || c.SchemaVersion != 1 || digest != c.RequestDigest || c.At.IsZero() || c.At.Before(r.Usage.Seal.At) || (r.Termination != nil && c.At.Before(r.Termination.CompletedAt)) {
		return TaskUsageClosure{}, ErrInvalidTaskUsage
	}
	c.Request, c.Digest = r, ""
	data, err := json.Marshal(c)
	if err != nil {
		return TaskUsageClosure{}, err
	}
	c.Digest = evidence.Digest(data)
	return c, nil
}

// Validate checks receipt integrity, not authority or external quiescence.
func (c TaskUsageClosure) Validate() error {
	sealed, err := SealTaskUsageClosure(c)
	if err != nil || sealed.Digest != c.Digest {
		return ErrInvalidTaskUsage
	}
	return nil
}

// TaskUsageClosureStore closes admission atomically with full evidence checks.
// Its trusted caller must hold current host authority through the transaction.
// Get returns nil when no closure exists. Reads and retries grant no authority.
type TaskUsageClosureStore interface {
	CloseTaskUsage(context.Context, TaskUsageClosureRequest) (TaskUsageClosure, error)
	GetTaskUsageClosure(context.Context, id.AgentRunID) (*TaskUsageClosure, error)
}

// TaskUsageClosureFence holds current host authority through one synchronous
// storage callback. External proof retrieval must happen before that callback.
type TaskUsageClosureFence interface {
	WithTaskUsageClosureFence(context.Context, TaskUsageClosureRequest, func(TaskUsageClosureRequest) error) error
}

// TaskUsageClosureAuthorizer authenticates Actor and authorizes permanent local
// closure for the exact run, claim and evidence, including historical retries.
type TaskUsageClosureAuthorizer interface {
	AuthorizeTaskUsageClosure(context.Context, TaskUsageClosureRequest) error
}

// TaskUsageCloser closes admission after current host checks. Errors may follow
// a commit; retry the identical request to recover its historical receipt.
type TaskUsageCloser interface {
	Close(context.Context, TaskUsageClosureRequest) (TaskUsageClosure, error)
}

type guardedTaskUsageCloser struct {
	store      TaskUsageClosureStore
	fence      TaskUsageClosureFence
	authorizer TaskUsageClosureAuthorizer
}

// NewTaskUsageCloser requires explicit authority and fencing ports.
func NewTaskUsageCloser(store TaskUsageClosureStore, fence TaskUsageClosureFence, authorizer TaskUsageClosureAuthorizer) (TaskUsageCloser, error) {
	if hostport.IsNil(store) || hostport.IsNil(fence) || hostport.IsNil(authorizer) {
		return nil, ErrInvalidTaskUsage
	}
	return &guardedTaskUsageCloser{store: store, fence: fence, authorizer: authorizer}, nil
}

func (c *guardedTaskUsageCloser) Close(ctx context.Context, r TaskUsageClosureRequest) (TaskUsageClosure, error) {
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	guard := taskAuthority[TaskUsageClosureRequest]{operation: "task usage closure", invalid: ErrInvalidTaskUsage, conflict: ErrTaskUsageConflict, scope: func(r TaskUsageClosureRequest) cortex.Scope { return r.Usage.Seal.Request.Scope }, fence: c.fence.WithTaskUsageClosureFence, authorize: c.authorizer.AuthorizeTaskUsageClosure}
	return withTaskAuthority(bounded, r, guard, func(tc context.Context, frozen TaskUsageClosureRequest, digest string) (TaskUsageClosure, error) {
		receipt, err := c.store.CloseTaskUsage(tc, frozen)
		if err != nil {
			return TaskUsageClosure{}, err
		}
		if receipt.Validate() != nil || receipt.RequestDigest != digest {
			return TaskUsageClosure{}, ErrTaskUsageConflict
		}
		return SealTaskUsageClosure(receipt)
	})
}
