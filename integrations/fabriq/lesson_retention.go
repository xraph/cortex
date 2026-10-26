package fabriqbrain

import (
	"context"
	"errors"

	"github.com/xraph/fabriq/core/command"
	"github.com/xraph/fabriq/core/fabriqerr"
	"github.com/xraph/fabriq/core/tenant"

	"github.com/xraph/cortex/integrations/fabriq/internal/lessontransport"
	"github.com/xraph/cortex/learning"
)

// LessonDeletion distinguishes active removal and named local payload erasure.
// HistoryErased remains false because downstream copies are not confirmed.
type LessonDeletion struct {
	Tombstone           learning.Tombstone
	ActiveCopiesRemoved bool
	HistoryErased       bool
	LocalPayloadsErased bool
	LocalErasure        *command.ErasureReceipt
}

// Delete erases local payloads, then removes the authoritative remote row and
// vector using the installed tenant-scoped command API. Repeating this call
// reconciles lost acknowledgments. An optional configured AggregateEraser also
// removes the named local history targets and irreversibly reserves the identity.
// Downstream copies remain unconfirmed.
func (a *LessonAdapter) Delete(ctx context.Context, request learning.DeletionRequest) (LessonDeletion, error) {
	tombstone, err := a.config.Guard.Delete(ctx, request)
	if err != nil {
		return LessonDeletion{}, err
	}
	result := LessonDeletion{Tombstone: tombstone}
	mapped, err := a.mapped(ctx)
	if err != nil {
		return result, err
	}
	row, readErr := lessontransport.Read(mapped, a.fabric, a.config.Entity, tombstone.AggregateID)
	if readErr != nil && !errors.Is(readErr, fabriqerr.ErrNotFound) {
		return result, learning.ErrUnavailable
	}
	if readErr == nil {
		tenantID, tenantErr := tenant.FromContext(mapped)
		if tenantErr != nil {
			return result, tenantErr
		}
		if row.Tenant != tenantID || row.ScopeKey != tombstone.Scope.Canonical() || row.Lesson.ID != tombstone.ID || row.LocalRevision > tombstone.Revision {
			return result, learning.ErrConflict
		}
		_, writeErr := a.fabric.Exec(mapped, command.Command{Entity: a.config.Entity, Op: command.OpDelete, AggID: tombstone.AggregateID, ExpectedVersion: &row.Version})
		_, afterErr := lessontransport.Read(mapped, a.fabric, a.config.Entity, tombstone.AggregateID)
		if !errors.Is(afterErr, fabriqerr.ErrNotFound) {
			return result, errors.Join(learning.ErrUnavailable, writeErr)
		}
	}
	if eraser, supported := a.fabric.(command.AggregateEraser); supported {
		return a.eraseLessonHistory(ctx, mapped, eraser, result)
	}
	if err = a.fabric.Vector().Delete(mapped, a.config.Entity, tombstone.AggregateID); err != nil && !errors.Is(err, fabriqerr.ErrNotFound) {
		return result, learning.ErrUnavailable
	}
	if authErr := a.config.Guard.Authorize(ctx, learning.ActionDelete, tombstone.ID, ""); authErr != nil {
		return result, authErr
	}
	result.ActiveCopiesRemoved = true
	return result, ctx.Err()
}

// SweepDeleted resumes active-copy cleanup from durable local tombstones. Hosts
// schedule this bounded scan with their retention worker's current delete grant.
// Replaying old tombstones is safe and also removes delayed destination writes.
func (a *LessonAdapter) SweepDeleted(ctx context.Context, after string) (string, error) {
	repository, ok := a.config.Repository.(learning.RetentionStore)
	if !ok {
		return "", learning.ErrUnavailable
	}
	page, err := repository.ListLessonTombstones(ctx, after, 100)
	if err != nil {
		return "", err
	}
	var failures []error
	for _, item := range page.Items {
		if contextErr := ctx.Err(); contextErr != nil {
			return after, contextErr
		}
		_, deleteErr := a.Delete(ctx, learning.DeletionRequest{ID: item.ID, ExpectedRevision: item.Revision, Actor: item.Actor, Reason: item.Reason})
		if deleteErr != nil {
			failures = append(failures, deleteErr)
		}
	}
	return page.After, errors.Join(failures...)
}
