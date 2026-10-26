package learning

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex/internal/hostport"
)

// Sink reconciles an accepted publication against authoritative remote state.
type Sink interface {
	Deliver(context.Context, Publication) (Receipt, error)
}

// Publisher drains one bounded page of exact-scope durable publication work.
// A host schedules repeated calls and supplies the worker's current authority.
type Publisher struct {
	repository Repository
	sink       Sink
	guard      *Guard
	owner      string
}

// NewPublisher requires durable storage and a guarded destination adapter.
func NewPublisher(repository Repository, sink Sink, guard *Guard, owner string) (*Publisher, error) {
	if hostport.IsNil(repository) || hostport.IsNil(sink) || guard == nil || owner == "" || len(owner) > 512 {
		return nil, ErrInvalid
	}
	return &Publisher{repository: repository, sink: sink, guard: guard, owner: owner}, nil
}

// WorkPage resumes from the supplied durable scan cursor. Delivery leases and
// acknowledgments survive worker replacement; a lost reply keeps its aggregate ID.
func (p *Publisher) WorkPage(ctx context.Context, after string) (string, error) {
	page, err := p.repository.ListLessonPublications(ctx, after, 100)
	if err != nil {
		return "", err
	}
	var failures []error
	for _, pending := range page.Items {
		if contextErr := ctx.Err(); contextErr != nil {
			return after, contextErr
		}
		action := ActionPublish
		if pending.Operation == "revoke" {
			action = ActionRevoke
		}
		if err = p.guard.Authorize(ctx, action, pending.LessonID, pending.Payload.Domain); err != nil {
			failures = append(failures, err)
			continue
		}
		if pending.Receipt != nil {
			continue
		}
		claim, claimErr := p.repository.ClaimLessonPublication(ctx, pending.LessonID, pending.Revision, p.owner)
		if errors.Is(claimErr, ErrConflict) {
			continue
		}
		if claimErr != nil {
			failures = append(failures, claimErr)
			continue
		}
		bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
		receipt, deliveryErr := p.sink.Deliver(bounded, claim)
		cancel()
		var acknowledgment *Receipt
		code := ""
		if deliveryErr == nil {
			acknowledgment = &receipt
		} else {
			code = "unavailable"
			switch {
			case errors.Is(deliveryErr, ErrDenied):
				code = "denied"
			case errors.Is(deliveryErr, ErrIneligible):
				code = "ineligible"
			case errors.Is(deliveryErr, ErrConflict):
				code = "conflict"
			}
			failures = append(failures, deliveryErr)
		}
		if completeErr := p.repository.CompleteLessonPublication(ctx, claim, acknowledgment, code); completeErr != nil {
			failures = append(failures, completeErr)
		}
	}
	return page.After, errors.Join(failures...)
}

// Retry authorizes an operator-requested retry at an exact delivery generation.
func (p *Publisher) Retry(ctx context.Context, publication Publication) error {
	current, err := p.repository.GetLessonPublication(ctx, publication.LessonID, publication.Revision)
	if err != nil {
		return err
	}
	if current.Generation != publication.Generation {
		return ErrConflict
	}
	if authErr := p.guard.Authorize(ctx, ActionRetry, current.LessonID, current.Payload.Domain); authErr != nil {
		return authErr
	}
	return p.repository.RetryLessonPublication(ctx, current.LessonID, current.Revision, current.Generation)
}
