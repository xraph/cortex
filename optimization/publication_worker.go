package optimization

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// PublicationSink delivers immutable outbox payloads. Implementations must
// reconcile the stable aggregate ID and digest before retrying an ambiguous write.
// A receipt must come from authoritative destination state, never a search index.
type PublicationSink interface {
	Deliver(context.Context, Publication) (PublicationReceipt, error)
}

// PublisherConfig supplies the destination and current host authority. Now must
// agree with the repository clock; nil selects the system clock.
type PublisherConfig struct {
	Repository       Repository
	Authorizer       Authorizer
	IdentityResolver IdentityResolver
	Caller           CallerIdentity
	Sink             PublicationSink
	Now              func() time.Time
}

// Publisher drains scoped lesson outbox records without changing experiment state.
// Each instance owns one serial delivery loop. The host supplies its scoped context.
type Publisher struct {
	config PublisherConfig
	meter  *LedgerMeter
	owner  string
	work   sync.Mutex
	cursor string
	scope  string
}

// NewPublisher validates dependencies without starting background work.
func NewPublisher(config PublisherConfig) (*Publisher, error) {
	if config.Caller == nil || config.Sink == nil {
		return nil, ErrInvalidSpec
	}
	meter, err := NewLedgerMeter(config.Repository, config.Authorizer, config.IdentityResolver)
	if err != nil {
		return nil, err
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Publisher{config: config, meter: meter, owner: id.New(PrefixEvent).String()}, nil
}

// WorkOnce visits one bounded page. A failed delivery records a retry before
// returning ErrUnavailable. Later pages remain reachable when earlier rows wait.
func (p *Publisher) WorkOnce(ctx context.Context) error {
	if !p.work.TryLock() {
		return ErrConflict
	}
	defer p.work.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	key, err := ScopeKey(scope)
	if err != nil {
		return err
	}
	actor, err := p.config.Caller.Current(copyScopeContext(ctx))
	if err != nil {
		return err
	}
	if !ValidOpaqueKey(actor.Subject) {
		return ErrPolicyDenied
	}
	if authErr := p.config.Authorizer.Authorize(copyScopeContext(ctx), ActionRead, Resource{Scope: copyScope(scope), Kind: "publication"}); authErr != nil {
		return authErr
	}
	if key != p.scope {
		p.scope = key
		p.cursor = ""
	}
	page, err := p.config.Repository.ListPublications(ctx, PublicationQuery{PageQuery: PageQuery{Limit: 100, Cursor: p.cursor}, PendingOnly: true})
	if err != nil {
		return err
	}
	p.cursor = page.NextCursor
	var failures []error
	for _, value := range page.Items {
		now := p.config.Now()
		if now.Before(value.NextRetry) || now.Before(value.DeliveryLease.ExpiresAt) {
			continue
		}
		if err = p.deliver(ctx, value, actor); err != nil && !errors.Is(err, ErrConflict) {
			if !errors.Is(err, ErrUnavailable) {
				return err
			}
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// Work polls until canceled. Durable delivery failures are retried with backoff;
// authorization and persistence failures return to the host supervisor.
func (p *Publisher) Work(ctx context.Context) error {
	for {
		if err := p.WorkOnce(ctx); err != nil && !errors.Is(err, ErrUnavailable) {
			return err
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (p *Publisher) deliver(ctx context.Context, value Publication, actor IdentityRef) error {
	action := ActionPublishLesson
	if value.Operation == PublicationRevoke {
		action = ActionRevokeLesson
	}
	resolved, cleanup, err := p.meter.authorizeAction(ctx, value.ExperimentID, action)
	if err != nil {
		return err
	}
	defer cleanup()
	var claimed Publication
	if claimErr := p.config.Repository.Transact(resolved, func(tx Transaction) error {
		var claimErr error
		claimed, claimErr = tx.ClaimPublication(PublicationClaim{LessonID: value.LessonID, LocalRevision: value.LocalRevision, Owner: p.owner, Duration: time.Minute, Actor: actor})
		return claimErr
	}); claimErr != nil {
		return claimErr
	}
	// Keep the lease separately from the detached value passed to the destination.
	lease := claimed.DeliveryLease
	if claimed.Attempts > 8 {
		return p.recordFailure(resolved, lease, claimed.Attempts, "remote_unavailable", true)
	}
	callCtx, cancel := context.WithTimeout(resolved, lease.ExpiresAt.Sub(p.config.Now()))
	receipt, deliveryErr := p.config.Sink.Deliver(callCtx, claimed)
	cancel()
	current, release, err := p.meter.authorizeAction(ctx, value.ExperimentID, action)
	if err != nil {
		return err
	}
	defer release()
	if deliveryErr == nil {
		err = p.config.Repository.Transact(current, func(tx Transaction) error { return tx.CompletePublication(lease, receipt) })
		if !errors.Is(err, ErrIncompleteEvidence) {
			return err
		}
		return p.recordFailure(current, lease, claimed.Attempts, "invalid_receipt", true)
	}
	terminal := errors.Is(deliveryErr, ErrConflict) || errors.Is(deliveryErr, ErrPolicyDenied) || errors.Is(deliveryErr, ErrIncompleteEvidence)
	code := "remote_unavailable"
	switch {
	case errors.Is(deliveryErr, ErrPolicyDenied):
		code = "policy_denied"
	case errors.Is(deliveryErr, ErrIncompleteEvidence):
		code = "invalid_receipt"
	case errors.Is(deliveryErr, ErrConflict):
		code = "conflict"
	}
	return p.recordFailure(current, lease, claimed.Attempts, code, terminal)
}

func (p *Publisher) recordFailure(ctx context.Context, lease PublicationLease, attempts int, code string, terminal bool) error {
	// Eight attempts bound repeated writes. Provider error text is never persisted.
	terminal = terminal || attempts >= 8
	var retry time.Time
	if !terminal {
		delay := time.Second << min(max(attempts-1, 0), 6)
		retry = p.config.Now().Add(delay)
	}
	if err := p.config.Repository.Transact(ctx, func(tx Transaction) error { return tx.FailPublication(lease, code, retry, terminal) }); err != nil {
		return err
	}
	return ErrUnavailable
}
