package learning

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

// LessonSummary exposes identity and lifecycle without disclosing retained text.
// Read the source result through its current authorized publication path.
type LessonSummary struct {
	Ref            Ref           `json:"ref"`
	Domain         string        `json:"domain"`
	EvidenceClass  EvidenceClass `json:"evidence_class"`
	CreatedAt      time.Time     `json:"created_at"`
	ExpiresAt      time.Time     `json:"expires_at"`
	RevokedAt      time.Time     `json:"revoked_at"`
	SourceRunID    id.AgentRunID `json:"source_run_id"`
	SourceResultID id.ID         `json:"source_result_id"`
	Corrects       *Ref          `json:"corrects,omitempty"`
	SupersededBy   *Ref          `json:"superseded_by,omitempty"`
}

// Review returns metadata even after withdrawal, without restoring eligibility.
func (g *Guard) Review(ctx context.Context, identifier id.ID) (LessonSummary, error) {
	if identifier.IsNil() {
		return LessonSummary{}, ErrInvalid
	}
	if err := g.Authorize(ctx, ActionRead, identifier, ""); err != nil {
		return LessonSummary{}, err
	}
	current, err := g.config.Repository.GetLesson(ctx, identifier)
	if err != nil {
		return LessonSummary{}, err
	}
	if current.ID != identifier || current.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return LessonSummary{}, ErrInvalid
	}
	if authErr := g.Authorize(ctx, ActionRead, identifier, current.Domain); authErr != nil {
		return LessonSummary{}, authErr
	}
	digest, err := Digest(current)
	if err != nil {
		return LessonSummary{}, err
	}
	out := LessonSummary{Ref: Ref{ID: current.ID, Revision: current.Revision, Digest: digest}, Domain: current.Domain, EvidenceClass: current.EvidenceClass, CreatedAt: current.CreatedAt, ExpiresAt: current.ExpiresAt, RevokedAt: current.RevokedAt, Corrects: current.Corrects, SupersededBy: current.SupersededBy}
	if current.Result != nil {
		out.SourceRunID = current.Result.RunID
		out.SourceResultID = current.Result.ResultID
	}
	return out, ctx.Err()
}

// AcceptResult derives the private receipt reference from current storage. You
// supply applicability and a stable lesson ID, never replacement answer text.
// Leave request.Result empty; source selects the governed result to accept.
func (g *Guard) AcceptResult(ctx context.Context, request AcceptRequest, source verification.ResultContentRequest) (PublicationSummary, error) {
	if source.Validate() != nil || request.Result != (verification.ResultReadRequest{}) {
		return PublicationSummary{}, ErrInvalid
	}
	if err := g.Authorize(ctx, ActionPublish, request.ID, request.Domain); err != nil {
		return PublicationSummary{}, err
	}
	if err := g.config.ContentAuthority.AuthorizeResultContent(ctx, source); err != nil {
		return PublicationSummary{}, err
	}
	receipt, err := g.config.Results.GetCommittedResult(ctx, source.ResultID)
	if err != nil {
		return PublicationSummary{}, err
	}
	if receipt.Validate() != nil || receipt.Request.ResultID != source.ResultID || receipt.Request.Current.RunID != source.RunID || receipt.Request.Current.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return PublicationSummary{}, ErrIneligible
	}
	request.Result = verification.ResultReadRequest{SchemaVersion: 1, RunID: source.RunID, ResultID: source.ResultID, ReceiptDigest: receipt.Digest}
	accepted, err := g.Accept(ctx, request)
	if err != nil {
		return PublicationSummary{}, err
	}
	return summarizePublication(accepted), nil
}
