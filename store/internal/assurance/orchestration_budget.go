package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

// KindOrchestrationBudget is the immutable anchor for team budget transactions.
const KindOrchestrationBudget = "orchestration_budget"
const kindOrchestrationBudgetClosed = "orchestration_budget_closed"

// OrchestrationBudgetDriver serializes all account mutations against its anchor.
// Callbacks may be replayed and must contain only transactional storage work.
type OrchestrationBudgetDriver interface {
	TransactOrchestrationBudget(context.Context, id.OrchestrationID, func(context.Context, Driver) error) error
}

var _ orchestration.BudgetStore = (*Repository)(nil)

func (s *Repository) orchestrationBudgetTransaction(ctx context.Context, key id.OrchestrationID, callback func(context.Context, *Repository) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := cortex.RequireScope(ctx); err != nil {
		return err
	}
	if key.IsNil() || key.Prefix() != id.PrefixOrchestration {
		return execution.ErrInvalidReservation
	}
	driver, ok := s.driver.(OrchestrationBudgetDriver)
	if !ok {
		return cortex.ErrBudgetUnavailable
	}
	return driver.TransactOrchestrationBudget(ctx, key, func(tc context.Context, tx Driver) error {
		repo := &Repository{driver: tx, clock: s.clock}
		if _, err := repo.GetOrchestrationBudget(tc, key); err != nil {
			return err
		}
		if err := callback(tc, repo); err != nil {
			return err
		}
		return tc.Err()
	})
}

// CreateOrchestrationBudget retains immutable bounds without creating a plan.
func (s *Repository) CreateOrchestrationBudget(ctx context.Context, policy orchestration.BudgetPolicy) error {
	if err := require(ctx, policy.Scope); err != nil {
		return err
	}
	frozen, digest, err := policy.Freeze()
	if err != nil {
		return err
	}
	if _, ok := s.driver.(OrchestrationBudgetDriver); !ok {
		return cortex.ErrBudgetUnavailable
	}
	row, err := record(KindOrchestrationBudget, frozen.ExecutionID.String(), digest, frozen.ExecutionID.String(), frozen.Scope, frozen)
	if err != nil {
		return err
	}
	if err = s.insert(ctx, row); errors.Is(err, ErrExists) {
		return execution.ErrReservationConflict
	}
	return err
}

// GetOrchestrationBudget returns the immutable policy at the exact scope.
func (s *Repository) GetOrchestrationBudget(ctx context.Context, key id.OrchestrationID) (orchestration.BudgetPolicy, error) {
	if key.IsNil() || key.Prefix() != id.PrefixOrchestration {
		return orchestration.BudgetPolicy{}, execution.ErrInvalidReservation
	}
	row, err := s.get(ctx, Query{Kind: KindOrchestrationBudget, ID: key.String()})
	if errors.Is(err, ErrAbsent) {
		return orchestration.BudgetPolicy{}, execution.ErrReservationNotFound
	}
	if err != nil {
		return orchestration.BudgetPolicy{}, err
	}
	var policy orchestration.BudgetPolicy
	if row.Deleted || len(row.Body) > 128<<10 || json.Unmarshal([]byte(row.Body), &policy) != nil {
		return policy, execution.ErrInvalidReservation
	}
	_, digest, err := policy.Freeze()
	if err != nil || row.Kind != KindOrchestrationBudget || row.ID != key.String() || row.RunID != key.String() || row.Digest != digest || policy.ExecutionID != key || policy.Scope.Canonical() != row.ScopeCanon {
		return policy, execution.ErrInvalidReservation
	}
	return policy, nil
}

// ReserveOrchestrationChild atomically admits one bounded participant invocation.
// Identical retries return retained accounting, including after admission closes.
func (s *Repository) ReserveOrchestrationChild(ctx context.Context, request execution.ReservationRequest) (execution.Reservation, error) {
	if err := require(ctx, request.Scope); err != nil {
		return execution.Reservation{}, err
	}
	req, digest, err := request.Freeze()
	if err != nil {
		return execution.Reservation{}, err
	}
	var result execution.Reservation
	err = s.orchestrationBudgetTransaction(ctx, req.PlanID, func(tc context.Context, tx *Repository) error {
		policy, readErr := tx.GetOrchestrationBudget(tc, req.PlanID)
		if readErr != nil {
			return readErr
		}
		if bindErr := policy.ValidateReservation(req); bindErr != nil {
			return bindErr
		}
		cached, readErr := tx.GetReservation(tc, req.ID)
		if readErr == nil {
			if cached.RequestDigest != digest {
				return execution.ErrReservationConflict
			}
			result = cached
			return nil
		}
		if !errors.Is(readErr, execution.ErrReservationNotFound) {
			return readErr
		}
		closed, readErr := tx.orchestrationBudgetClosed(tc, policy)
		if readErr != nil {
			return readErr
		}
		if closed {
			return execution.ErrBudgetExceeded
		}
		reservations, readErr := tx.accountReservations(tc, req.PlanID)
		if readErr != nil {
			return readErr
		}
		for _, reservation := range reservations {
			if reservation.Request.TaskID == req.TaskID {
				return execution.ErrReservationConflict
			}
		}
		result, _, readErr = tx.admitReservationInTransaction(tc, policy.AccountingPolicy(), reservations, req, digest)
		return readErr
	})
	if errors.Is(err, ErrExists) {
		err = execution.ErrReservationConflict
	}
	if err != nil {
		return execution.Reservation{}, err
	}
	return result, nil
}

