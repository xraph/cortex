package fabriqbrain

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xraph/fabriq/core/command"
	"github.com/xraph/fabriq/core/fabriqerr"
	"github.com/xraph/fabriq/core/tenant"

	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/learning"
)

func (a *LessonAdapter) eraseLessonHistory(ctx, mapped context.Context, eraser command.AggregateEraser, result LessonDeletion) (LessonDeletion, error) {
	if hostport.IsNil(eraser) {
		return result, learning.ErrUnavailable
	}
	if err := a.config.Guard.Authorize(ctx, learning.ActionDelete, result.Tombstone.ID, ""); err != nil {
		return result, err
	}
	tid, err := tenant.Require(mapped)
	if err != nil {
		return result, err
	}
	if mappingErr := a.checkErasureMapping(ctx, mapped); mappingErr != nil {
		return result, mappingErr
	}
	request := command.ErasureRequest{Entity: a.config.Entity, AggregateID: result.Tombstone.AggregateID, ScopeID: tenant.ScopeOrEmpty(mapped), ExpectedVersion: 0, ReasonCode: "cortex_retention"}
	if err = request.Validate(mapped); err != nil {
		return result, learning.ErrInvalid
	}
	receipt, err := eraser.EraseAggregate(mapped, request)
	if err != nil {
		return result, lessonErasureError(err)
	}
	// Freeze the facade's slices before validation or any host callback.
	receipt.LocalTargets = slices.Clone(receipt.LocalTargets)
	receipt.Downstream = slices.Clone(receipt.Downstream)
	if validationErr := validateLessonErasure(tid, request, receipt, a.config.Now().UTC()); validationErr != nil {
		return result, validationErr
	}
	if authErr := a.config.Guard.Authorize(ctx, learning.ActionDelete, result.Tombstone.ID, ""); authErr != nil {
		return result, authErr
	}
	if mappingErr := a.checkErasureMapping(ctx, mapped); mappingErr != nil {
		return result, mappingErr
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return result, contextErr
	}
	result.ActiveCopiesRemoved = true
	result.LocalPayloadsErased = true
	result.LocalErasure = &receipt
	return result, nil
}

func validateLessonErasure(tid string, request command.ErasureRequest, r command.ErasureReceipt, now time.Time) error {
	if r.TenantID != tid || r.Request != request || !r.LocalPayloadsErased || r.LastVersion < 0 || r.OutboxRowsRedacted < 0 || r.ErasedAt.IsZero() || r.ErasedAt.After(now) {
		return learning.ErrConflict
	}
	for _, value := range []string{r.Actor, r.AuthorityRevision} {
		if value == "" || len(value) > 512 || strings.TrimSpace(value) != value || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
			return learning.ErrConflict
		}
	}
	targets := map[string]bool{"aggregate_row": true, "command_outbox": true, "embedding": true, "spatial": true}
	if len(r.LocalTargets) != len(targets) {
		return learning.ErrConflict
	}
	for _, target := range r.LocalTargets {
		if !targets[target] {
			return learning.ErrConflict
		}
		delete(targets, target)
	}
	downstream := map[string]bool{"streams": true, "graph": true, "search": true, "cache": true, "live_subscribers": true, "backups": true}
	if len(r.Downstream) != len(downstream) {
		return learning.ErrConflict
	}
	for _, target := range r.Downstream {
		if !downstream[target.Target] || target.State != "unconfirmed" {
			return learning.ErrConflict
		}
		delete(downstream, target.Target)
	}
	return nil
}

func (a *LessonAdapter) checkErasureMapping(ctx, mapped context.Context) error {
	current, err := a.mapped(ctx)
	if err != nil {
		return err
	}
	expected, err := tenant.Require(mapped)
	if err != nil {
		return err
	}
	actual, err := tenant.Require(current)
	if err != nil {
		return err
	}
	if actual != expected || tenant.ScopeOrEmpty(current) != tenant.ScopeOrEmpty(mapped) {
		return learning.ErrDenied
	}
	return nil
}

func lessonErasureError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, learning.ErrDenied):
		return learning.ErrDenied
	case errors.Is(err, learning.ErrConflict), errors.Is(err, fabriqerr.ErrVersionConflict):
		return learning.ErrConflict
	case errors.Is(err, learning.ErrInvalid):
		return learning.ErrInvalid
	}
	switch fabriqerr.CodeOf(err) {
	case fabriqerr.CodeUnauthorized, fabriqerr.CodePermissionDenied:
		return learning.ErrDenied
	case fabriqerr.CodeInvalidInput:
		return learning.ErrInvalid
	default:
		return learning.ErrUnavailable
	}
}
