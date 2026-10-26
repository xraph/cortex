package a2a

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// ErrRecipientDiscoveryUnavailable means bounded recipient discovery is absent.
var ErrRecipientDiscoveryUnavailable = errors.New("cortex: a2a: recipient discovery unavailable")

// RecipientDiscoveryStore lists bound deliveries still awaiting a durable reply.
// Reads require exact scope and routing partition, return identities only, and
// order them strictly after the cursor. Limit must be between 1 and 256.
// Discovery grants no execution or observation authority.
type RecipientDiscoveryStore interface {
	ListDeferredRecipients(context.Context, id.DeliveryID, int) ([]id.DeliveryID, error)
}

func (s *routingStore) ListDeferredRecipients(ctx context.Context, after id.DeliveryID, limit int) ([]id.DeliveryID, error) {
	reader, ok := s.inner.(RecipientDiscoveryStore)
	if !ok {
		return nil, ErrRecipientDiscoveryUnavailable
	}
	return reader.ListDeferredRecipients(s.context(ctx), after, limit)
}

// RecipientRecoveryConfig enables one bounded discovery worker on a roster-bound
// reserved bus. Both authorization callbacks must consult current host authority.
// Callbacks must honor cancellation. Configuration is copied during NewBus.
type RecipientRecoveryConfig struct {
	AuthorizeDiscovery func(context.Context, id.OrchestrationID) error
	AuthorizeDelivery  func(context.Context, id.OrchestrationID, id.DeliveryID) error
	// BatchSize defaults to 32 and cannot exceed 256.
	BatchSize int
	// AttemptTimeout bounds each discovery or recovery call; default 5s, max 1m.
	AttemptTimeout time.Duration
	// ReportError receives a failed pass on the worker. It must not block shutdown.
	ReportError func(context.Context, error)
}

// RecipientRecoveryPage reports one pass without exposing message or child data.
// Next is the exclusive cursor when the page was full, or zero at the end.
// Per-delivery errors advance Next so one denied or broken row cannot starve
// later rows. The next traversal retries those rows from the beginning.
type RecipientRecoveryPage struct {
	Next      id.DeliveryID
	Examined  int
	Completed int
	Waiting   int
	Failed    int
}

func (b *Bus) configureRecipientRecovery(config *RecipientRecoveryConfig, backend Store) error {
	if config == nil {
		return nil
	}
	if b.roster == nil || b.reserved == nil || nilHostPort(b.recipientObserver) || config.AuthorizeDiscovery == nil || config.AuthorizeDelivery == nil {
		return ErrRecipientObservationUnavailable
	}
	if reader, ok := backend.(RecipientDiscoveryStore); !ok || nilHostPort(reader) {
		return ErrRecipientDiscoveryUnavailable
	}
	cfg := *config
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 32
	}
	if cfg.AttemptTimeout == 0 {
		cfg.AttemptTimeout = 5 * time.Second
	}
	if cfg.BatchSize < 1 || cfg.BatchSize > 256 || cfg.AttemptTimeout < time.Millisecond || cfg.AttemptTimeout > time.Minute {
		return ErrReplyCommitConflict
	}
	b.recipientRecovery = &cfg
	return nil
}

// RecoverRecipients discovers and observes one bounded page. It never reserves,
// executes or resumes a recipient. Callers of synchronous buses drive pages
// themselves; asynchronous buses run this pass from Start until Stop joins it.
func (b *Bus) RecoverRecipients(ctx context.Context, after id.DeliveryID) (RecipientRecoveryPage, error) {
	cfg := b.recipientRecovery
	if cfg == nil {
		return RecipientRecoveryPage{}, ErrRecipientDiscoveryUnavailable
	}
	if err := b.checkRosterScope(ctx); err != nil {
		return RecipientRecoveryPage{}, err
	}
	if !after.IsNil() && after.Prefix() != id.PrefixDelivery {
		return RecipientRecoveryPage{}, ErrReplyCommitConflict
	}
	rows, err := b.discoverRecipients(ctx, after)
	if err != nil {
		return RecipientRecoveryPage{}, err
	}
	page := RecipientRecoveryPage{}
	if len(rows) == cfg.BatchSize {
		page.Next = rows[len(rows)-1]
	}
	var failures []error
	for _, key := range rows {
		if contextErr := ctx.Err(); contextErr != nil {
			return page, errors.Join(append(failures, contextErr)...)
		}
		attempt, cancel := context.WithTimeout(ctx, cfg.AttemptTimeout)
		receipt, recoverErr := b.RecoverRecipient(attempt, key, cfg.AuthorizeDelivery)
		cancel()
		page.Examined++
		switch {
		case recoverErr != nil:
			page.Failed++
			failures = append(failures, recoverErr)
		case receipt.ReplyMessageID.IsNil():
			page.Waiting++
		default:
			page.Completed++
		}
	}
	return page, errors.Join(failures...)
}

