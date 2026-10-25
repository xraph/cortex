package optimization

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/query"
	"github.com/xraph/fabriq/core/tenant"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
)

var _ opt.LessonStore = (*Adapter)(nil)
var _ opt.PublicationSink = (*Adapter)(nil)

// Recall returns a bounded semantic result set. A nonempty cursor is refused:
// changing search rankings cannot supply a stable continuation. Missing required
// search channels or token-truncated results return ErrIncompleteEvidence.
func (a *Adapter) Recall(ctx context.Context, request opt.RecallQuery) (opt.LessonPage, error) {
	result := opt.LessonPage{Items: []opt.Lesson{}}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	key, err := opt.ScopeKey(cortex.ScopeFromContext(ctx))
	if err != nil {
		return result, err
	}
	requested, err := opt.ScopeKey(request.Scope)
	if err != nil || requested != key {
		return result, opt.ErrPolicyDenied
	}
	now := a.config.Now()
	if strings.TrimSpace(request.Domain) == "" || strings.TrimSpace(request.Query) == "" || len(request.Query) > 32768 || request.HarnessSchema != 1 || request.Limit < 1 || request.Limit > 100 || request.Cutoff.IsZero() || request.Cutoff.After(now) || request.Cursor != "" {
		return result, opt.ErrInvalidSpec
	}
	if authErr := a.config.Authorizer.Authorize(ctx, opt.ActionRead, opt.Resource{Scope: request.Scope, Kind: "lesson"}); authErr != nil {
		return result, authErr
	}
	mapped, err := a.mapped(ctx)
	if err != nil {
		return result, err
	}
	tenantID, err := tenant.FromContext(mapped)
	if err != nil {
		return result, err
	}
	pack, err := a.toolkit.Recall(mapped, agent.RecallRequest{Query: request.Query, Budget: 128000, K: request.Limit, Entities: []string{a.config.Entity}, Filters: query.Where{
		query.Eq("scope_key", key), query.Eq("domain", request.Domain), query.Eq("harness_schema", request.HarnessSchema), query.Eq("active", true), query.Lte("created_at", request.Cutoff), query.Or(query.IsNull("expires_at"), query.Gt("expires_at", now)),
	}})
	if err != nil {
		return result, opt.ErrUnavailable
	}
	if len(pack.Warnings) > 0 || pack.Omitted > 0 || len(pack.Items) > request.Limit {
		return result, opt.ErrIncompleteEvidence
	}
	seen := make(map[string]bool)
	for _, item := range pack.Items {
		if item.Entity != a.config.Entity || seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		var raw map[string]any
		if json.Unmarshal(item.Row, &raw) != nil {
			continue
		}
		row, decodeErr := decodeRow(raw)
		if decodeErr != nil || row.ID != item.ID || row.ScopeKey != key || row.Domain != request.Domain || row.HarnessSchema != request.HarnessSchema || !row.Active || row.CreatedAt.After(request.Cutoff) || row.ExpiresAt != nil && !now.Before(*row.ExpiresAt) {
			continue
		}
		p, lookupErr := a.config.Repository.GetPublication(ctx, row.Lesson.ID, row.LocalRevision)
		if errors.Is(lookupErr, opt.ErrNotFound) || errors.Is(lookupErr, opt.ErrStaleVersion) {
			continue
		}
		if lookupErr != nil {
			return opt.LessonPage{}, lookupErr
		}
		if !row.matches(p, tenantID) {
			continue
		}
		resolved, cleanup, authErr := a.authorize(ctx, p, opt.ActionRead)
		if authErr != nil {
			return opt.LessonPage{}, authErr
		}
		lesson, resolveErr := a.config.Repository.ResolveLesson(resolved, opt.LessonRef{ID: p.LessonID, Revision: p.LocalRevision, Digest: p.PayloadDigest})
		cleanup()
		if errors.Is(resolveErr, opt.ErrNotFound) || errors.Is(resolveErr, opt.ErrStaleVersion) {
			continue
		}
		if resolveErr != nil {
			return opt.LessonPage{}, resolveErr
		}
		if lesson.CreatedAt.After(request.Cutoff) || !lesson.ExpiresAt.IsZero() && !now.Before(lesson.ExpiresAt) || !lesson.RevokedAt.IsZero() {
			continue
		}
		result.Items = append(result.Items, lesson)
	}
	return result, nil
}
