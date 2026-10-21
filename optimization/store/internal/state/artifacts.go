package state

import (
	"context"
	"strings"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/internal/canonical"
)

// ListArtifactWork returns pending deletions and artifacts expired at cutoff,
// ordered by immutable registration time and binding digest. Keep cutoff fixed
// across pages. New work behind a cursor is discovered by the next full sweep.
// The output is bounded; durable backends currently hydrate the whole scope.
func (s *Store) ListArtifactWork(ctx context.Context, cutoff time.Time, query optimization.PageQuery) (optimization.Page[optimization.ArtifactRetention], error) {
	if cutoff.IsZero() || cutoff.After(s.now()) {
		return optimization.Page[optimization.ArtifactRetention]{}, optimization.ErrInvalidSpec
	}
	cutoff = cutoff.UTC()
	return readPage(ctx, s, "artifact-work", id.Nil, cutoff, query, func(data *scopeData) ([]pageRow[optimization.ArtifactRetention], error) {
		return selectRows(data.Artifacts, func(value optimization.ArtifactRetention) bool {
			return value.DeletedAt.IsZero() && (value.Deletion != nil || !value.ExpiresAt.After(cutoff))
		}, func(value optimization.ArtifactRetention) time.Time { return value.RegisteredAt }), nil
	})
}

func artifactKey(scope string, ref optimization.ArtifactRef) (string, error) {
	key, err := ref.BindingDigest()
	if err != nil {
		return "", err
	}
	if ref.Scope.Canonical() != scope {
		return "", optimization.ErrPolicyDenied
	}
	return key, nil
}

// GetArtifact reads retention metadata without asserting current availability.
func (s *Store) GetArtifact(ctx context.Context, ref optimization.ArtifactRef) (optimization.ArtifactRetention, error) {
	var empty optimization.ArtifactRetention
	scope, err := contextScope(ctx)
	if err != nil {
		return empty, err
	}
	key, err := artifactKey(scope, ref)
	if err != nil {
		return empty, err
	}
	unlock := s.lockOperation()
	defer unlock()
	if s.closed {
		return empty, optimization.ErrUnavailable
	}
	data, err := s.loadResource(ctx, scope, RecordKey{Kind: "artifacts", Key: key})
	if err != nil {
		return empty, err
	}
	if data != nil {
		if record, exists := data.Artifacts[key]; exists {
			return clone(record)
		}
	}
	return empty, optimization.ErrNotFound
}

func (tx *transaction) RegisterArtifact(request optimization.ArtifactRegistration) error {
	if !tx.active {
		return optimization.ErrConflict
	}
	if err := tx.ctx.Err(); err != nil {
		return err
	}
	key, err := artifactKey(tx.scope, request.Ref)
	if err != nil {
		return err
	}
	if !optimization.ValidOpaqueKey(request.Actor.Subject) || request.CreatedAt.IsZero() || request.CreatedAt.After(tx.now) || !request.ExpiresAt.After(tx.now) || !request.ExpiresAt.After(request.CreatedAt) {
		return optimization.ErrInvalidSpec
	}
	experiment, exists := tx.data.Experiments[request.Ref.ExperimentID.String()]
	if !exists {
		return optimization.ErrNotFound
	}
	if !tx.validLease(experiment, request.Lease) {
		return optimization.ErrConflict
	}
	if prior, found := tx.data.Artifacts[key]; found {
		if prior.Deletion != nil {
			return optimization.ErrStaleVersion
		}
		if !same(prior.Ref, request.Ref) || !prior.CreatedAt.Equal(request.CreatedAt) || !prior.ExpiresAt.Equal(request.ExpiresAt) || prior.RegisteredBy != request.Actor {
			return optimization.ErrConflict
		}
		return nil
	}
	value, err := clone(optimization.ArtifactRetention{Ref: request.Ref, CreatedAt: request.CreatedAt.UTC(), ExpiresAt: request.ExpiresAt.UTC(), RegisteredAt: tx.now, RegisteredBy: request.Actor, Revision: 1})
	if err != nil {
		return err
	}
	payload, err := canonical.Marshal(map[string]string{"artifact_binding": key})
	if err != nil {
		return err
	}
	if tx.data.Artifacts == nil {
		tx.data.Artifacts = make(map[string]optimization.ArtifactRetention)
	}
	tx.data.Artifacts[key] = value
	tx.appendEvent(experiment, "artifact_registered", request.Actor).Payload = payload
	return nil
}

func (tx *transaction) BeginArtifactDeletion(request optimization.ArtifactDeletion) error {
	if !tx.active {
		return optimization.ErrConflict
	}
	if err := tx.ctx.Err(); err != nil {
		return err
	}
	key, err := artifactKey(tx.scope, request.Ref)
	if err != nil {
		return err
	}
	if !optimization.ValidOpaqueKey(request.RequestID) || !optimization.ValidOpaqueKey(request.Actor.Subject) || strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 4096 {
		return optimization.ErrInvalidSpec
	}
	value, exists := tx.data.Artifacts[key]
	if !exists {
		return optimization.ErrNotFound
	}
	if value.Deletion != nil {
		if !same(*value.Deletion, request) {
			return optimization.ErrConflict
		}
		return nil
	}
	experiment, exists := tx.data.Experiments[request.Ref.ExperimentID.String()]
	if !exists {
		return optimization.ErrIncompleteEvidence
	}
	detached, err := clone(request)
	if err != nil {
		return err
	}
	payload, err := canonical.Marshal(map[string]string{"artifact_binding": key, "request_id": request.RequestID})
	if err != nil {
		return err
	}
	value.Deletion, value.InvalidatedAt, value.Revision = &detached, tx.now, 2
	// Build invalidation and all local revocation outbox entries off to the side.
	// A caller that catches an error cannot accidentally commit half this method.
	workingData, err := clone(*tx.data)
	if err != nil {
		return err
	}
	working := *tx
	working.data = &workingData
	working.data.Artifacts[key] = value
	if err := working.revokeArtifactLessons(key, request); err != nil {
		return err
	}
	working.appendEvent(experiment, "artifact_deletion_requested", request.Actor).Payload = payload
	*tx.data = workingData
	return nil
}

func (tx *transaction) CompleteArtifactDeletion(ref optimization.ArtifactRef, requestID string) error {
	if !tx.active {
		return optimization.ErrConflict
	}
	if err := tx.ctx.Err(); err != nil {
		return err
	}
	key, err := artifactKey(tx.scope, ref)
	if err != nil {
		return err
	}
	value, exists := tx.data.Artifacts[key]
	if !exists {
		return optimization.ErrNotFound
	}
	if value.Deletion == nil || value.Deletion.RequestID != requestID {
		return optimization.ErrConflict
	}
	if !value.DeletedAt.IsZero() {
		return nil
	}
	experiment, exists := tx.data.Experiments[ref.ExperimentID.String()]
	if !exists {
		return optimization.ErrIncompleteEvidence
	}
	payload, err := canonical.Marshal(map[string]string{"artifact_binding": key, "request_id": requestID})
	if err != nil {
		return err
	}
	value.DeletedAt, value.Revision = tx.now, 3
	tx.data.Artifacts[key] = value
	tx.appendEvent(experiment, "artifact_deleted", value.Deletion.Actor).Payload = payload
	return nil
}
