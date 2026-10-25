package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/store/internal/orchestrationstate"
)

const kindOrchestrationCoordinator = "orchestration_coordinator"

type coordinatorParentDriver interface {
	CoordinatorParentDriver(context.Context) (orchestrationstate.Driver, error)
}

func coordinatorKey(team id.OrchestrationID, revision int64) string {
	return fmt.Sprintf("%s:%020d", team, revision)
}
func decodeCoordinator(row Record) (orchestration.CoordinatorLease, error) {
	var lease orchestration.CoordinatorLease
	if row.Deleted || len(row.Body) > 16<<10 || json.Unmarshal([]byte(row.Body), &lease) != nil {
		return lease, cortex.ErrInvalidContinuation
	}
	_, digest, err := lease.Freeze()
	if err != nil || row.Kind != kindOrchestrationCoordinator || row.ID != coordinatorKey(lease.TeamID, lease.Revision) || row.RunID != lease.TeamID.String() || row.ScopeCanon != lease.Scope.Canonical() || row.Digest != digest {
		return orchestration.CoordinatorLease{}, cortex.ErrInvalidContinuation
	}
	return lease, nil
}
func coordinatorSuccessor(previous, next orchestration.CoordinatorLease) bool {
	_, digest, err := previous.Freeze()
	if err != nil || next.PreviousDigest != digest || previous.Revision == math.MaxInt64 || next.Revision != previous.Revision+1 || next.TeamID != previous.TeamID || next.Scope.Canonical() != previous.Scope.Canonical() {
		return false
	}
	if next.Generation == previous.Generation {
		return !previous.Released && next.Owner == previous.Owner && next.AcquiredAt.Equal(previous.AcquiredAt) && !next.ExpiresAt.Before(previous.ExpiresAt) && (!next.Released || next.ExpiresAt.Equal(previous.ExpiresAt))
	}
	return previous.Generation < math.MaxInt64 && next.Generation == previous.Generation+1 && next.Owner != previous.Owner && !next.Released && !next.AcquiredAt.Before(previous.AcquiredAt) && (previous.Released || !next.AcquiredAt.Before(previous.ExpiresAt))
}

// GetOrchestrationCoordinator returns retained ownership evidence, not a claim.
func (s *Repository) GetOrchestrationCoordinator(ctx context.Context, team id.OrchestrationID) (orchestration.CoordinatorLease, error) {
	var empty orchestration.CoordinatorLease
	if _, err := cortex.RequireScope(ctx); err != nil {
		return empty, err
	}
	if team.IsNil() || team.Prefix() != id.PrefixOrchestration {
		return empty, cortex.ErrInvalidContinuation
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindOrchestrationCoordinator, RunID: team.String(), Reverse: true, Limit: 1})
	if err != nil {
		return empty, err
	}
	if len(rows) == 0 {
		return empty, cortex.ErrOrchestrationCoordinatorNotFound
	}
	if writeErr := validateRecord(ctx, rows[0]); writeErr != nil {
		return empty, writeErr
	}
	current, err := decodeCoordinator(rows[0])
	if err != nil || current.TeamID != team {
		return empty, errors.Join(err, cortex.ErrInvalidContinuation)
	}
	if current.Revision > 1 {
		row, readErr := s.get(ctx, Query{Kind: kindOrchestrationCoordinator, ID: coordinatorKey(team, current.Revision-1)})
		if readErr != nil {
			return empty, readErr
		}
		previous, decodeErr := decodeCoordinator(row)
		if decodeErr != nil || !coordinatorSuccessor(previous, current) {
			return empty, cortex.ErrInvalidContinuation
		}
	}
	return current, nil
}
func (s *Repository) appendCoordinator(ctx context.Context, lease orchestration.CoordinatorLease) error {
	frozen, digest, err := lease.Freeze()
	if err != nil {
		return err
	}
	row, err := record(kindOrchestrationCoordinator, coordinatorKey(lease.TeamID, lease.Revision), digest, lease.TeamID.String(), lease.Scope, frozen)
	if err != nil {
		return err
	}
	if err = s.driver.Insert(ctx, row); errors.Is(err, ErrExists) {
		return cortex.ErrOrchestrationRunConflict
	}
	return err
}
func (s *Repository) coordinatorParent(ctx context.Context) (orchestrationstate.Driver, error) {
	backend, ok := s.driver.(coordinatorParentDriver)
	if !ok {
		return nil, cortex.ErrOrchestrationContinuationUnavailable
	}
	return backend.CoordinatorParentDriver(ctx)
}
func validCoordinatorDuration(d time.Duration) bool {
	return d >= 100*time.Millisecond && d <= time.Minute
}
func (s *Repository) coordinatorPolicy(ctx context.Context, team id.OrchestrationID) (orchestration.BudgetPolicy, error) {
	p, err := s.GetOrchestrationBudget(ctx, team)
	if err != nil {
		return p, err
	}
	closed, err := s.orchestrationBudgetClosed(ctx, p)
	if err != nil {
		return p, err
	}
	if closed || !s.clock.Now().Before(p.Deadline) {
		return p, cortex.ErrOrchestrationRunConflict
	}
	return p, nil
}

