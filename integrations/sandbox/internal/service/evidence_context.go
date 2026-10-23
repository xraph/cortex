package service

import (
	"context"
	"slices"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/sandbox"
)

type evidenceContextKey struct{}
type evidenceAuthority struct {
	job     sandbox.Job
	actor   string
	cleanup bool
}

func evidenceContext(ctx context.Context, job sandbox.Job) context.Context {
	return bindEvidenceContext(ctx, job, "", false)
}

func cleanupEvidenceContext(ctx context.Context, job sandbox.Job, actor string) context.Context {
	return bindEvidenceContext(ctx, job, actor, true)
}

func bindEvidenceContext(ctx context.Context, job sandbox.Job, actor string, cleanup bool) context.Context {
	// Shadow inherited authority even when validation fails. Seal only after
	// validating the original digest, so mutation cannot become a new grant.
	ctx = context.WithValue(ctx, evidenceContextKey{}, evidenceAuthority{})
	if ctx.Err() != nil || job.Validate() != nil || (cleanup && !validCleanupText(actor)) {
		return ctx
	}
	candidate, err := sandbox.Seal(job)
	if err != nil {
		return ctx
	}
	ctx = cortex.WithScope(ctx, cortex.Scope{Levels: slices.Clone(candidate.Scope.Levels)})
	principal := candidate.Principal
	if cleanup {
		principal = actor
	}
	ctx = cortex.WithPrincipal(ctx, principal)
	ctx = evidence.WithDeletionActor(ctx, actor)
	return context.WithValue(ctx, evidenceContextKey{}, evidenceAuthority{job: candidate, actor: actor, cleanup: cleanup})
}

func boundEvidenceAuthority(ctx context.Context, ref evidence.Ref) (evidenceAuthority, error) {
	if err := ctx.Err(); err != nil {
		return evidenceAuthority{}, err
	}
	authority, ok := ctx.Value(evidenceContextKey{}).(evidenceAuthority)
	if !ok || authority.job.Validate() != nil {
		return evidenceAuthority{}, evidence.ErrDenied
	}
	if err := ref.Validate(); err != nil {
		return evidenceAuthority{}, err
	}
	principal := authority.job.Principal
	if authority.cleanup {
		principal = authority.actor
	}
	subject, ok := cortex.PrincipalFromContext(ctx).(string)
	scope := cortex.ScopeFromContext(ctx).Canonical()
	if !ok || subject != principal || evidence.DeletionActor(ctx) != authority.actor || scope != authority.job.Scope.Canonical() || scope != ref.Scope.Canonical() {
		return evidenceAuthority{}, evidence.ErrDenied
	}
	return authority, nil
}
