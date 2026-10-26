package learning

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// PublicationSummary excludes lesson text, actor input and retained result bytes.
// Acknowledged describes destination delivery, not current recall eligibility.
type PublicationSummary struct {
	LessonID      id.ID     `json:"lesson_id"`
	Revision      int64     `json:"revision"`
	Domain        string    `json:"domain"`
	Operation     string    `json:"operation"`
	PayloadDigest string    `json:"payload_digest"`
	Generation    int64     `json:"generation"`
	Attempts      int       `json:"attempts"`
	LeaseUntil    time.Time `json:"lease_until"`
	NextAttempt   time.Time `json:"next_attempt"`
	ErrorCode     string    `json:"error_code,omitempty"`
	Acknowledged  bool      `json:"acknowledged"`
}

// PublicationReviewPage preserves the store's bounded exact-scope cursor.
type PublicationReviewPage struct {
	Items []PublicationSummary `json:"items"`
	After string               `json:"after,omitempty"`
}

func summarizePublication(p Publication) PublicationSummary {
	return PublicationSummary{LessonID: p.LessonID, Revision: p.Revision, Domain: p.Payload.Domain, Operation: p.Operation, PayloadDigest: p.PayloadDigest, Generation: p.Generation, Attempts: p.Attempts, LeaseUntil: p.LeaseUntil, NextAttempt: p.NextAttempt, ErrorCode: p.ErrorCode, Acknowledged: p.Receipt != nil}
}

// Publications returns delivery metadata under current lesson grants. Source
// content is available only through Resolve and its result-evidence fence.
func (g *Guard) Publications(ctx context.Context, after string, limit int) (PublicationReviewPage, error) {
	out := PublicationReviewPage{Items: []PublicationSummary{}}
	if limit < 1 || limit > 100 || len(after) > 128 {
		return out, ErrInvalid
	}
	if err := g.Authorize(ctx, ActionRead, id.ID{}, ""); err != nil {
		return out, err
	}
	page, err := g.config.Repository.ListLessonPublications(ctx, after, limit)
	if err != nil {
		return out, err
	}
	if len(page.Items) > limit {
		return out, ErrInvalid
	}
	for _, item := range page.Items {
		if item.Payload.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() || item.LessonID != item.Payload.ID {
			return PublicationReviewPage{}, ErrInvalid
		}
		if authErr := g.Authorize(ctx, ActionRead, item.LessonID, item.Payload.Domain); authErr != nil {
			return PublicationReviewPage{}, authErr
		}
		out.Items = append(out.Items, summarizePublication(item))
	}
	if authErr := g.Authorize(ctx, ActionRead, id.ID{}, ""); authErr != nil {
		return PublicationReviewPage{}, authErr
	}
	for _, item := range page.Items {
		if authErr := g.Authorize(ctx, ActionRead, item.LessonID, item.Payload.Domain); authErr != nil {
			return PublicationReviewPage{}, authErr
		}
	}
	out.After = page.After
	return out, ctx.Err()
}

// Retry requests one new delivery cycle at the exact retained generation. It
// does not restore eligibility, extend retention or bypass delivery-time grants.
func (g *Guard) Retry(ctx context.Context, identifier id.ID, revision, generation int64) error {
	if identifier.IsNil() || revision < 1 || generation < 1 {
		return ErrInvalid
	}
	if err := g.Authorize(ctx, ActionRetry, identifier, ""); err != nil {
		return err
	}
	publication, err := g.config.Repository.GetLessonPublication(ctx, identifier, revision)
	if err != nil {
		return err
	}
	if publication.LessonID != identifier || publication.Revision != revision || publication.Generation != generation || publication.Payload.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return ErrConflict
	}
	if authErr := g.Authorize(ctx, ActionRetry, identifier, publication.Payload.Domain); authErr != nil {
		return authErr
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	return g.config.Repository.RetryLessonPublication(ctx, identifier, revision, generation)
}
