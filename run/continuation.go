package run

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex/id"
)

// ReadySource says what made a paused run ready.
type ReadySource string

const (
	// ReadyApproval is a reviewer's approval of the run's pending calls.
	ReadyApproval ReadySource = "approval"
	// ReadyExternalResults is a caller's answers to the run's external calls.
	ReadyExternalResults ReadySource = "external_results"
	// ReadyRejection is a reviewer's rejection that goes back to the model.
	// It only appears with engine.WithRejectionsReturnToModel.
	ReadyRejection ReadySource = "rejection"
)

// Readiness is what a ready run continues with.
type Readiness struct {
	Source ReadySource `json:"source"`
	// Results holds the engine-encoded tool results. Stores keep the bytes
	// exactly as given and never interpret them.
	Results      json.RawMessage `json:"results,omitempty"`
	CheckpointID id.CheckpointID `json:"checkpoint_id,omitempty"`
	At           time.Time       `json:"at"`
	// SuspensionID is the pause the decision or the results answer.
	// MarkReady refuses with cortex.ErrNotSuspended when the run's current
	// suspension is a different one, so an answer to one pause can never
	// be applied to the next.
	SuspensionID id.SuspensionID `json:"suspension_id,omitempty"`
	// DecidedBy and DecidedByPrincipal are the checkpoint decider, kept
	// so the decision can still be recorded with its real decider if the
	// write to the checkpoint is lost. Empty for external results.
	DecidedBy          string `json:"decided_by,omitempty"`
	DecidedByPrincipal []byte `json:"decided_by_principal,omitempty"`
}

// ReadyNotice is what a host hears when a run becomes ready to continue:
// what made it ready, and when. The run's scope travels on the hook's ctx.
type ReadyNotice struct {
	Source ReadySource
	// CheckpointID is the checkpoint whose decision made the run ready. It
	// is zero when Source is ReadyExternalResults.
	CheckpointID id.CheckpointID
	At           time.Time
	// Renotified is true when the sweeper repeats the notice for a run
	// that has waited longer than it should, false the first time.
	Renotified bool
}

// ReclaimNotice is what a host hears when a run becomes reclaimable: whose
// lease it was, when that lease ended, and how often the run has been
// reclaimed. The run's scope travels on the hook's ctx.
type ReclaimNotice struct {
	// PreviousOwner is the worker whose lease the run was under.
	PreviousOwner string
	// LeaseExpiredAt is when the lease lapsed, or, for a hand-off, when
	// the worker released it.
	LeaseExpiredAt time.Time
	// Reclaims counts how often the sweeper has reclaimed the run after a
	// lapsed lease. A hand-off does not add to it.
	Reclaims int
	// HandOff is true when the worker released the run at shutdown rather
	// than its lease lapsing.
	HandOff bool
	// Renotified is true when the sweeper repeats the notice for a run
	// that has waited longer than it should, false the first time.
	Renotified bool
}

// Lease is one worker's hold on a running run. A lease whose ExpiresAt has
// passed has lapsed, and the sweeper may hand the run to another replica.
type Lease struct {
	Owner     string    `json:"owner"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ContinuationStore is an optional capability of run stores. Every method works at the caller's
// exact scope except the two AcrossScopes listings, which exist for the engine sweeper only.
type ContinuationStore interface {
	// MarkReady moves a paused run whose suspension has not expired to StateReady, saves r and
	// advances Revision. ErrNotSuspended when the run is not paused, or when its suspension is not
	// r.SuspensionID; ErrSuspensionExpired when its suspension's deadline has passed. Then it
	// clears the suspension's deadline.
	MarkReady(ctx context.Context, runID id.AgentRunID, r Readiness) (*Run, error)
	// HoldLease sets or extends the lease of a running run whose lease is unowned or owned by
	// lease.Owner. ErrRunLeaseLost otherwise, and for a lease with no owner or no expiry. Never
	// advances Revision.
	HoldLease(ctx context.Context, runID id.AgentRunID, lease Lease) error
	// MarkReclaimable moves a running run to StateReclaimable and clears its lease, advancing
	// Revision. With owner == "" the lease must have expired at or before now (Reclaims+1,
	// ErrRunLeased otherwise, including a run that holds no lease). With owner set the lease must
	// be owner's (a graceful hand-off; Reclaims unchanged, ErrRunLeaseLost otherwise). A run that
	// is no longer running returns ErrRunConflict.
	MarkReclaimable(ctx context.Context, runID id.AgentRunID, now time.Time, owner string) (*Run, error)
	// ClaimContinuation moves a ready or reclaimable run to running under lease, clears Ready and
	// NotifiedAt, advances Revision. ErrRunNotContinuable otherwise. Two callers: exactly one wins.
	// A zero Lease claims the run with no lease, which the lapsed listing never returns. A lease
	// with an owner and no expiry, or an expiry and no owner, is refused with ErrRunLeaseLost.
	ClaimContinuation(ctx context.Context, runID id.AgentRunID, lease Lease) (*Run, error)
	// ListLapsedAcrossScopes lists running runs whose lease expiry is set and at or before now,
	// oldest expiry first. It deliberately bypasses scope, for the sweeper only.
	ListLapsedAcrossScopes(ctx context.Context, now time.Time, limit int) ([]*Run, error)
	// ListContinuableAcrossScopes lists ready or reclaimable runs whose NotifiedAt is nil or before
	// notifiedBefore, oldest UpdatedAt first. It deliberately bypasses scope, for the sweeper only.
	ListContinuableAcrossScopes(ctx context.Context, notifiedBefore time.Time, limit int) ([]*Run, error)
	// MarkNotified sets NotifiedAt on a ready or reclaimable run and never advances Revision.
	// ErrRunNotContinuable when the run is in any other state.
	MarkNotified(ctx context.Context, runID id.AgentRunID, at time.Time) error
}
