package a2a

import (
	"context"
	"errors"

	"github.com/xraph/cortex"
)

// admit asks the host, when it gave the bus an Admitter, whether work that
// can start or resume a run may go ahead here.
func (b *Bus) admit(ctx context.Context) (context.Context, func(), error) {
	if b.admitter == nil {
		return ctx, func() {}, nil
	}
	return b.admitter.AdmitRun(ctx)
}

// isStopping reports a refusal that means "carry this elsewhere": the host
// is draining and nothing was started.
func isStopping(err error) bool { return errors.Is(err, cortex.ErrEngineStopping) }

// requeueDelivery hands a claimed delivery back to the queue. It is only
// for a delivery whose carry started nothing and answered nothing.
func (b *Bus) requeueDelivery(ctx context.Context, d *Delivery) error {
	d.State = DeliveryQueued
	d.ClaimedAt = nil
	d.Error = ""
	return b.store.UpdateDelivery(ctx, d)
}
