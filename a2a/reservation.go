package a2a

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// ErrDeliveryBindingUnavailable means persistence cannot fence run admission.
var ErrDeliveryBindingUnavailable = errors.New("cortex: a2a: delivery run binding unavailable")

// DeliveryAdmissionError retains an attempted child when dispatch is refused.
// Its identity is evidence for reconciliation, never permission to replay.
type DeliveryAdmissionError struct {
	deliveryID id.DeliveryID
	runID      id.AgentRunID
	cause      error
}

func (e *DeliveryAdmissionError) Error() string {
	return "cortex: a2a: reserved delivery admission failed"
}

// Unwrap preserves matching of the underlying admission failure.
func (e *DeliveryAdmissionError) Unwrap() error { return e.cause }

// RunIdentity returns the reserved child's identity, including uncertain writes.
func (e *DeliveryAdmissionError) RunIdentity() id.AgentRunID { return e.runID }

// DeliveryIdentity returns the delivery whose admission failed.
func (e *DeliveryAdmissionError) DeliveryIdentity() id.DeliveryID { return e.deliveryID }

// DeliveryRunBinder atomically records a reserved run under the current claim.
// Binding is single-use, including after a lost acknowledgment. A bound delivery
// cannot be reclaimed by timeout; the host must reconcile its execution first.
type DeliveryRunBinder interface {
	BindDeliveryRun(context.Context, *Delivery, id.AgentRunID) error
}

func deliveryRunBinder(s Store) (DeliveryRunBinder, bool) {
	if bound, ok := s.(*routingStore); ok {
		s = bound.inner
	}
	binder, ok := s.(DeliveryRunBinder)
	return binder, ok
}

func (s *routingStore) BindDeliveryRun(ctx context.Context, d *Delivery, runID id.AgentRunID) error {
	binder, ok := deliveryRunBinder(s)
	if !ok {
		return ErrDeliveryBindingUnavailable
	}
	return binder.BindDeliveryRun(s.context(ctx), d, runID)
}

func (b *Bus) reserveDirective(ctx context.Context, d *Delivery, e *Envelope) (result *cortex.AgentResult, executionErr, admissionErr error) {
	if !d.RunID.IsNil() || d.ClaimToken == "" {
		return nil, nil, ErrDeliveryClaimLost
	}
	inv, reserveErr := b.reserved.ReserveAgent(ctx, b.orchestrationID, d.Receiver.Agent, RenderInput(e), nil)
	if inv == nil || inv.RunID.IsNil() || inv.RunID.Prefix() != id.PrefixAgentRun {
		return nil, nil, errors.Join(cortex.ErrRunIncomplete, reserveErr)
	}
	// Own the admission values across storage and execution callbacks.
	frozen := *inv
	inv = &frozen
	defer func() {
		if admissionErr != nil {
			admissionErr = &DeliveryAdmissionError{deliveryID: d.ID, runID: inv.RunID, cause: admissionErr}
		}
	}()
	// A cancellation after reservation does not erase the identity. Binding
	// cannot dispatch work, and a lost acknowledgment requires reconciliation.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	bindErr := b.binder.BindDeliveryRun(writeCtx, d, inv.RunID)
	cancel()
	if bindErr != nil {
		return nil, nil, errors.Join(bindErr, reserveErr)
	}
	d.RunID = inv.RunID
	if reserveErr != nil || ctx.Err() != nil {
		return nil, nil, errors.Join(reserveErr, ctx.Err())
	}
	if inv.Execute == nil {
		return nil, nil, cortex.ErrRunIncomplete
	}
	out, runErr := inv.Execute(ctx)
	if out == nil {
		return nil, errors.Join(cortex.ErrRunIncomplete, runErr), nil
	}
	if out.RunID != inv.RunID {
		mismatch := fmt.Errorf("%w: reserved %s returned %s", cortex.ErrRunIncomplete, inv.RunID, out.RunID)
		return nil, nil, errors.Join(mismatch, b.failDelivery(ctx, d, mismatch))
	}
	return out, errors.Join(runErr, out.Err), nil
}
