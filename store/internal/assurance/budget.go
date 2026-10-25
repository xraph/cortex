package assurance

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

const (
	kindReservationPolicy = "reservation_policy"
	kindReservation       = "budget_reservation"
	kindSettlement        = "budget_settlement"
)

var _ execution.BudgetStore = (*Repository)(nil)

// CreateReservationPolicy fixes a shared account without resetting on replans.
func (s *Repository) CreateReservationPolicy(ctx context.Context, policy execution.ReservationPolicy) error {
	if err := require(ctx, policy.Scope); err != nil {
		return err
	}
	frozen, digest, err := policy.Freeze()
	if err != nil {
		return err
	}
	if frozen.PlanID.Prefix() != planning.PrefixPlan {
		return execution.ErrInvalidReservation
	}
	return s.planTransaction(ctx, frozen.PlanID, func(tc context.Context, tx *Repository) error {
		previous, readErr := tx.GetReservationPolicy(tc, frozen.PlanID)
		if readErr == nil {
			_, previousDigest, freezeErr := previous.Freeze()
			if freezeErr != nil {
				return freezeErr
			}
			if previousDigest != digest {
				return execution.ErrReservationConflict
			}
			return nil
		}
		if !errors.Is(readErr, execution.ErrReservationNotFound) {
			return readErr
		}
		p, readErr := tx.GetPlan(tc, frozen.PlanID)
		if readErr != nil {
			return readErr
		}
		if frozen.Scope.Canonical() != p.Scope.Canonical() || frozen.Deadline.After(p.Budget.Deadline) || frozen.Limits.Steps > int64(p.Budget.MaxSteps) || frozen.Limits.Tokens > int64(p.Budget.MaxTokens) || frozen.Limits.ToolCalls > int64(p.Budget.MaxToolCalls) {
			return execution.ErrInvalidReservation
		}
		row, buildErr := record(kindReservationPolicy, frozen.PlanID.String(), digest, frozen.PlanID.String(), frozen.Scope, frozen)
		if buildErr != nil {
			return buildErr
		}
		return tx.driver.Insert(tc, row)
	})
}

// GetReservationPolicy reads the immutable policy, before current plan narrowing.
func (s *Repository) GetReservationPolicy(ctx context.Context, planID id.ID) (execution.ReservationPolicy, error) {
	if planID.IsNil() || planID.Prefix() != planning.PrefixPlan {
		return execution.ReservationPolicy{}, execution.ErrInvalidReservation
	}
	row, err := s.get(ctx, Query{Kind: kindReservationPolicy, ID: planID.String()})
	if errors.Is(err, ErrAbsent) {
		return execution.ReservationPolicy{}, execution.ErrReservationNotFound
	}
	if err != nil {
		return execution.ReservationPolicy{}, err
	}
	var p execution.ReservationPolicy
	if row.Deleted || len(row.Body) > 16<<10 || json.Unmarshal([]byte(row.Body), &p) != nil {
		return p, execution.ErrInvalidReservation
	}
	_, digest, err := p.Freeze()
	if err != nil || row.Kind != kindReservationPolicy || row.ID != planID.String() || row.RunID != planID.String() || row.Digest != digest || p.PlanID != planID || p.Scope.Canonical() != row.ScopeCanon {
		return p, execution.ErrInvalidReservation
	}
	return p, nil
}
func (s *Repository) effectiveReservationPolicy(ctx context.Context, planID id.ID) (execution.ReservationPolicy, planning.Plan, error) {
	policy, err := s.GetReservationPolicy(ctx, planID)
	if err != nil {
		return policy, planning.Plan{}, err
	}
	p, err := s.GetPlan(ctx, planID)
	if err != nil {
		return policy, p, err
	}
	policy.Limits.Steps = min(policy.Limits.Steps, int64(p.Budget.MaxSteps))
	policy.Limits.Tokens = min(policy.Limits.Tokens, int64(p.Budget.MaxTokens))
	policy.Limits.ToolCalls = min(policy.Limits.ToolCalls, int64(p.Budget.MaxToolCalls))
	if p.Budget.Deadline.Before(policy.Deadline) {
		policy.Deadline = p.Budget.Deadline
	}
	return policy, p, nil
}
func reservationRecord(r execution.Reservation) (Record, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return Record{}, err
	}
	return record(kindReservation, r.Request.ID.String(), evidence.Digest(data), r.Request.PlanID.String(), r.Request.Scope, r)
}

