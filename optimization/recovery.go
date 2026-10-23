package optimization

import (
	"context"

	"github.com/xraph/cortex/id"
)

// VerifiedUsageReceipt is returned only after the host verifies provider receipt
// provenance and its exact dispatch digest. Recovery cannot execute another call.
type VerifiedUsageReceipt struct {
	RequestDigest string
	Settlement    UsageSettlement
}

// UsageReceiptProvider looks up existing accounting evidence without replaying
// the original operation. Ambiguous or missing evidence returns an error.
type UsageReceiptProvider interface {
	Recover(context.Context, Reservation) (VerifiedUsageReceipt, error)
}

// RecoveryRepository supplies durable accounting and bounded reservation reads.
type RecoveryRepository interface {
	MeterRepository
	GetReservation(context.Context, id.ID, string) (Reservation, error)
	ListReservations(context.Context, id.ID, PageQuery) (Page[Reservation], error)
}

// UsageRecovery reconciles existing dispatches after cancellation or lease loss.
type UsageRecovery struct {
	repository RecoveryRepository
	meter      *LedgerMeter
	provider   UsageReceiptProvider
}

// NewUsageRecovery requires host authority and an explicit receipt verifier.
func NewUsageRecovery(repo RecoveryRepository, auth Authorizer, resolver IdentityResolver, provider UsageReceiptProvider) (*UsageRecovery, error) {
	if provider == nil {
		return nil, ErrInvalidSpec
	}
	meter, err := NewLedgerMeter(repo, auth, resolver)
	if err != nil {
		return nil, err
	}
	return &UsageRecovery{repository: repo, meter: meter, provider: provider}, nil
}

// Recover accepts a host-attributed actor, never an agent-provided subject. It
// rechecks current caller and original requester grants after receipt retrieval.
func (r *UsageRecovery) Recover(ctx context.Context, experimentID id.ID, key string, actor IdentityRef) error {
	if !ValidOpaqueKey(key) || !ValidOpaqueKey(actor.Subject) {
		return ErrInvalidSpec
	}
	resolved, cleanup, err := r.meter.authorizeAction(ctx, experimentID, ActionReconcileUsage)
	if err != nil {
		return err
	}
	defer cleanup()
	saved, err := r.repository.GetReservation(resolved, experimentID, key)
	if err != nil {
		return err
	}
	if saved.Request.ExperimentID != experimentID || saved.Request.CallKey != key || saved.Call == nil {
		return ErrIncompleteEvidence
	}
	if saved.State == ReservationSettled {
		return nil
	}
	// Detach callback inputs so the provider cannot change the commit binding.
	input, err := copyModelJSON(&saved, 16<<20)
	if err != nil {
		return err
	}
	receipt, err := r.provider.Recover(copyScopeContext(resolved), *input)
	if err != nil {
		return err
	}
	settlement := receipt.Settlement
	if receipt.RequestDigest != saved.Call.RequestDigest || settlement.ExperimentID != experimentID || settlement.AttemptID != saved.Request.AttemptID || settlement.CallKey != key {
		return ErrIncompleteEvidence
	}
	current, done, err := r.meter.authorizeAction(ctx, experimentID, ActionReconcileUsage)
	if err != nil {
		return err
	}
	defer done()
	return r.repository.Transact(current, func(tx Transaction) error {
		return tx.ReconcileUsage(UsageReconciliation{Settlement: settlement, RequestDigest: receipt.RequestDigest, ExpectedRevision: saved.Revision, Actor: actor})
	})
}
