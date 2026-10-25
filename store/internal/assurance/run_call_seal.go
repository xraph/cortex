package assurance

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

// KindRunCallGuard is the shared row that serializes admission and closure.
const KindRunCallGuard = "run_call_guard"
const kindRunCallSeal = "run_call_seal"

// RunCallDriver fences one exact run's call admission and closure. Callbacks
// contain transactional storage work only; Mongo may repeat them after conflicts.
type RunCallDriver interface {
	TransactRunCalls(context.Context, id.AgentRunID, func(context.Context, Driver) error) error
}

// RunCallLocker adds a journal guard to an existing database transaction.
// The driver must reject calls outside a live transaction. Hold a plan guard
// before journal guards when changing plan state; never acquire a plan guard
// while holding only a journal guard. Callbacks contain storage operations only.
type RunCallLocker interface {
	LockRunCalls(context.Context, id.AgentRunID) error
}

var _ execution.RunCallSealStore = (*Repository)(nil)

func (s *Repository) runCallTransaction(ctx context.Context, runID id.AgentRunID, callback func(context.Context, *Repository) error) error {
	_, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	if runID.IsNil() || runID.Prefix() != id.PrefixAgentRun {
		return execution.ErrInvalidRunCall
	}
	driver, ok := s.driver.(RunCallDriver)
	if !ok {
		return execution.ErrRunCallUnavailable
	}
	if anchorErr := s.ensureRunCallGuard(ctx, runID); anchorErr != nil {
		return anchorErr
	}
	err = driver.TransactRunCalls(ctx, runID, func(tc context.Context, tx Driver) error {
		repo := &Repository{driver: tx, clock: s.clock}
		if guardErr := repo.requireRunCallGuard(tc, runID); guardErr != nil {
			return guardErr
		}
		if callbackErr := callback(tc, repo); callbackErr != nil {
			return callbackErr
		}
		return tc.Err()
	})
	if err != nil {
		return err
	}
	return ctx.Err()
}

// SealRunCalls permanently closes admission while retaining late observations.
// The caller must hold current host authority through this transaction.
func (s *Repository) SealRunCalls(ctx context.Context, request execution.RunCallSealRequest) (execution.RunCallSeal, error) {
	if err := require(ctx, request.Scope); err != nil {
		return execution.RunCallSeal{}, err
	}
	r, digest, err := request.Freeze()
	if err != nil {
		return execution.RunCallSeal{}, err
	}
	var result execution.RunCallSeal
	err = s.runCallTransaction(ctx, r.RunID, func(tc context.Context, tx *Repository) error {
		var sealErr error
		result, sealErr = tx.sealRunCallsInTransaction(tc, r, digest)
		return sealErr
	})
	if err != nil {
		return execution.RunCallSeal{}, err
	}
	return result, nil
}

// ensureRunCallGuard creates an anchor without granting execution permission.
func (s *Repository) ensureRunCallGuard(ctx context.Context, runID id.AgentRunID) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	anchor, err := record(KindRunCallGuard, runID.String(), evidence.Digest([]byte("{}")), runID.String(), scope, struct{}{})
	if err != nil {
		return err
	}
	return s.insert(ctx, anchor)
}

// sealRunCallsInTransaction requires the journal guard until outer commit.
func (s *Repository) sealRunCallsInTransaction(ctx context.Context, r execution.RunCallSealRequest, digest string) (execution.RunCallSeal, error) {
	existing, err := s.GetRunCallSeal(ctx, r.RunID)
	if err != nil {
		return execution.RunCallSeal{}, err
	}
	if existing != nil {
		if existing.RequestDigest != digest {
			return execution.RunCallSeal{}, execution.ErrRunCallConflict
		}
		return *existing, nil
	}
	value := execution.RunCallSeal{Request: r, RequestDigest: digest, At: s.clock.Now().UTC()}
	if value.Validate() != nil {
		return execution.RunCallSeal{}, execution.ErrInvalidRunCall
	}
	body, err := json.Marshal(value)
	if err != nil {
		return execution.RunCallSeal{}, err
	}
	row, err := record(kindRunCallSeal, r.RunID.String(), evidence.Digest(body), r.RunID.String(), r.Scope, value)
	if err != nil {
		return execution.RunCallSeal{}, err
	}
	if insertErr := s.driver.Insert(ctx, row); insertErr != nil {
		return execution.RunCallSeal{}, insertErr
	}
	return value, nil
}

// GetRunCallSeal returns a permanent barrier or nil when membership is still open.
func (s *Repository) GetRunCallSeal(ctx context.Context, runID id.AgentRunID) (*execution.RunCallSeal, error) {
	if runID.IsNil() || runID.Prefix() != id.PrefixAgentRun {
		return nil, execution.ErrInvalidRunCall
	}
	row, err := s.get(ctx, Query{Kind: kindRunCallSeal, ID: runID.String()})
	if errors.Is(err, ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sealed execution.RunCallSeal
	if row.Deleted || row.Kind != kindRunCallSeal || row.ID != runID.String() || row.RunID != runID.String() || len(row.Body) > 32<<10 || row.Digest != evidence.Digest([]byte(row.Body)) || json.Unmarshal([]byte(row.Body), &sealed) != nil || sealed.Validate() != nil || sealed.Request.RunID != runID || sealed.Request.Scope.Canonical() != row.ScopeCanon {
		return nil, execution.ErrInvalidRunCall
	}
	return &sealed, nil
}

func (s *Repository) requireRunCallGuard(ctx context.Context, runID id.AgentRunID) error {
	row, err := s.get(ctx, Query{Kind: KindRunCallGuard, ID: runID.String()})
	if err != nil {
		return err
	}
	if row.Deleted || row.Kind != KindRunCallGuard || row.ID != runID.String() || row.RunID != runID.String() || row.Body != "{}" || row.Digest != evidence.Digest([]byte("{}")) {
		return execution.ErrInvalidRunCall
	}
	return nil
}