func decodeReservation(row Record) (execution.Reservation, error) {
	var r execution.Reservation
	if row.Deleted || len(row.Body) > 32<<10 || json.Unmarshal([]byte(row.Body), &r) != nil || r.Settlement != nil || r.Validate() != nil {
		return r, execution.ErrInvalidReservation
	}
	expected, err := reservationRecord(r)
	if err != nil || row.Kind != expected.Kind || row.ID != expected.ID || row.RunID != expected.RunID || row.ScopeCanon != expected.ScopeCanon || row.Digest != expected.Digest {
		return r, execution.ErrInvalidReservation
	}
	return r, nil
}
func decodeSettlement(row Record) (execution.SettlementReceipt, error) {
	var receipt execution.SettlementReceipt
	if row.Deleted || len(row.Body) > 16<<10 || json.Unmarshal([]byte(row.Body), &receipt) != nil || row.Kind != kindSettlement || row.ID != receipt.Request.ReservationID.String() || row.Digest != evidence.Digest([]byte(row.Body)) {
		return receipt, execution.ErrInvalidReservation
	}
	_, digest, err := receipt.Request.Freeze()
	if err != nil || digest != receipt.Digest || receipt.At.IsZero() {
		return receipt, execution.ErrInvalidReservation
	}
	return receipt, nil
}

// GetReservation returns allocation plus its optional final accounting receipt.
func (s *Repository) GetReservation(ctx context.Context, identifier id.ID) (execution.Reservation, error) {
	if identifier.IsNil() || identifier.Prefix() != execution.PrefixReservation {
		return execution.Reservation{}, execution.ErrInvalidReservation
	}
	row, err := s.get(ctx, Query{Kind: kindReservation, ID: identifier.String()})
	if errors.Is(err, ErrAbsent) {
		return execution.Reservation{}, execution.ErrReservationNotFound
	}
	if err != nil {
		return execution.Reservation{}, err
	}
	r, err := decodeReservation(row)
	if err != nil {
		return r, err
	}
	settled, err := s.get(ctx, Query{Kind: kindSettlement, ID: identifier.String()})
	if errors.Is(err, ErrAbsent) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	receipt, err := decodeSettlement(settled)
	if err != nil {
		return r, err
	}
	if settled.RunID != r.Request.PlanID.String() {
		return r, execution.ErrInvalidReservation
	}
	r.Settlement = &receipt
	return r, r.Validate()
}
func (s *Repository) accountReservations(ctx context.Context, planID id.ID) ([]execution.Reservation, error) {
	rows, err := s.driver.Select(ctx, Query{Kind: kindReservation, RunID: planID.String(), Limit: execution.MaxReservations + 1})
	if err != nil {
		return nil, err
	}
	if len(rows) > execution.MaxReservations {
		return nil, execution.ErrInvalidReservation
	}
	reservations := make([]execution.Reservation, 0, len(rows))
	indices := make(map[id.ID]int, len(rows))
	for _, row := range rows {
		if checkErr := validateRecord(ctx, row); checkErr != nil {
			return nil, checkErr
		}
		r, decodeErr := decodeReservation(row)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if r.Request.PlanID != planID {
			return nil, execution.ErrInvalidReservation
		}
		indices[r.Request.ID] = len(reservations)
		reservations = append(reservations, r)
	}
	settled, err := s.driver.Select(ctx, Query{Kind: kindSettlement, RunID: planID.String(), Limit: execution.MaxReservations + 1})
	if err != nil {
		return nil, err
	}
	if len(settled) > len(reservations) {
		return nil, execution.ErrInvalidReservation
	}
	for _, row := range settled {
		if checkErr := validateRecord(ctx, row); checkErr != nil {
			return nil, checkErr
		}
		receipt, decodeErr := decodeSettlement(row)
		if decodeErr != nil {
			return nil, decodeErr
		}
		index, ok := indices[receipt.Request.ReservationID]
		if !ok || reservations[index].Settlement != nil {
			return nil, execution.ErrInvalidReservation
		}
		reservations[index].Settlement = &receipt
		if reservations[index].Validate() != nil {
			return nil, execution.ErrInvalidReservation
		}
	}
	return reservations, nil
}

