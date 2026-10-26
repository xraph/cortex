package contract

import (
	"context"
	"errors"
	"time"

	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/learning"
	"github.com/xraph/cortex/verification"
)

type adaptiveReviewInput struct {
	ID    string `json:"id"`
	After int64  `json:"after"`
	Limit int    `json:"limit"`
}
type adaptiveReviewOutput struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	engine.AdaptiveReviewPage
}
type learningPageInput struct {
	After string `json:"after"`
	Limit int    `json:"limit"`
}
type learningPageOutput struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	learning.PublicationReviewPage
}
type learningRevokeInput struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Reason   string `json:"reason"`
}
type learningRetryInput struct {
	ID         string `json:"id"`
	Revision   int64  `json:"revision"`
	Generation int64  `json:"generation"`
}
type learningAcceptInput struct {
	ID                string        `json:"id"`
	RunID             string        `json:"run_id"`
	ResultID          string        `json:"result_id"`
	Domain            string        `json:"domain"`
	Applicability     []string      `json:"applicability"`
	Contraindications []string      `json:"contraindications"`
	ExpiresAt         time.Time     `json:"expires_at"`
	Corrects          *learning.Ref `json:"corrects,omitempty"`
}

func adaptiveError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, learning.ErrDenied), errors.Is(err, evidence.ErrDenied), errors.Is(err, verification.ErrResultAccessDenied):
		return denied("Current lesson permission is required")
	case errors.Is(err, learning.ErrInvalid):
		return bad("Invalid lesson request")
	case errors.Is(err, learning.ErrConflict), errors.Is(err, learning.ErrIneligible):
		return &dc.Error{Code: dc.CodeConflict, Message: "The lesson or its source changed. Reload before retrying."}
	case errors.Is(err, learning.ErrNotFound):
		return &dc.Error{Code: dc.CodeNotFound, Message: "The lesson is unavailable in your scope"}
	default:
		return mapError(err)
	}
}

func (s *service) bindAdaptive(d *dispatcher.Dispatcher) error {
	if err := query(d, s, "cognitive.review", func(ctx context.Context, in adaptiveReviewInput) (adaptiveReviewOutput, error) {
		if s.deps.AdaptiveReview == nil {
			return adaptiveReviewOutput{Reason: "Authorized decision review is not installed"}, nil
		}
		key, err := id.ParseAgentRunID(in.ID)
		if err != nil {
			return adaptiveReviewOutput{}, bad("Invalid run ID")
		}
		if in.Limit == 0 {
			in.Limit = 10
		}
		page, err := s.deps.AdaptiveReview.ReviewRun(ctx, key, in.After, in.Limit)
		return adaptiveReviewOutput{Available: err == nil, AdaptiveReviewPage: page}, adaptiveError(err)
	}); err != nil {
		return err
	}
	if err := query(d, s, "learning.publications", func(ctx context.Context, in learningPageInput) (learningPageOutput, error) {
		if s.deps.Learning == nil {
			return learningPageOutput{Reason: "Governed lesson review is not installed"}, nil
		}
		if in.Limit == 0 {
			in.Limit = 25
		}
		page, err := s.deps.Learning.Publications(ctx, in.After, in.Limit)
		return learningPageOutput{Available: err == nil, PublicationReviewPage: page}, adaptiveError(err)
	}); err != nil {
		return err
	}
	if err := query(d, s, "learning.detail", func(ctx context.Context, in IDInput) (learning.LessonSummary, error) {
		if s.deps.Learning == nil {
			return learning.LessonSummary{}, learning.ErrUnavailable
		}
		key, err := id.ParseWithPrefix(in.ID, learning.PrefixLesson)
		if err != nil {
			return learning.LessonSummary{}, bad("Invalid lesson ID")
		}
		out, err := s.deps.Learning.Review(ctx, key)
		return out, adaptiveError(err)
	}); err != nil {
		return err
	}
	if err := command(d, s, "learning.revoke", "manage", func(ctx context.Context, in learningRevokeInput) (learning.PublicationSummary, error) {
		if s.deps.Learning == nil {
			return learning.PublicationSummary{}, learning.ErrUnavailable
		}
		key, err := id.ParseWithPrefix(in.ID, learning.PrefixLesson)
		if err != nil {
			return learning.PublicationSummary{}, bad("Invalid lesson ID")
		}
		principal, ok := cortex.PrincipalFromContext(ctx).(dc.Principal)
		if !ok || principal.User == nil {
			return learning.PublicationSummary{}, denied("An authenticated principal is required")
		}
		out, err := s.deps.Learning.Revoke(ctx, learning.RevokeRequest{ID: key, ExpectedRevision: in.Revision, Actor: principal.User.Subject, Reason: in.Reason})
		return learning.PublicationSummary{LessonID: out.LessonID, Revision: out.Revision, Operation: out.Operation}, adaptiveError(err)
	}); err != nil {
		return err
	}
	if err := command(d, s, "learning.retry", "manage", func(ctx context.Context, in learningRetryInput) (map[string]bool, error) {
		if s.deps.Learning == nil {
			return nil, learning.ErrUnavailable
		}
		key, err := id.ParseWithPrefix(in.ID, learning.PrefixLesson)
		if err != nil {
			return nil, bad("Invalid lesson ID")
		}
		err = s.deps.Learning.Retry(ctx, key, in.Revision, in.Generation)
		return map[string]bool{"queued": err == nil}, adaptiveError(err)
	}); err != nil {
		return err
	}
	return command(d, s, "learning.accept", "manage", func(ctx context.Context, in learningAcceptInput) (learning.PublicationSummary, error) {
		if s.deps.Learning == nil {
			return learning.PublicationSummary{}, learning.ErrUnavailable
		}
		key, err := id.ParseWithPrefix(in.ID, learning.PrefixLesson)
		if err != nil {
			return learning.PublicationSummary{}, bad("Invalid lesson ID")
		}
		source, err := verification.ParseResultContentRequest(in.RunID, in.ResultID)
		if err != nil {
			return learning.PublicationSummary{}, bad("Invalid source result")
		}
		principal, ok := cortex.PrincipalFromContext(ctx).(dc.Principal)
		if !ok || principal.User == nil {
			return learning.PublicationSummary{}, denied("An authenticated principal is required")
		}
		out, err := s.deps.Learning.AcceptResult(ctx, learning.AcceptRequest{ID: key, Domain: in.Domain, Applicability: in.Applicability, Contraindications: in.Contraindications, ExpiresAt: in.ExpiresAt, Actor: principal.User.Subject, Corrects: in.Corrects}, source)
		return out, adaptiveError(err)
	})
}