// AcquireOrchestrationCoordinator atomically advances ownership and the parent
// generation under the budget lock. Initial requires generation one and no prior
// owner; hosts use it only immediately after creating the parent. Recovery of a
// running parent requires retained expired ownership.
func (s *Repository) AcquireOrchestrationCoordinator(ctx context.Context, request orchestration.CoordinatorRequest) (orchestration.CoordinatorClaim, error) {
	var result orchestration.CoordinatorClaim
	if request.Owner.IsNil() || request.Owner.Prefix() != orchestration.PrefixCoordinator || request.ExpectedGeneration < 1 || request.ExpectedGeneration == math.MaxInt64 || !validCoordinatorDuration(request.Duration) {
		return result, cortex.ErrInvalidContinuation
	}
	err := s.orchestrationBudgetTransaction(ctx, request.TeamID, func(tc context.Context, tx *Repository) error {
		policy, err := tx.coordinatorPolicy(tc, request.TeamID)
		if err != nil {
			return err
		}
		driver, err := tx.coordinatorParent(tc)
		if err != nil {
			return err
		}
		before, err := driver.Read(tc, request.TeamID)
		if err != nil {
			return err
		}
		if before.Run == nil || before.Run.Scope.Canonical() != policy.Scope.Canonical() || before.Run.ContinuationGeneration != request.ExpectedGeneration || before.Revision < 0 || before.Revision == math.MaxInt64 {
			return cortex.ErrOrchestrationRunConflict
		}
		now := tx.clock.Now()
		if now.IsZero() {
			return cortex.ErrInvalidContinuation
		}
		previous, readErr := tx.GetOrchestrationCoordinator(tc, request.TeamID)
		absent := errors.Is(readErr, cortex.ErrOrchestrationCoordinatorNotFound)
		if readErr != nil && !absent {
			return readErr
		}
		if absent {
			if request.Initial {
				if before.Run.Status != orchestration.StatusRunning || request.ExpectedGeneration != 1 {
					return cortex.ErrOrchestrationRunConflict
				}
			} else if before.Run.Status != orchestration.StatusWaiting {
				return cortex.ErrOrchestrationRunConflict
			}
		} else {
			if now.Before(previous.AcquiredAt) || request.Initial || previous.Generation != request.ExpectedGeneration || previous.Revision == math.MaxInt64 || previous.Owner == request.Owner || !previous.Released && now.Before(previous.ExpiresAt) || before.Run.Status != orchestration.StatusRunning && before.Run.Status != orchestration.StatusWaiting {
				return cortex.ErrOrchestrationRunConflict
			}
		}
		lease := orchestration.CoordinatorLease{SchemaVersion: 1, TeamID: request.TeamID, Scope: policy.Scope, Owner: request.Owner, Generation: request.ExpectedGeneration + 1, Revision: 1, AcquiredAt: now, ExpiresAt: minTime(now.Add(request.Duration), policy.Deadline)}
		if !absent {
			lease.Revision = previous.Revision + 1
			_, lease.PreviousDigest, err = previous.Freeze()
			if err != nil {
				return err
			}
		}
		if writeErr := tx.appendCoordinator(tc, lease); writeErr != nil {
			return writeErr
		}
		parent := *before.Run
		parent.ContinuationGeneration = lease.Generation
		parent.Status = orchestration.StatusRunning
		parent.CompletedAt = nil
		parent.Error = ""
		parent.Output = ""
		parent.UpdatedAt = now
		written, writeErr := driver.CompareAndSwap(tc, before, &parent)
		if writeErr != nil {
			return writeErr
		}
		if !written {
			return cortex.ErrOrchestrationRunConflict
		}
		result = orchestration.CoordinatorClaim{Lease: lease, Run: &parent}
		return nil
	})
	if err != nil {
		return orchestration.CoordinatorClaim{}, err
	}
	return result, nil
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (s *Repository) checkCoordinator(ctx context.Context, lease orchestration.CoordinatorLease) (orchestration.CoordinatorLease, orchestrationstate.Driver, error) {
	var empty orchestration.CoordinatorLease
	if err := require(ctx, lease.Scope); err != nil {
		return empty, nil, err
	}
	if _, _, err := lease.Freeze(); err != nil {
		return empty, nil, err
	}
	if _, err := s.coordinatorPolicy(ctx, lease.TeamID); err != nil {
		return empty, nil, err
	}
	current, err := s.GetOrchestrationCoordinator(ctx, lease.TeamID)
	if err != nil {
		return empty, nil, err
	}
	now := s.clock.Now()
	if now.IsZero() || now.Before(current.AcquiredAt) || lease.Released || current.Released || current.Owner != lease.Owner || current.Generation != lease.Generation || current.Revision < lease.Revision || !current.AcquiredAt.Equal(lease.AcquiredAt) || current.ExpiresAt.Before(lease.ExpiresAt) || !now.Before(lease.ExpiresAt) || !now.Before(current.ExpiresAt) {
		return empty, nil, cortex.ErrOrchestrationRunConflict
	}
	driver, err := s.coordinatorParent(ctx)
	if err != nil {
		return empty, nil, err
	}
	parent, err := driver.Read(ctx, lease.TeamID)
	if err != nil {
		return empty, nil, err
	}
	if parent.Run == nil || parent.Run.Scope.Canonical() != lease.Scope.Canonical() || parent.Run.Status != orchestration.StatusRunning || parent.Run.ContinuationGeneration != lease.Generation {
		return empty, nil, cortex.ErrOrchestrationRunConflict
	}
	return current, driver, nil
}

// CheckOrchestrationCoordinator verifies an acknowledged lease against current
// account admission, ownership and the parent generation in one transaction.
func (s *Repository) CheckOrchestrationCoordinator(ctx context.Context, lease orchestration.CoordinatorLease) (orchestration.CoordinatorLease, error) {
	var result orchestration.CoordinatorLease
	err := s.orchestrationBudgetTransaction(ctx, lease.TeamID, func(tc context.Context, tx *Repository) error {
		var checkErr error
		result, _, checkErr = tx.checkCoordinator(tc, lease)
		return checkErr
	})
	if err != nil {
		return orchestration.CoordinatorLease{}, err
	}
	return result, nil
}

// RenewOrchestrationCoordinator retains a new immutable revision without
// extending the team's immutable deadline or reviving an expired acknowledgment.
func (s *Repository) RenewOrchestrationCoordinator(ctx context.Context, lease orchestration.CoordinatorLease, duration time.Duration) (orchestration.CoordinatorLease, error) {
	var result orchestration.CoordinatorLease
	if !validCoordinatorDuration(duration) {
		return result, cortex.ErrInvalidContinuation
	}
	err := s.orchestrationBudgetTransaction(ctx, lease.TeamID, func(tc context.Context, tx *Repository) error {
		current, _, err := tx.checkCoordinator(tc, lease)
		if err != nil {
			return err
		}
		if current.Revision == math.MaxInt64 {
			return cortex.ErrOrchestrationRunConflict
		}
		policy, err := tx.GetOrchestrationBudget(tc, lease.TeamID)
		if err != nil {
			return err
		}
		next := current
		next.Revision++
		_, next.PreviousDigest, err = current.Freeze()
		if err != nil {
			return err
		}
		renewed := minTime(tx.clock.Now().Add(duration), policy.Deadline)
		if renewed.After(next.ExpiresAt) {
			next.ExpiresAt = renewed
		}
		if writeErr := tx.appendCoordinator(tc, next); writeErr != nil {
			return writeErr
		}
		result = next
		return nil
	})
	if err != nil {
		return orchestration.CoordinatorLease{}, err
	}
	return result, nil
}

// FinishOrchestrationCoordinator atomically releases a waiting pass or saves a
// terminal parent and closes its account. A stale owner can do neither.
func (s *Repository) FinishOrchestrationCoordinator(ctx context.Context, lease orchestration.CoordinatorLease, parent *orchestration.Run) error {
	if parent == nil || parent.ID != lease.TeamID || parent.Scope.Canonical() != lease.Scope.Canonical() || parent.ContinuationGeneration != lease.Generation {
		return cortex.ErrInvalidContinuation
	}
	if parent.Status != orchestration.StatusWaiting && parent.Status != orchestration.StatusCompleted && parent.Status != orchestration.StatusFailed {
		return cortex.ErrInvalidContinuation
	}
	detached := *parent
	err := s.orchestrationBudgetTransaction(ctx, lease.TeamID, func(tc context.Context, tx *Repository) error {
		current, driver, err := tx.checkCoordinator(tc, lease)
		if err != nil {
			return err
		}
		if current.Revision == math.MaxInt64 {
			return cortex.ErrOrchestrationRunConflict
		}
		observed, err := driver.Read(tc, lease.TeamID)
		if err != nil {
			return err
		}
		if observed.Run.ConfigID != parent.ConfigID || observed.Run.Strategy != parent.Strategy || observed.Run.Input != parent.Input {
			return cortex.ErrInvalidContinuation
		}
		now := tx.clock.Now()
		detached.CompletedAt = &now
		if detached.Status == orchestration.StatusWaiting {
			detached.CompletedAt = nil
			detached.Output = ""
			detached.Error = ""
		}
		if writeErr := orchestrationstate.Update(tc, driver, &detached); writeErr != nil {
			return writeErr
		}
		released := current
		released.Revision++
		released.Released = true
		_, released.PreviousDigest, err = current.Freeze()
		if err != nil {
			return err
		}
		if writeErr := tx.appendCoordinator(tc, released); writeErr != nil {
			return writeErr
		}
		if parent.Status != orchestration.StatusWaiting {
			policy, readErr := tx.GetOrchestrationBudget(tc, lease.TeamID)
			if readErr != nil {
				return readErr
			}
			return tx.closeOrchestrationBudget(tc, policy)
		}
		return nil
	})
	if err == nil {
		*parent = detached
	}
	return err
}

// BeginCoordinatedInvocation fences a single-use strategy slot with ownership.
func (s *Repository) BeginCoordinatedInvocation(ctx context.Context, lease orchestration.CoordinatorLease, intent orchestration.InvocationIntent) error {
	if intent.OrchestrationID != lease.TeamID || intent.Scope.Canonical() != lease.Scope.Canonical() {
		return cortex.ErrInvalidContinuation
	}
	return s.orchestrationBudgetTransaction(ctx, lease.TeamID, func(tc context.Context, tx *Repository) error {
		if _, _, err := tx.checkCoordinator(tc, lease); err != nil {
			return err
		}
		return tx.beginOrchestrationInvocation(tc, intent)
	})
}

// ReserveCoordinatedChild atomically charges and links one child while ownership
// is current. A retained reservation is evidence, never another dispatch grant.
func (s *Repository) ReserveCoordinatedChild(ctx context.Context, lease orchestration.CoordinatorLease, request execution.ReservationRequest) (execution.Reservation, error) {
	var result execution.Reservation
	if request.PlanID != lease.TeamID || request.Scope.Canonical() != lease.Scope.Canonical() {
		return result, cortex.ErrInvalidContinuation
	}
	req, digest, err := request.Freeze()
	if err != nil {
		return result, err
	}
	err = s.orchestrationBudgetTransaction(ctx, lease.TeamID, func(tc context.Context, tx *Repository) error {
		_, driver, checkErr := tx.checkCoordinator(tc, lease)
		if checkErr != nil {
			return checkErr
		}
		_, readErr := tx.GetReservation(tc, req.ID)
		if readErr == nil {
			return cortex.ErrAlreadyExists
		}
		if !errors.Is(readErr, execution.ErrReservationNotFound) {
			return readErr
		}
		result, readErr = tx.reserveOrchestrationChild(tc, req, digest)
		if readErr != nil {
			return readErr
		}
		return orchestrationstate.Link(tc, driver, lease.TeamID, req.TaskID)
	})
	if err != nil {
		return execution.Reservation{}, err
	}
	return result, nil
}

// UnownedOrchestrationParent serializes legacy parent mutations with possible
// coordinator acquisition. Closed accounts permit only the caller's explicitly
// selected cleanup path. A missing budget leaves ordinary orchestration intact.
func (s *Repository) UnownedOrchestrationParent(ctx context.Context, team id.OrchestrationID, allowClosed bool, change func(orchestrationstate.Driver) error) (bool, error) {
	if _, err := s.GetOrchestrationBudget(ctx, team); errors.Is(err, execution.ErrReservationNotFound) {
		return false, nil
	} else if err != nil {
		return true, err
	}
	err := s.orchestrationBudgetTransaction(ctx, team, func(tc context.Context, tx *Repository) error {
		_, ownerErr := tx.GetOrchestrationCoordinator(tc, team)
		if ownerErr != nil && !errors.Is(ownerErr, cortex.ErrOrchestrationCoordinatorNotFound) {
			return ownerErr
		}
		if ownerErr == nil {
			if !allowClosed {
				return cortex.ErrOrchestrationRunConflict
			}
			policy, readErr := tx.GetOrchestrationBudget(tc, team)
			if readErr != nil {
				return readErr
			}
			closed, readErr := tx.orchestrationBudgetClosed(tc, policy)
			if readErr != nil {
				return readErr
			}
			if !closed {
				return cortex.ErrOrchestrationRunConflict
			}
		}
		driver, readErr := tx.coordinatorParent(tc)
		if readErr != nil {
			return readErr
		}
		return change(&coordinatorContextDriver{Driver: driver, ctx: tc})
	})
	return true, err
}

// Mongo transactions carry their session in tc. Keep it when a backend's legacy
// callback uses its original request context. The enclosing transaction owns it.
type coordinatorContextDriver struct {
	orchestrationstate.Driver
	ctx context.Context
}

func (d *coordinatorContextDriver) Read(_ context.Context, key id.OrchestrationID) (orchestrationstate.Snapshot, error) {
	return d.Driver.Read(d.ctx, key)
}
func (d *coordinatorContextDriver) CompareAndSwap(_ context.Context, before orchestrationstate.Snapshot, after *orchestration.Run) (bool, error) {
	return d.Driver.CompareAndSwap(d.ctx, before, after)
}
