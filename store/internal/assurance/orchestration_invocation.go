package assurance

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

const kindOrchestrationInvocation = "orchestration_invocation"
const kindOrchestrationInvocationBinding = "orchestration_invocation_binding"

var _ orchestration.InvocationStore = (*Repository)(nil)

type orchestrationInvocationBinding struct {
	IntentDigest string        `json:"intent_digest"`
	RunID        id.AgentRunID `json:"run_id"`
}

func orchestrationInvocationKey(team id.OrchestrationID, slot string) string {
	return evidence.Digest([]byte(team.String() + "/" + slot))
}

// BeginOrchestrationInvocation records single-use intent before reservation.
// Duplicate writes, including identical retries, never return fresh admission.
func (s *Repository) BeginOrchestrationInvocation(ctx context.Context, intent orchestration.InvocationIntent) error {
	if _, err := s.GetOrchestrationBudget(ctx, intent.OrchestrationID); err == nil {
		return s.orchestrationBudgetTransaction(ctx, intent.OrchestrationID, func(tc context.Context, tx *Repository) error {
			if _, ownerErr := tx.GetOrchestrationCoordinator(tc, intent.OrchestrationID); ownerErr == nil {
				return cortex.ErrOrchestrationRunConflict
			} else if !errors.Is(ownerErr, cortex.ErrOrchestrationCoordinatorNotFound) {
				return ownerErr
			}
			return tx.beginOrchestrationInvocation(tc, intent)
		})
	} else if !errors.Is(err, execution.ErrReservationNotFound) {
		return err
	}
	return s.beginOrchestrationInvocation(ctx, intent)
}
func (s *Repository) beginOrchestrationInvocation(ctx context.Context, intent orchestration.InvocationIntent) error {
	if err := require(ctx, intent.Scope); err != nil {
		return err
	}
	frozen, digest, err := intent.Freeze()
	if err != nil {
		return err
	}
	row, err := record(kindOrchestrationInvocation, orchestrationInvocationKey(frozen.OrchestrationID, frozen.Slot), digest, frozen.OrchestrationID.String(), frozen.Scope, frozen)
	if err != nil {
		return err
	}
	return singleInvocationInsert(ctx, s.driver, row)
}

func singleInvocationInsert(ctx context.Context, driver Driver, row Record) error {
	// Repository.insert is intentionally idempotent. Admission must instead expose
	// duplicate writes, so only the unique insert winner can reach reservation.
	err := driver.Insert(ctx, row)
	if errors.Is(err, ErrExists) {
		return cortex.ErrAlreadyExists
	}
	return err
}

// BindOrchestrationInvocation retains the original reserved child once.
func (s *Repository) BindOrchestrationInvocation(ctx context.Context, intent orchestration.InvocationIntent, child id.AgentRunID) error {
	if err := require(ctx, intent.Scope); err != nil {
		return err
	}
	frozen, digest, err := intent.Freeze()
	if err != nil {
		return err
	}
	if child.IsNil() || child.Prefix() != id.PrefixAgentRun {
		return cortex.ErrInvalidState
	}
	retained, err := s.GetOrchestrationInvocation(ctx, frozen.OrchestrationID, frozen.Slot)
	if err != nil {
		return err
	}
	_, retainedDigest, err := retained.Intent.Freeze()
	if err != nil || retainedDigest != digest {
		return cortex.ErrInvalidState
	}
	if !retained.RunID.IsNil() {
		return cortex.ErrAlreadyExists
	}
	binding := orchestrationInvocationBinding{IntentDigest: digest, RunID: child}
	data, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	row, err := record(kindOrchestrationInvocationBinding, orchestrationInvocationKey(frozen.OrchestrationID, frozen.Slot), evidence.Digest(data), frozen.OrchestrationID.String(), frozen.Scope, binding)
	if err != nil {
		return err
	}
	return singleInvocationInsert(ctx, s.driver, row)
}

// GetOrchestrationInvocation reads retained identity for reconciliation only.
func (s *Repository) GetOrchestrationInvocation(ctx context.Context, team id.OrchestrationID, slot string) (orchestration.InvocationRecord, error) {
	var out orchestration.InvocationRecord
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return out, err
	}
	// Validate the public lookup with the same bounded identity rules as writes.
	_, _, err = (orchestration.InvocationIntent{SchemaVersion: 1, OrchestrationID: team, Scope: scope, Slot: slot, AgentName: "lookup", RequestDigest: evidence.Digest(nil)}).Freeze()
	if err != nil {
		return out, err
	}
	key := orchestrationInvocationKey(team, slot)
	row, err := s.get(ctx, Query{Kind: kindOrchestrationInvocation, ID: key})
	if errors.Is(err, ErrAbsent) {
		return out, cortex.ErrOrchestrationInvocationNotFound
	}
	if err != nil {
		return out, err
	}
	if row.Deleted || len(row.Body) > 16<<10 || json.Unmarshal([]byte(row.Body), &out.Intent) != nil {
		return out, cortex.ErrInvalidState
	}
	_, digest, err := out.Intent.Freeze()
	if err != nil || row.Kind != kindOrchestrationInvocation || row.ID != key || row.Digest != digest || row.RunID != team.String() || out.Intent.OrchestrationID != team || out.Intent.Slot != slot || out.Intent.Scope.Canonical() != scope.Canonical() {
		return orchestration.InvocationRecord{}, cortex.ErrInvalidState
	}
	bound, err := s.get(ctx, Query{Kind: kindOrchestrationInvocationBinding, ID: key})
	if errors.Is(err, ErrAbsent) {
		return out, nil
	}
	if err != nil {
		return orchestration.InvocationRecord{}, err
	}
	var binding orchestrationInvocationBinding
	if bound.Deleted || len(bound.Body) > 1024 || json.Unmarshal([]byte(bound.Body), &binding) != nil || bound.Kind != kindOrchestrationInvocationBinding || bound.ID != key || bound.RunID != team.String() || bound.Digest != evidence.Digest([]byte(bound.Body)) || binding.IntentDigest != digest || binding.RunID.IsNil() || binding.RunID.Prefix() != id.PrefixAgentRun {
		return orchestration.InvocationRecord{}, cortex.ErrInvalidState
	}
	out.RunID = binding.RunID
	return out, nil
}
