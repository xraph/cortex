package optimization

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/fabriqerr"
	"github.com/xraph/fabriq/core/tenant"

	"github.com/xraph/cortex/id"
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
	row, readErr := a.read(ctx, p.RemoteAggregateID)
	if readErr == nil && row.matches(p, tenantID) {
		return row, nil
	}
	if readErr != nil && !errors.Is(readErr, fabriqerr.ErrNotFound) {
		return lessonRow{}, opt.ErrUnavailable
	}
	operation, version := "create", int64(0)
	var previous *opt.Publication
	if readErr == nil {
		if p.Operation != opt.PublicationRevoke {
			return lessonRow{}, opt.ErrConflict
		}
		prior, err := a.config.Repository.GetPublication(ctx, p.LessonID, p.LocalRevision-1)
		if err != nil {
			return lessonRow{}, err
		}
		if prior.Operation != opt.PublicationPublish || !row.matches(prior, tenantID) {
			return lessonRow{}, opt.ErrConflict
		}
		operation, version = "update", row.Version
		previous = &prior
	}
	payload, err := json.Marshal(rowFor(p))
	if err != nil {
		return lessonRow{}, opt.ErrInvalidSpec
	}
	_, writeErr := a.toolkit.Remember(ctx, agent.RememberRequest{Entity: a.config.Entity, Op: operation, AggID: p.RemoteAggregateID, ExpectedVersion: &version, Payload: payload})
	// A successful response and an ambiguous write require the same source lookup.
	// A version conflict alone never proves this payload was published.
	row, readErr = a.read(ctx, p.RemoteAggregateID)
	if readErr != nil {
		var denied *agent.WriteError
		if errors.As(writeErr, &denied) && denied.Code == "not_allowed" {
			return lessonRow{}, opt.ErrPolicyDenied
		}
		return lessonRow{}, opt.ErrUnavailable
	}
	if !row.matches(p, tenantID) {
		if previous != nil && row.Version == version && row.matches(*previous, tenantID) && writeErr != nil {
			var denied *agent.WriteError
			if errors.As(writeErr, &denied) && denied.Code == "not_allowed" {
				return lessonRow{}, opt.ErrPolicyDenied
			}
			return lessonRow{}, opt.ErrUnavailable
		}
		return lessonRow{}, opt.ErrConflict
	}
	return row, nil
}

func (a *Adapter) read(ctx context.Context, aggregate string) (lessonRow, error) {
	var rows []map[string]any
	if err := a.fabric.Relational().GetMany(ctx, a.config.Entity, []string{aggregate}, &rows); err != nil {
		return lessonRow{}, err
	}
	if len(rows) == 0 {
		return lessonRow{}, fabriqerr.ErrNotFound
	}
	if len(rows) != 1 {
		return lessonRow{}, opt.ErrIncompleteEvidence
	}
	return decodeRow(rows[0])
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
