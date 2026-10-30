package suspension

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

// LeasedClaim is a pause claim that takes the run's lease in the same
// transaction as the move from paused to running, so no worker that
// claimed a pause can die holding a running run nobody owns.
type LeasedClaim struct {
	// Version pins the pause and the run revision read before the claim,
	// exactly as ClaimSuspensionVersion does.
	Version ClaimVersion
	// ExpiredBy, when set, claims only a pause whose deadline passed at or
	// before it: the expiry sweep's claim. Otherwise only a pause that has
	// not expired is claimed, and an expired one is ErrSuspensionExpired.
	ExpiredBy time.Time
	// Lease is the claimer's lease. An owner with no expiry, or an expiry
	// with no owner, is refused with cortex.ErrRunLeaseLost.
	Lease run.Lease
	// First, when set, is the run's first progress snapshot under the new
	// lease, written in the same transaction. Its Owner must be Lease.Owner.
	First *Progress
}

// LeasedClaimStore is an optional capability of suspension stores: claims
// that take the run's lease, and write its first progress snapshot, in the
// same transaction that moves the run to running. A worker that dies right
// after such a claim leaves a lapsed lease, which the engine sweep reclaims,
// and a snapshot that already carries every answer the claim was made with.
type LeasedClaimStore interface {
	// ClaimSuspensionLeased claims the pinned pause under claim.Lease and
	// returns it with ClaimedRun set. The refusals are ClaimSuspensionVersion's
	// (cortex.ErrNotSuspended, cortex.ErrRunConflict,
	// cortex.ErrSuspensionExpired), plus cortex.ErrRunLeaseLost for a half
	// lease or a First owned by someone else.
	ClaimSuspensionLeased(ctx context.Context, claim LeasedClaim) (*Suspension, error)
	// ClaimContinuationLeased is run.ContinuationStore's ClaimContinuation
	// under a lease that must be set, writing first in the same transaction.
	ClaimContinuationLeased(ctx context.Context, runID id.AgentRunID, lease run.Lease, first *Progress) (*run.Run, error)
	// AdmitResumeLeased is ResumeAdmissionStore's AdmitResume, taking the
	// run's lease and writing first in the transaction that claims the
	// pause. An admitter that dies before it starts the resume leaves a
	// lease that lapses and the reply in the snapshot. An admission recorded
	// before is returned as it is, and takes nothing.
	AdmitResumeLeased(ctx context.Context, req ResumeAdmissionRequest, lease run.Lease, first *Progress) (ResumeAdmission, error)
	// StartResumeLeased is ResumeAdmissionStore's StartResume, taking the
	// lease of the run the admission claimed, whoever held it, and writing
	// first, in the transaction that grants execution: the one worker
	// allowed to execute the resume holds the run from then on. It never
	// advances the run's revision, which the admission recorded. A start
	// recorded before is returned as it is, and takes nothing.
	StartResumeLeased(ctx context.Context, runID id.AgentRunID, key id.SuspensionID, digest string, lease run.Lease, first *Progress) (ResumeStart, error)
}