// Reserve admits capacity under the plan lock before any worker dispatch.
func (s *Repository) Reserve(ctx context.Context, request execution.ReservationRequest) (execution.Reservation, error) {
	if err := require(ctx, request.Scope); err != nil {
		return execution.Reservation{}, err
	}
	req, digest, err := request.Freeze()
	if err != nil {
		return execution.Reservation{}, err
	}
	if req.PlanID.Prefix() != planning.PrefixPlan || req.TaskID.Prefix() != planning.PrefixTask {
		return execution.Reservation{}, execution.ErrInvalidReservation
	}
	var result execution.Reservation
	err = s.planTransaction(ctx, req.PlanID, func(tc context.Context, tx *Repository) error {
		var reserveErr error
		result, _, reserveErr = tx.reserveInTransaction(tc, req, digest)
		return reserveErr
	})
	if errors.Is(err, ErrExists) {
		err = execution.ErrReservationConflict
	}
	if err != nil {
		return execution.Reservation{}, err
	}
	return result, nil
}

// reserveInTransaction requires a frozen request and an existing plan transaction.
func (s *Repository) reserveInTransaction(ctx context.Context, req execution.ReservationRequest, digest string) (execution.Reservation, bool, error) {
	cached, readErr := s.GetReservation(ctx, req.ID)
	if readErr == nil {
		if cached.RequestDigest != digest {
			return execution.Reservation{}, false, execution.ErrReservationConflict
		}
		return cached, false, nil
	}
	if !errors.Is(readErr, execution.ErrReservationNotFound) {
		return execution.Reservation{}, false, readErr
	}
	policy, p, readErr := s.effectiveReservationPolicy(ctx, req.PlanID)
	if readErr != nil {
		return execution.Reservation{}, false, readErr
	}
	if req.PlanDigest != p.Digest {
		return execution.Reservation{}, false, execution.ErrReservationConflict
	}
	found := false
	for _, task := range p.Tasks {
		if task.ID == req.TaskID {
			found = true
			if req.Allocation.Steps > int64(task.Budget.MaxSteps) || req.Allocation.Tokens > int64(task.Budget.MaxTokens) || req.Allocation.ToolCalls > int64(task.Budget.MaxToolCalls) || req.Deadline.After(task.Budget.Deadline) {
				return execution.Reservation{}, false, execution.ErrBudgetExceeded
			}
			break
		}
	}
	if !found {
		return execution.Reservation{}, false, execution.ErrInvalidReservation
	}
	reservations, readErr := s.accountReservations(ctx, req.PlanID)
	if readErr != nil {
		return execution.Reservation{}, false, readErr
	}
	return s.admitReservationInTransaction(ctx, policy, reservations, req, digest)
}

func (s *Repository) admitReservationInTransaction(ctx context.Context, policy execution.ReservationPolicy, reservations []execution.Reservation, req execution.ReservationRequest, digest string) (execution.Reservation, bool, error) {
	at := s.clock.Now()
	if admitErr := execution.AdmitReservation(policy, reservations, req, at); admitErr != nil {
		return execution.Reservation{}, false, admitErr
	}
	next := execution.Reservation{Request: req, RequestDigest: digest, CreatedAt: at}
	row, buildErr := reservationRecord(next)
	if buildErr != nil {
		return execution.Reservation{}, false, buildErr
	}
	if insertErr := s.driver.Insert(ctx, row); insertErr != nil {
		return execution.Reservation{}, false, insertErr
	}
	observed := s.clock.Now()
	if observed.IsZero() || observed.Before(at) || !observed.Before(req.Deadline) || !observed.Before(policy.Deadline) {
		return execution.Reservation{}, false, execution.ErrBudgetExceeded
	}
	return next, true, nil
}

// Settle retains final usage once. It remains available after expiry or replan so
// unknown work is not refunded and late provider overruns can be accounted for.
func (s *Repository) Settle(ctx context.Context, settlement execution.Settlement) error {
	req, digest, err := settlement.Freeze()
	if err != nil {
		return err
	}
	initial, err := s.GetReservation(ctx, req.ReservationID)
	if err != nil {
		return err
	}
	err = s.planTransaction(ctx, initial.Request.PlanID, func(tc context.Context, tx *Repository) error {
		_, settleErr := tx.settleInTransaction(tc, req, digest)
		return settleErr
	})
	if errors.Is(err, ErrExists) {
		return execution.ErrReservationConflict
	}
	return err
}