// SettleOrchestrationChild retains final host usage after expiry or closure too.
func (s *Repository) SettleOrchestrationChild(ctx context.Context, settlement execution.Settlement) error {
	req, digest, err := settlement.Freeze()
	if err != nil {
		return err
	}
	initial, err := s.GetReservation(ctx, req.ReservationID)
	if err != nil {
		return err
	}
	err = s.orchestrationBudgetTransaction(ctx, initial.Request.PlanID, func(tc context.Context, tx *Repository) error {
		_, settleErr := tx.settleAccountInTransaction(tc, req, digest, func(pc context.Context, key id.ID) (execution.ReservationPolicy, error) {
			policy, readErr := tx.GetOrchestrationBudget(pc, key)
			return policy.AccountingPolicy(), readErr
		})
		return settleErr
	})
	if errors.Is(err, ErrExists) {
		return execution.ErrReservationConflict
	}
	return err
}

type orchestrationBudgetClosure struct {
	ExecutionID    id.OrchestrationID `json:"execution_id"`
	PreparedDigest string             `json:"prepared_digest"`
	At             time.Time          `json:"at"`
}

func (s *Repository) orchestrationBudgetClosed(ctx context.Context, policy orchestration.BudgetPolicy) (bool, error) {
	row, err := s.get(ctx, Query{Kind: kindOrchestrationBudgetClosed, ID: policy.ExecutionID.String()})
	if errors.Is(err, ErrAbsent) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var closure orchestrationBudgetClosure
	if row.Deleted || len(row.Body) > 16<<10 || json.Unmarshal([]byte(row.Body), &closure) != nil || closure.ExecutionID != policy.ExecutionID || closure.PreparedDigest != policy.PreparedDigest || closure.At.IsZero() || row.Kind != kindOrchestrationBudgetClosed || row.ID != policy.ExecutionID.String() || row.RunID != policy.ExecutionID.String() || row.Digest != evidence.Digest([]byte(row.Body)) {
		return false, execution.ErrInvalidReservation
	}
	return true, nil
}

// CloseOrchestrationBudget permanently seals new admission without refunding
// unsettled reservations. Recreating an identical policy does not reopen it.
func (s *Repository) CloseOrchestrationBudget(ctx context.Context, key id.OrchestrationID) error {
	return s.orchestrationBudgetTransaction(ctx, key, func(tc context.Context, tx *Repository) error {
		policy, err := tx.GetOrchestrationBudget(tc, key)
		if err != nil {
			return err
		}
		closed, err := tx.orchestrationBudgetClosed(tc, policy)
		if err != nil || closed {
			return err
		}
		closure := orchestrationBudgetClosure{ExecutionID: key, PreparedDigest: policy.PreparedDigest, At: s.clock.Now()}
		if closure.At.IsZero() {
			return execution.ErrInvalidReservation
		}
		data, err := json.Marshal(closure)
		if err != nil {
			return err
		}
		row, err := record(kindOrchestrationBudgetClosed, key.String(), evidence.Digest(data), key.String(), policy.Scope, closure)
		if err != nil {
			return err
		}
		return tx.driver.Insert(tc, row)
	})
}

// GetOrchestrationBudgetSnapshot serializes charges with admission and settlement.
func (s *Repository) GetOrchestrationBudgetSnapshot(ctx context.Context, key id.OrchestrationID) (orchestration.BudgetSnapshot, error) {
	var result orchestration.BudgetSnapshot
	err := s.orchestrationBudgetTransaction(ctx, key, func(tc context.Context, tx *Repository) error {
		policy, readErr := tx.GetOrchestrationBudget(tc, key)
		if readErr != nil {
			return readErr
		}
		closed, readErr := tx.orchestrationBudgetClosed(tc, policy)
		if readErr != nil {
			return readErr
		}
		reservations, readErr := tx.accountReservations(tc, key)
		if readErr != nil {
			return readErr
		}
		snapshot, accountErr := execution.AccountReservations(policy.AccountingPolicy(), reservations)
		if accountErr != nil {
			return accountErr
		}
		result = orchestration.BudgetSnapshot{Policy: policy, Charged: snapshot.Charged, Overflow: snapshot.Overflow, Reservations: snapshot.Reservations, Closed: closed}
		return nil
	})
	if err != nil {
		return orchestration.BudgetSnapshot{}, err
	}
	return result, nil
}

// ListOrchestrationReservations provides bounded recovery discovery for one team.
func (s *Repository) ListOrchestrationReservations(ctx context.Context, req execution.ReservationListRequest) ([]execution.Reservation, error) {
	if req.PlanID.IsNil() || req.PlanID.Prefix() != id.PrefixOrchestration || req.Limit < 1 || req.Limit > 256 || (!req.After.IsNil() && req.After.Prefix() != execution.PrefixReservation) {
		return nil, execution.ErrInvalidReservation
	}
	if _, err := s.GetOrchestrationBudget(ctx, req.PlanID); err != nil {
		return nil, err
	}
	return s.listReservations(ctx, req)
}
