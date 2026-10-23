package engine

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

func (e *Engine) loadRequiredInputs(ctx context.Context, r *run.Run, intent verification.CaptureIntent) (verification.ExecutionInputs, error) {
	storage, ok := e.store.(requiredVerificationStore)
	if !ok {
		return verification.ExecutionInputs{}, cortex.ErrVerificationUnavailable
	}
	if intent.Template.Binding.Context.ExecutionInputs == nil || e.requiredVerification == nil {
		return verification.ExecutionInputs{}, cortex.ErrVerificationUnavailable
	}
	ref := *intent.Template.Binding.Context.ExecutionInputs
	ref.Scope.Levels = append([]cortex.Level(nil), ref.Scope.Levels...)
	want, err := ref.BindingDigest()
	if err != nil {
		return verification.ExecutionInputs{}, cortex.ErrVerificationUnavailable
	}
	offered := ref
	offered.Scope.Levels = append([]cortex.Level(nil), ref.Scope.Levels...)
	if e.requiredVerification.CaptureAuthorizer.AuthorizeEvidence(ctx, evidence.ActionRead, offered) != nil {
		return verification.ExecutionInputs{}, cortex.ErrVerificationUnavailable
	}
	artifact, err := storage.LoadEvidence(ctx, ref)
	if err != nil || artifact.Validate() != nil {
		return verification.ExecutionInputs{}, cortex.ErrVerificationUnavailable
	}
	got, gotErr := artifact.Ref.BindingDigest()
	if gotErr != nil || want != got {
		return verification.ExecutionInputs{}, cortex.ErrVerificationUnavailable
	}
	inputs, err := verification.DecodeExecutionInputs(artifact.Data)
	if err != nil || r.ValidateExecutionInputs(inputs) != nil {
		return verification.ExecutionInputs{}, cortex.ErrVerificationUnavailable
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return verification.ExecutionInputs{}, contextErr
	}
	return inputs, nil
}