// settleInTransaction requires a frozen request and the reservation's plan lock.
func (s *Repository) settleInTransaction(ctx context.Context, req execution.Settlement, digest string) (execution.SettlementReceipt, error) {
	return s.settleAccountInTransaction(ctx, req, digest, s.GetReservationPolicy)
}

func (s *Repository) settleAccountInTransaction(ctx context.Context, req execution.Settlement, digest string, readPolicy func(context.Context, id.ID) (execution.ReservationPolicy, error)) (execution.SettlementReceipt, error) {
	current, err := s.GetReservation(ctx, req.ReservationID)
	if err != nil {
		return execution.SettlementReceipt{}, err
	}
	if current.Settlement != nil {
		if current.Settlement.Digest != digest {
			return execution.SettlementReceipt{}, execution.ErrReservationConflict
		}
		return *current.Settlement, nil
	}
	if current.RequestDigest != req.RequestDigest {
		return execution.SettlementReceipt{}, execution.ErrReservationConflict
	}
	policy, err := readPolicy(ctx, current.Request.PlanID)
	if err != nil {
		return execution.SettlementReceipt{}, err
	}
	reservations, err := s.accountReservations(ctx, current.Request.PlanID)
	if err != nil {
		return execution.SettlementReceipt{}, err
	}
	receipt := execution.SettlementReceipt{Request: req, Digest: digest, At: s.clock.Now()}
	found := false
	for i := range reservations {
		if reservations[i].Request.ID == req.ReservationID {
			reservations[i].Settlement = &receipt
			found = true
			break
		}
	}
	if !found {
		return execution.SettlementReceipt{}, execution.ErrReservationNotFound
	}
	if _, err = execution.AccountReservations(policy, reservations); err != nil {
		return execution.SettlementReceipt{}, err
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return execution.SettlementReceipt{}, err
	}
	row, err := record(kindSettlement, req.ReservationID.String(), evidence.Digest(data), current.Request.PlanID.String(), current.Request.Scope, receipt)
	if err != nil {
		return execution.SettlementReceipt{}, err
	}
	if insertErr := s.driver.Insert(ctx, row); insertErr != nil {
		return execution.SettlementReceipt{}, insertErr
	}
	return receipt, nil
}

// ListReservations is bounded discovery, not a transactional accounting view.
func (s *Repository) ListReservations(ctx context.Context, req execution.ReservationListRequest) ([]execution.Reservation, error) {
	if req.PlanID.IsNil() || req.PlanID.Prefix() != planning.PrefixPlan || req.Limit < 1 || req.Limit > 256 || (!req.After.IsNil() && req.After.Prefix() != execution.PrefixReservation) {
		return nil, execution.ErrInvalidReservation
	}
	return s.listReservations(ctx, req)
}

func (s *Repository) listReservations(ctx context.Context, req execution.ReservationListRequest) ([]execution.Reservation, error) {
	if _, err := cortex.RequireScope(ctx); err != nil {
		return nil, err
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindReservation, RunID: req.PlanID.String(), After: req.After.String(), Limit: req.Limit})
	if err != nil {
		return nil, err
	}
	result := make([]execution.Reservation, 0, len(rows))
	for _, row := range rows {
		if checkErr := validateRecord(ctx, row); checkErr != nil {
			return nil, checkErr
		}
		r, decodeErr := decodeReservation(row)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if r.Request.PlanID != req.PlanID {
			return nil, execution.ErrInvalidReservation
		}
		current, readErr := s.GetReservation(ctx, r.Request.ID)
		if readErr != nil {
			return nil, readErr
		}
		result = append(result, current)
	}
	return result, nil
}

// GetBudgetSnapshot serializes aggregate reads with every reservation/settlement.
// Its policy reflects current plan narrowing; GetReservationPolicy is immutable.
func (s *Repository) GetBudgetSnapshot(ctx context.Context, planID id.ID) (execution.BudgetSnapshot, error) {
	var result execution.BudgetSnapshot
	err := s.planTransaction(ctx, planID, func(tc context.Context, tx *Repository) error {
		policy, _, readErr := tx.effectiveReservationPolicy(tc, planID)
		if readErr != nil {
			return readErr
		}
		reservations, readErr := tx.accountReservations(tc, planID)
		if readErr != nil {
			return readErr
		}
		var accountErr error
		result, accountErr = execution.AccountReservations(policy, reservations)
		return accountErr
	})
	if err != nil {
		return execution.BudgetSnapshot{}, err
	}
	return result, nil
}