func (b *Bus) discoverRecipients(ctx context.Context, after id.DeliveryID) ([]id.DeliveryID, error) {
	cfg := b.recipientRecovery
	ctx, cancel := context.WithTimeout(ctx, cfg.AttemptTimeout)
	defer cancel()
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return cfg.AuthorizeDiscovery(ctx, b.orchestrationID)
	}
	if err := check(); err != nil {
		return nil, err
	}
	reader, ok := b.store.(RecipientDiscoveryStore)
	if !ok {
		return nil, ErrRecipientDiscoveryUnavailable
	}
	rows, err := reader.ListDeferredRecipients(ctx, after, cfg.BatchSize)
	if err != nil {
		return nil, err
	}
	if authErr := check(); authErr != nil {
		return nil, authErr
	}
	if len(rows) > cfg.BatchSize {
		return nil, ErrReplyCommitConflict
	}
	previous := after.String()
	for _, key := range rows {
		if key.Prefix() != id.PrefixDelivery || key.String() <= previous {
			return nil, ErrReplyCommitConflict
		}
		previous = key.String()
	}
	return rows, nil
}

func (b *Bus) recoverRecipientsUntilStopped(ctx context.Context) {
	ctx = cortex.WithScope(ctx, b.roster.discoveryScope)
	ticker := time.NewTicker(b.opts.SweepInterval)
	defer ticker.Stop()
	var after id.DeliveryID
	for {
		reclaimErr := b.reclaimRecipientReplies(ctx)
		page, err := b.RecoverRecipients(ctx, after)
		// A discovery error returns no examined rows. Retain the cursor for retry.
		if err == nil || page.Examined > 0 {
			after = page.Next
		}
		err = errors.Join(reclaimErr, err)
		if ctx.Err() != nil {
			return
		}
		if err != nil && b.recipientRecovery.ReportError != nil {
			b.recipientRecovery.ReportError(ctx, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *Bus) reclaimRecipientReplies(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, b.recipientRecovery.AttemptTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := b.recipientRecovery.AuthorizeDiscovery(ctx, b.orchestrationID); err != nil {
		return err
	}
	reclaimed, err := b.ReclaimStaleDeliveries(ctx)
	if reclaimed > 0 {
		b.dispatch.enqueue(id.Nil)
	}
	return err
}

// PendingDispatch reports queued deliveries or expired asks in the captured
// roster scope. Current discovery authority is checked around the metadata read.
// It never claims work, publishes a reply or resumes an agent.
func (b *Bus) PendingDispatch(ctx context.Context) (bool, error) {
	if b.recipientRecovery == nil {
		return false, ErrRecipientDiscoveryUnavailable
	}
	if err := b.checkRosterScope(ctx); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, b.recipientRecovery.AttemptTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := b.recipientRecovery.AuthorizeDiscovery(ctx, b.orchestrationID); err != nil {
		return false, err
	}
	queued, err := b.store.ListQueuedDeliveries(ctx, 1)
	if err != nil {
		return false, err
	}
	ready := len(queued) > 0
	if !ready {
		expired, readErr := b.store.ListExpiredAsks(ctx, b.clock.Now(), 1)
		if readErr != nil {
			return false, readErr
		}
		ready = len(expired) > 0
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := b.recipientRecovery.AuthorizeDiscovery(ctx, b.orchestrationID); err != nil {
		return false, err
	}
	return ready, nil
}
