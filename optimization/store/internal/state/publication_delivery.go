package state

import (
	"math"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
)

func (tx *transaction) ClaimPublication(request optimization.PublicationClaim) (optimization.Publication, error) {
	var empty optimization.Publication
	if !tx.active {
		return empty, optimization.ErrConflict
	}
	if err := tx.ctx.Err(); err != nil {
		return empty, err
	}
	if _, err := optimization.ParseID(request.LessonID.String(), optimization.PrefixLesson); err != nil {
		return empty, err
	}
	if request.LocalRevision < 1 || !optimization.ValidOpaqueKey(request.Owner) || !optimization.ValidOpaqueKey(request.Actor.Subject) || request.Duration <= 0 || request.Duration > 5*time.Minute {
		return empty, optimization.ErrInvalidSpec
	}
	key := publicationKey(request.LessonID, request.LocalRevision)
	value, ok := tx.data.Publications[key]
	if !ok {
		return empty, optimization.ErrNotFound
	}
	if !value.CompletedAt.IsZero() || !value.SupersededAt.IsZero() || value.TerminalError != "" || tx.now.Before(value.NextRetry) || tx.now.Before(value.DeliveryLease.ExpiresAt) {
		return empty, optimization.ErrConflict
	}
	if value.Attempts == math.MaxInt || value.DeliveryLease.Generation == math.MaxInt64 {
		return empty, optimization.ErrConflict
	}
	if value.Operation == optimization.PublicationPublish {
		ref := optimization.LessonRef{ID: value.LessonID, Revision: value.LocalRevision, Digest: value.PayloadDigest}
		if err := checkLessonBindings(tx.data, optimization.Experiment{LessonIDs: []id.ID{value.LessonID}, FrozenLessons: []optimization.LessonRef{ref}}, tx.now); err != nil {
			return empty, err
		}
	}
	value.Attempts++
	value.DeliveryLease = optimization.PublicationLease{LessonID: value.LessonID, LocalRevision: value.LocalRevision, Owner: request.Owner, Generation: value.DeliveryLease.Generation + 1, ExpiresAt: tx.now.Add(request.Duration)}
	value.DeliveryActor = request.Actor
	tx.data.Publications[key] = value
	tx.publicationEvent(value, "lesson_publication_claimed")
	return clone(value)
}

func (tx *transaction) CompletePublication(lease optimization.PublicationLease, receipt optimization.PublicationReceipt) error {
	value, err := tx.publicationDelivery(lease)
	if err != nil {
		return err
	}
	if receipt.LessonID != value.LessonID || receipt.PayloadDigest != value.PayloadDigest || receipt.AggregateID != value.RemoteAggregateID || receipt.Version < 1 || receipt.AcknowledgedAt.IsZero() || receipt.AcknowledgedAt.After(tx.now) {
		return optimization.ErrIncompleteEvidence
	}
	if !value.CompletedAt.IsZero() {
		if value.Receipt == nil || !same(*value.Receipt, receipt) {
			return optimization.ErrConflict
		}
		return nil
	}
	if !tx.now.Before(lease.ExpiresAt) {
		return optimization.ErrConflict
	}
	if value.Operation == optimization.PublicationRevoke && value.RemoteVersion > 0 && receipt.Version < value.RemoteVersion {
		return optimization.ErrIncompleteEvidence
	}
	value.Receipt = &receipt
	value.RemoteVersion = receipt.Version
	value.CompletedAt = tx.now
	value.NextRetry = time.Time{}
	value.LastErrorCode = ""
	tx.data.Publications[publicationKey(value.LessonID, value.LocalRevision)] = value
	tx.publicationEvent(value, "lesson_publication_delivered")
	return nil
}

func (tx *transaction) FailPublication(lease optimization.PublicationLease, code string, retryAt time.Time, terminal bool) error {
	value, err := tx.publicationDelivery(lease)
	if err != nil {
		return err
	}
	if !value.CompletedAt.IsZero() || !tx.now.Before(lease.ExpiresAt) {
		return optimization.ErrConflict
	}
	switch code {
	case "remote_unavailable", "evidence_unavailable", "policy_denied", "conflict", "invalid_receipt":
	default:
		return optimization.ErrInvalidSpec
	}
	if terminal && !retryAt.IsZero() || !terminal && (!retryAt.After(tx.now) || retryAt.After(tx.now.Add(24*time.Hour))) {
		return optimization.ErrInvalidSpec
	}
	value.LastErrorCode = code
	value.DeliveryLease.ExpiresAt = tx.now
	value.NextRetry = retryAt
	event := "lesson_publication_retry_scheduled"
	if terminal {
		value.TerminalError = code
		event = "lesson_publication_failed"
	}
	tx.data.Publications[publicationKey(value.LessonID, value.LocalRevision)] = value
	tx.publicationEvent(value, event)
	return nil
}

func (tx *transaction) publicationDelivery(lease optimization.PublicationLease) (optimization.Publication, error) {
	var empty optimization.Publication
	if !tx.active {
		return empty, optimization.ErrConflict
	}
	if err := tx.ctx.Err(); err != nil {
		return empty, err
	}
	value, ok := tx.data.Publications[publicationKey(lease.LessonID, lease.LocalRevision)]
	if !ok {
		return empty, optimization.ErrNotFound
	}
	if lease.Generation < 1 || !same(value.DeliveryLease, lease) || !value.SupersededAt.IsZero() || value.TerminalError != "" {
		return empty, optimization.ErrConflict
	}
	return value, nil
}

func (tx *transaction) publicationEvent(value optimization.Publication, eventType string) {
	experiment := tx.data.Experiments[value.ExperimentID.String()]
	event := tx.appendEvent(experiment, eventType, value.DeliveryActor)
	event.Resources = append(event.Resources, value.LessonID)
}
