package assurance

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

const kindOrchestrationContinuation = "orchestration_continuation"
const kindOrchestrationResult = "orchestration_invocation_result"

var _ orchestration.ContinuationStore = (*Repository)(nil)

// PutOrchestrationContinuation retains the original private execution snapshot.
func (s *Repository) PutOrchestrationContinuation(ctx context.Context, c orchestration.Continuation) error {
	if err := require(ctx, c.Scope); err != nil {
		return err
	}
	frozen, digest, err := c.Freeze()
	if err != nil {
		return err
	}
	row, err := record(kindOrchestrationContinuation, c.OrchestrationID.String(), digest, c.OrchestrationID.String(), c.Scope, frozen)
	if err != nil {
		return err
	}
	if err = s.insert(ctx, row); errors.Is(err, ErrExists) {
		return cortex.ErrInvalidContinuation
	}
	return err
}

// GetOrchestrationContinuation reads the frozen strategy at the exact scope.
func (s *Repository) GetOrchestrationContinuation(ctx context.Context, team id.OrchestrationID) (orchestration.Continuation, error) {
	var out orchestration.Continuation
	if team.IsNil() || team.Prefix() != id.PrefixOrchestration {
		return out, cortex.ErrInvalidContinuation
	}
	row, err := s.get(ctx, Query{Kind: kindOrchestrationContinuation, ID: team.String()})
	if errors.Is(err, ErrAbsent) {
		return out, cortex.ErrInvalidContinuation
	}
	if err != nil {
		return out, err
	}
	if row.Deleted || len(row.Body) > 16<<20 || json.Unmarshal([]byte(row.Body), &out) != nil {
		return out, cortex.ErrInvalidContinuation
	}
	_, digest, err := out.Freeze()
	if err != nil || row.ID != team.String() || row.RunID != team.String() || row.Kind != kindOrchestrationContinuation || row.Digest != digest || out.OrchestrationID != team || out.Scope.Canonical() != row.ScopeCanon {
		return orchestration.Continuation{}, cortex.ErrInvalidContinuation
	}
	return out, nil
}

type orchestrationResult struct {
	IntentDigest string                    `json:"intent_digest"`
	Result       orchestration.AgentResult `json:"result"`
}

// PutOrchestrationInvocationResult retains one completed result for strategy replay.
func (s *Repository) PutOrchestrationInvocationResult(ctx context.Context, intent orchestration.InvocationIntent, result orchestration.AgentResult) error {
	if err := require(ctx, intent.Scope); err != nil {
		return err
	}
	_, digest, err := intent.Freeze()
	if err != nil {
		return err
	}
	if validateErr := intent.ValidateResult(&result); validateErr != nil {
		return validateErr
	}
	retained, err := s.GetOrchestrationInvocation(ctx, intent.OrchestrationID, intent.Slot)
	if err != nil {
		return err
	}
	_, actual, err := retained.Intent.Freeze()
	if err != nil || actual != digest || retained.RunID != result.RunID {
		return cortex.ErrInvalidContinuation
	}
	value := orchestrationResult{IntentDigest: digest, Result: result}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	row, err := record(kindOrchestrationResult, orchestrationInvocationKey(intent.OrchestrationID, intent.Slot), evidence.Digest(data), intent.OrchestrationID.String(), intent.Scope, value)
	if err != nil {
		return err
	}
	if err = s.insert(ctx, row); errors.Is(err, ErrExists) {
		return cortex.ErrInvalidContinuation
	}
	return err
}

// GetOrchestrationInvocationResult returns nil until a completed result is saved.
func (s *Repository) GetOrchestrationInvocationResult(ctx context.Context, intent orchestration.InvocationIntent) (*orchestration.AgentResult, error) {
	if err := require(ctx, intent.Scope); err != nil {
		return nil, err
	}
	_, digest, err := intent.Freeze()
	if err != nil {
		return nil, err
	}
	retained, err := s.GetOrchestrationInvocation(ctx, intent.OrchestrationID, intent.Slot)
	if err != nil {
		return nil, err
	}
	_, actual, err := retained.Intent.Freeze()
	if err != nil || actual != digest {
		return nil, cortex.ErrInvalidContinuation
	}
	key := orchestrationInvocationKey(intent.OrchestrationID, intent.Slot)
	row, err := s.get(ctx, Query{Kind: kindOrchestrationResult, ID: key})
	if errors.Is(err, ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out orchestrationResult
	if row.Deleted || len(row.Body) > 32<<20 || json.Unmarshal([]byte(row.Body), &out) != nil || row.Kind != kindOrchestrationResult || row.ID != key || row.RunID != intent.OrchestrationID.String() || row.Digest != evidence.Digest([]byte(row.Body)) || out.IntentDigest != digest || out.Result.RunID != retained.RunID || intent.ValidateResult(&out.Result) != nil {
		return nil, cortex.ErrInvalidContinuation
	}
	return &out.Result, nil
}
