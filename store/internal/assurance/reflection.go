package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

const kindReflection = "cognitive_reflection"

var _ cognitive.ReflectionStore = (*Repository)(nil)

func reflectionID(runID id.AgentRunID, version int64) string {
	return fmt.Sprintf("%s:%016d", runID.String(), version)
}
func decodeReflection(ctx context.Context, row Record) (cognitive.ReflectionReceipt, error) {
	var result cognitive.ReflectionReceipt
	if validateRecord(ctx, row) != nil || row.Deleted || len(row.Body) > 1<<20 || row.Digest != evidence.Digest([]byte(row.Body)) || json.Unmarshal([]byte(row.Body), &result) != nil || result.Validate() != nil || row.RunID != result.Request.Snapshot.Binding.RunID.String() || row.ID != reflectionID(result.Request.Snapshot.Binding.RunID, result.State.Version) {
		return cognitive.ReflectionReceipt{}, cognitive.ErrInvalidReflection
	}
	return result, nil
}

// GetReflection returns the latest immutable transition, never dispatch permission.
func (s *Repository) GetReflection(ctx context.Context, runID id.AgentRunID) (*cognitive.ReflectionReceipt, error) {
	if _, err := cortex.RequireScope(ctx); err != nil {
		return nil, err
	}
	if runID.IsNil() || runID.Prefix() != id.PrefixAgentRun {
		return nil, cognitive.ErrInvalidReflection
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindReflection, RunID: runID.String(), Reverse: true, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	receipt, err := decodeReflection(ctx, rows[0])
	if err != nil {
		return nil, err
	}
	return &receipt, nil
}

// AdmitReflection serializes observations with run changes. All referenced bytes
// must remain available; host guards additionally authenticate their meaning.
func (s *Repository) AdmitReflection(ctx context.Context, request cognitive.ReflectionRequest) (cognitive.ReflectionReceipt, error) {
	frozen, digest, err := request.Freeze()
	if err != nil {
		return cognitive.ReflectionReceipt{}, err
	}
	if scopeErr := require(ctx, frozen.Snapshot.Binding.Scope); scopeErr != nil {
		return cognitive.ReflectionReceipt{}, scopeErr
	}
	driver, ok := s.driver.(RunDriver)
	if !ok {
		return cognitive.ReflectionReceipt{}, cognitive.ErrInvalidReflection
	}
	var receipt cognitive.ReflectionReceipt
	err = driver.TransactRun(ctx, frozen.Snapshot.Binding.RunID, func(tc context.Context, tx RunTransaction) error {
		receipt = cognitive.ReflectionReceipt{}
		repository := s.inTransaction(tx)
		row, readErr := repository.get(tc, Query{Kind: kindReflection, ID: reflectionID(frozen.Snapshot.Binding.RunID, frozen.ExpectedVersion+1)})
		if readErr == nil {
			retained, decodeErr := decodeReflection(tc, row)
			if decodeErr != nil {
				return decodeErr
			}
			if retained.RequestDigest != digest {
				return cognitive.ErrReflectionConflict
			}
			receipt = retained
			return nil
		}
		if !errors.Is(readErr, ErrAbsent) {
			return readErr
		}
		current, readErr := tx.GetRun(tc, frozen.Snapshot.Binding.RunID)
		if readErr != nil {
			return readErr
		}
		if validateReflectionRun(current, frozen) != nil {
			return cognitive.ErrReflectionConflict
		}
		previous, readErr := repository.GetReflection(tc, current.ID)
		if readErr != nil {
			return readErr
		}
		var before cognitive.ReflectionState
		if previous != nil {
			if !reflect.DeepEqual(previous.Request.Policy, frozen.Policy) || !reflect.DeepEqual(previous.Request.Snapshot.Binding, frozen.Snapshot.Binding) {
				return cognitive.ErrReflectionConflict
			}
			before = previous.State
		}
		now := s.clock.Now().UTC()
		if now.Before(frozen.Snapshot.ObservedAt) || now.Sub(frozen.Snapshot.ObservedAt) > 30*time.Second {
			return cognitive.ErrReflectionConflict
		}
		for _, ref := range frozen.Snapshot.References() {
			if !now.Before(ref.RetainUntil) {
				return cognitive.ErrReflectionConflict
			}
			if _, loadErr := repository.LoadEvidence(tc, ref); loadErr != nil {
				return loadErr
			}
		}
		next, advanceErr := cognitive.AdvanceReflection(frozen, before, now)
		if advanceErr != nil {
			return advanceErr
		}
		saved, recordErr := record(kindReflection, reflectionID(current.ID, next.State.Version), "", current.ID.String(), current.Scope, next)
		if recordErr != nil {
			return recordErr
		}
		saved.Digest = evidence.Digest([]byte(saved.Body))
		if insertErr := tx.Insert(tc, saved); insertErr != nil {
			return insertErr
		}
		next.Fresh = true
		receipt = next
		return tc.Err()
	})
	if err != nil {
		return cognitive.ReflectionReceipt{}, err
	}
	return receipt, nil
}
func validateReflectionRun(r *run.Run, req cognitive.ReflectionRequest) error {
	b := req.Snapshot.Binding
	if r == nil || r.ID != b.RunID || r.AgentID != b.AgentID || r.Scope.Canonical() != b.Scope.Canonical() || r.Revision != req.ExpectedRunRevision || r.Provenance.PreparedDigest != b.PreparedDigest || r.Provenance.VerificationRequired != b.VerificationRequired || !reflect.DeepEqual(r.Budget, b.Budget) || r.Provenance.Purpose != run.PurposeNormal || req.Snapshot.Step < r.StepCount || req.Snapshot.Usage.Steps < r.Usage.Steps || req.Snapshot.Usage.Tokens < r.Usage.Tokens || req.Snapshot.Usage.ToolCalls < r.Usage.ToolCalls {
		return cognitive.ErrReflectionConflict
	}
	if req.Intervention == nil && r.State != run.StateRunning {
		return cognitive.ErrReflectionConflict
	}
	if req.Intervention != nil && r.State != run.StateRunning && r.State != run.StatePaused && r.State != run.StateIncomplete {
		return cognitive.ErrReflectionConflict
	}
	return nil
}
