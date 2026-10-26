package engine

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

// Generation completion does not finish a child whose verification is pending.
// This shared check also covers fresh execution after a lost capture response.
func requiredChildResultError(child *run.Run) error {
	if child == nil || child.State != run.StateCompleted || (!child.Provenance.VerificationRequired && child.Provenance.VerificationIntentID == "") {
		return nil
	}
	if child.VerificationStatus == verification.StatusPending {
		return cortex.ErrOrchestrationWaiting
	}
	resultID, err := id.Parse(child.ResultRef)
	if child.VerificationStatus != verification.StatusPassed || err != nil || resultID.Prefix() != verification.PrefixResult {
		return cortex.ErrRunUnverified
	}
	return nil
}

// recoveredTeamResult checks the frozen requirement before exposing saved output.
// Passing metadata alone cannot replace current result access, evidence validity
// and publication checks. A redacted projection is not the verified candidate.
func (e *Engine) recoveredTeamResult(ctx context.Context, child *run.Run, snap *preparedSnapshot) (*run.Run, error) {
	required := snap.Config.Verification != nil
	if child.Provenance.VerificationRequired != required || (!required && child.Provenance.VerificationIntentID != "") {
		return nil, cortex.ErrInvalidPrepared
	}
	if !required {
		return child, nil
	}
	intentID, err := id.Parse(child.Provenance.VerificationIntentID)
	if err != nil || intentID.Prefix() != verification.PrefixCaptureIntent || child.Provenance.AcceptanceRef != snap.Config.Verification.ContractID.String() {
		return nil, cortex.ErrInvalidPrepared
	}
	if child.State != run.StateCompleted || child.VerificationStatus != verification.StatusPassed {
		return child, nil
	}
	if verificationErr := requiredChildResultError(child); verificationErr != nil {
		return nil, verificationErr
	}
	request, err := verification.ParseResultContentRequest(child.ID.String(), child.ResultRef)
	if err != nil {
		return nil, cortex.ErrRunUnverified
	}
	result, err := e.ReadResultContent(ctx, request)
	if err != nil {
		return nil, err
	}
	if result.RunID != child.ID || result.RunRevision != child.Revision || result.ResultID != request.ResultID || result.VerificationStatus != verification.StatusPassed || result.ContentChanged {
		return nil, cortex.ErrRunUnverified
	}
	if observationErr := e.checkOrchestrationObservation(ctx, child, snap); observationErr != nil {
		return nil, observationErr
	}
	projection := *child
	projection.Output = result.Content
	return &projection, nil
}
