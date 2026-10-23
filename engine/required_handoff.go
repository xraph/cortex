package engine

import (
	"context"
	"slices"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

// ToolHandoffAuthorizer checks current access to private pending calls and
// permission to supply their outcomes. It is separate from tool dispatch policy.
// The host must check the context principal and exact run scope on every call.
type ToolHandoffAuthorizer interface {
	AuthorizeToolHandoff(context.Context, cortex.Subject) error
}

// PendingVerificationTools returns private pending calls only after current host
// authorization. Ordinary run responses and events omit these arguments and IDs.
// Reading does not claim a suspension or authorize executing a tool. Raw Store
// access remains a trusted host capability outside this authorization boundary.
func (e *Engine) PendingVerificationTools(ctx context.Context, runID id.AgentRunID) ([]suspension.PendingCall, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	r, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if !r.Provenance.VerificationRequired || r.State != run.StatePaused {
		return nil, cortex.ErrVerificationUnavailable
	}
	if authErr := e.authorizeHandoff(ctx, r); authErr != nil {
		return nil, authErr
	}
	s, err := e.store.GetSuspension(ctx, runID)
	if err != nil {
		return nil, err
	}
	if resumeErr := e.checkRequiredResume(ctx, r, s); resumeErr != nil {
		return nil, resumeErr
	}
	return slices.Clone(s.Pending), nil
}

func (e *Engine) authorizeHandoff(ctx context.Context, r *run.Run) error {
	if e.requiredVerification == nil || e.requiredVerification.HandoffAuthorizer == nil || cortex.ScopeFromContext(ctx).Canonical() != r.Scope.Canonical() {
		return cortex.ErrVerificationUnavailable
	}
	scope := r.Scope
	scope.Levels = slices.Clone(scope.Levels)
	subject := cortex.Subject{Scope: scope, Principal: cortex.PrincipalFromContext(ctx), AgentID: r.AgentID, RunID: r.ID}
	if err := e.requiredVerification.HandoffAuthorizer.AuthorizeToolHandoff(ctx, subject); err != nil {
		return cortex.ErrVerificationUnavailable
	}
	return ctx.Err()
}

func (e *Engine) checkRequiredResume(ctx context.Context, r *run.Run, s *suspension.Suspension) error {
	_, snap, err := decodeContinuation(s.Cont)
	if err != nil {
		return err
	}
	required := r.Provenance.VerificationRequired || r.Provenance.VerificationIntentID != "" || e.requiredVerification != nil || (snap != nil && snap.Config.Verification != nil)
	if !required {
		return nil
	}
	if snap == nil || snap.SchemaVersion != requiredToolsSchemaVersion || !r.Provenance.VerificationRequired || r.Provenance.VerificationIntentID == "" || s.RunID != r.ID || s.Scope.Canonical() != r.Scope.Canonical() {
		return cortex.ErrVerificationUnavailable
	}
	if policyErr := e.requireVerification(snap); policyErr != nil {
		return policyErr
	}
	if continuationErr := validateFrozenContinuation(s.Cont, snap, r); continuationErr != nil {
		return continuationErr
	}
	if authErr := e.authorizeHandoff(ctx, r); authErr != nil {
		return authErr
	}
	storage, ok := e.store.(requiredVerificationStore)
	if !ok {
		return cortex.ErrVerificationUnavailable
	}
	intent, err := storage.GetRunCaptureIntent(ctx, r.ID)
	if err != nil || intent.Validate() != nil {
		return cortex.ErrVerificationUnavailable
	}
	req, p := intent.Template, snap.Config.Verification
	if intent.ID.String() != r.Provenance.VerificationIntentID || !requiredIntentMatchesPolicy(intent, p, r) || r.Provenance.AcceptanceRef != p.ContractID.String() || req.Deadline.After(r.Budget.Deadline) || !e.verificationWorker.config.Clock.Now().Before(req.Deadline) {
		return cortex.ErrVerificationUnavailable
	}
	return e.authorizeCandidate(ctx, req.Binding.Candidate)
}

func publicPendingCalls(r *run.Run, calls []suspension.PendingCall) []suspension.PendingCall {
	if r.Provenance.VerificationRequired {
		return nil
	}
	return calls
}
