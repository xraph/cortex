package optimization

import (
	"context"
	"errors"

	"github.com/xraph/fabriq/core/tenant"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/fabriq/internal/lessontransport"
	"github.com/xraph/cortex/learning"
	opt "github.com/xraph/cortex/optimization"
)

// Deliver reconciles a durable publication with authoritative destination state.
func (a *Adapter) Deliver(ctx context.Context, request opt.Publication) (opt.PublicationReceipt, error) {
	var empty opt.PublicationReceipt
	p, err := a.config.Repository.GetPublication(ctx, request.LessonID, request.LocalRevision)
	if err != nil {
		return empty, err
	}
	digest, err := opt.LessonDigest(request.Payload)
	if err != nil || digest != p.PayloadDigest || request.PayloadDigest != p.PayloadDigest || request.RemoteAggregateID != p.RemoteAggregateID || request.ExperimentID != p.ExperimentID || request.Operation != p.Operation {
		return empty, opt.ErrConflict
	}
	action := opt.ActionPublishLesson
	switch p.Operation {
	case opt.PublicationPublish:
	case opt.PublicationRevoke:
		action = opt.ActionRevokeLesson
	default:
		return empty, opt.ErrInvalidSpec
	}
	mapped, cleanup, err := a.authorize(ctx, p, action)
	if err != nil {
		return empty, err
	}
	defer cleanup()
	if eligibleErr := a.eligible(mapped, p); eligibleErr != nil {
		return empty, eligibleErr
	}
	tenantID, err := tenant.FromContext(mapped)
	if err != nil {
		return empty, err
	}
	row, err := a.reconcile(mapped, p, tenantID)
	if err != nil {
		return empty, err
	}
	checked, release, err := a.authorize(ctx, p, action)
	if err != nil {
		return empty, err
	}
	defer release()
	if eligibleErr := a.eligible(checked, p); eligibleErr != nil {
		return empty, eligibleErr
	}
	return opt.PublicationReceipt{LessonID: p.LessonID, PayloadDigest: p.PayloadDigest, AggregateID: p.RemoteAggregateID, Version: row.Version, AcknowledgedAt: a.config.Now().UTC()}, nil
}

func (a *Adapter) eligible(ctx context.Context, p opt.Publication) error {
	if p.Operation == opt.PublicationRevoke {
		if p.Payload.RevokedAt.IsZero() || p.LocalRevision < 2 {
			return opt.ErrIncompleteEvidence
		}
		return nil
	}
	_, err := a.config.Repository.ResolveLesson(ctx, opt.LessonRef{ID: p.LessonID, Revision: p.LocalRevision, Digest: p.PayloadDigest})
	return err
}

func (a *Adapter) reconcile(ctx context.Context, p opt.Publication, tenantID string) (lessonRow, error) {
	row, err := lessontransport.Reconcile(ctx, a.toolkit, a.fabric, a.config.Entity, tenantID, sharedPublication(p), func(c context.Context) (learning.Publication, error) {
		prior, readErr := a.config.Repository.GetPublication(c, p.LessonID, p.LocalRevision-1)
		return sharedPublication(prior), readErr
	})
	return row, transportError(err)
}

func transportError(err error) error {
	switch {
	case errors.Is(err, learning.ErrDenied):
		return opt.ErrPolicyDenied
	case errors.Is(err, learning.ErrConflict):
		return opt.ErrConflict
	case errors.Is(err, learning.ErrInvalid):
		return opt.ErrInvalidSpec
	case errors.Is(err, learning.ErrIneligible):
		return opt.ErrIncompleteEvidence
	case errors.Is(err, learning.ErrUnavailable):
		return opt.ErrUnavailable
	default:
		return err
	}
}

// Publish delivers an already accepted local lesson. The outbox remains authoritative.
func (a *Adapter) Publish(ctx context.Context, lesson opt.Lesson) (opt.PublicationReceipt, error) {
	p, err := a.config.Repository.GetPublication(ctx, lesson.ID, lesson.Revision)
	if err != nil {
		return opt.PublicationReceipt{}, err
	}
	p.Payload = lesson
	return a.Deliver(ctx, p)
}

// Revoke delivers an existing local revocation revision. Local withdrawal must
// commit first so a remote outage cannot leave the lesson eligible for recall.
func (a *Adapter) Revoke(ctx context.Context, lessonID id.ID, revision int64) error {
	p, err := a.config.Repository.GetPublication(ctx, lessonID, revision)
	if err != nil {
		return err
	}
	if p.Operation != opt.PublicationRevoke {
		return opt.ErrInvalidSpec
	}
	_, err = a.Deliver(ctx, p)
	return err
}
