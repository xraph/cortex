package state

import (
	"bytes"
	"context"
	"math"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/internal/canonical"
)

// GetReservation reads a call's conservative debit and complete receipt history.
func (s *Store) GetReservation(ctx context.Context, experimentID id.ID, callKey string) (optimization.Reservation, error) {
	var empty optimization.Reservation
	key, err := contextScope(ctx)
	if err != nil {
		return empty, err
	}
	unlock := s.lockOperation()
	defer unlock()
	if s.closed {
		return empty, optimization.ErrUnavailable
	}
	data, loadErr := s.loadResource(ctx, key, RecordKey{Kind: "reservations", Key: callKey, Parent: experimentID.String()})
	if loadErr != nil {
		return empty, loadErr
	}
	if data == nil {
		return empty, optimization.ErrNotFound
	}
	value, exists := data.Reservations[experimentID.String()][callKey]
	if !exists {
		return empty, optimization.ErrNotFound
	}
	return clone(value)
}

func (tx *transaction) Reserve(request optimization.UsageReservation) (optimization.Reservation, error) {
	var empty optimization.Reservation
	experiment, err := tx.reservationExperiment(request)
	if err != nil {
		return empty, err
	}
	key := request.ExperimentID.String()
	if validationErr := validateReservation(request, experiment.Spec.Budget, tx.now, experiment.Deadline); validationErr != nil {
		return empty, validationErr
	}
	entries := tx.data.Reservations[key]
	if previous, found := entries[request.CallKey]; found {
		if !sameReservationRequest(previous.Request, request) {
			return empty, optimization.ErrConflict
		}
		return clone(previous)
	}
	tokens, money := debits(entries)
	budget := experiment.Spec.Budget
	if tokens > budget.MaxTokens || request.MaxTokens > budget.MaxTokens-tokens || (!budget.TokenOnly && (money > budget.MaxUSDMicros || request.MaxUSDMicros > budget.MaxUSDMicros-money)) {
		return empty, optimization.ErrBudgetExceeded
	}
	value := optimization.Reservation{Request: request, State: optimization.ReservationHeld, Revision: 1, History: []optimization.UsageSettlement{}, DebitedTokens: request.MaxTokens, DebitedUSDMicros: request.MaxUSDMicros, CreatedAt: tx.now}
	detached, err := clone(value)
	if err != nil {
		return empty, err
	}
	if entries == nil {
		entries = make(map[string]optimization.Reservation)
		tx.data.Reservations[key] = entries
	}
	entries[request.CallKey] = detached
	if err := tx.appendUsageEvent(experiment, "usage_reserved", detached, request.Lease.Owner); err != nil {
		return empty, err
	}
	return clone(detached)
}

func (tx *transaction) reservationExperiment(request optimization.UsageReservation) (optimization.Experiment, error) {
	var empty optimization.Experiment
	if !tx.active {
		return empty, optimization.ErrConflict
	}
	if err := tx.ctx.Err(); err != nil {
		return empty, err
	}
	experiment, exists := tx.data.Experiments[request.ExperimentID.String()]
	if !exists {
		return empty, optimization.ErrNotFound
	}
	if experiment.State == optimization.StateBudgetExhausted || !tx.now.Before(experiment.Deadline) {
		return empty, optimization.ErrBudgetExceeded
	}
	if !tx.validLease(experiment, request.Lease) {
		return empty, optimization.ErrConflict
	}
	return experiment, nil
}

func sameReservationRequest(left, right optimization.UsageReservation) bool {
	// Recovery changes authority, never the frozen call content.
	left.Lease, right.Lease = optimization.WorkLease{}, optimization.WorkLease{}
	return same(left, right)
}

func validateReservation(request optimization.UsageReservation, budget optimization.Budget, now, deadline time.Time) error {
	if !optimization.ValidOpaqueKey(request.CallKey) || request.MaxTokens < 0 || request.MaxUSDMicros < 0 || (request.MaxTokens == 0 && request.MaxUSDMicros == 0) || !request.ExpiresAt.After(now) || request.ExpiresAt.After(deadline) {
		return optimization.ErrInvalidSpec
	}
	if _, err := optimization.ParseID(request.AttemptID.String(), optimization.PrefixAttempt); err != nil {
		return err
	}
	switch request.Operation {
	case optimization.OperationBaseline, optimization.OperationProposal, optimization.OperationProbe, optimization.OperationComposition, optimization.OperationValidation, optimization.OperationReflection, optimization.OperationJudge, optimization.OperationEmbedding:
		if request.MaxTokens == 0 {
			return optimization.ErrInvalidSpec
		}
	case optimization.OperationRetrieval, optimization.OperationTool:
	default:
		return optimization.ErrInvalidSpec
	}
	if budget.TokenOnly {
		if request.MaxUSDMicros != 0 || request.PricingRevision != "" {
			return optimization.ErrInvalidSpec
		}
	} else if request.PricingRevision != budget.PricingRevision {
		return optimization.ErrInvalidSpec
	}
	return nil
}

func (tx *transaction) Settle(settlement optimization.UsageSettlement) error {
	if !tx.active {
		return optimization.ErrConflict
	}
	if err := tx.ctx.Err(); err != nil {
		return err
	}
	key := settlement.ExperimentID.String()
	experiment, exists := tx.data.Experiments[key]
	if !exists {
		return optimization.ErrNotFound
	}
	if !tx.validLease(experiment, settlement.Lease) {
		return optimization.ErrConflict
	}
	if !optimization.ValidOpaqueKey(settlement.Receipt) || !optimization.ValidOpaqueKey(settlement.CallKey) {
		return optimization.ErrInvalidSpec
	}
	if err := settlement.Usage.Validate(); err != nil {
		return err
	}
	if settlement.Usage.Receipt != "" && settlement.Usage.Receipt != settlement.Receipt {
		return optimization.ErrInvalidSpec
	}
	entries := tx.data.Reservations[key]
	reserved, exists := entries[settlement.CallKey]
	if !exists {
		return optimization.ErrNotFound
	}
	if settlement.AttemptID != reserved.Request.AttemptID {
		return optimization.ErrConflict
	}
	budget := experiment.Spec.Budget
	priced := settlement.Usage.MoneyQuality == optimization.UsageKnown || settlement.Usage.MoneyQuality == optimization.UsageEstimated
	if !budget.TokenOnly && priced && settlement.Usage.PricingRevision != budget.PricingRevision {
		return optimization.ErrInvalidSpec
	}
	receipts := tx.data.Receipts[key]
	if callKey, found := receipts[settlement.Receipt]; found && callKey != settlement.CallKey {
		return optimization.ErrConflict
	}
	for _, prior := range reserved.History {
		if prior.Receipt != settlement.Receipt {
			continue
		}
		left, right := prior, settlement
		left.Lease, right.Lease = optimization.WorkLease{}, optimization.WorkLease{}
		left.ObservedAt, right.ObservedAt = time.Time{}, time.Time{}
		if !same(left, right) {
			return optimization.ErrConflict
		}
		return nil
	}
	if reserved.State == optimization.ReservationSettled {
		return optimization.ErrConflict
	}
	knownTokens := settlement.Complete && settlement.Usage.TokenQuality == optimization.UsageKnown
	knownMoney := settlement.Complete && settlement.Usage.MoneyQuality == optimization.UsageKnown
	if knownTokens {
		reserved.DebitedTokens = settlement.Usage.Tokens
	} else {
		reserved.DebitedTokens = max(reserved.DebitedTokens, settlement.Usage.Tokens)
	}
	if knownMoney {
		reserved.DebitedUSDMicros = settlement.Usage.USDMicros
	} else {
		reserved.DebitedUSDMicros = max(reserved.DebitedUSDMicros, settlement.Usage.USDMicros)
	}
	reserved.State = optimization.ReservationUnknown
	if knownTokens && (budget.TokenOnly || knownMoney) {
		reserved.State = optimization.ReservationSettled
	}
	settlement.ObservedAt = tx.now
	reserved.Settlement = &settlement
	reserved.History = append(reserved.History, settlement)
	reserved.Revision++
	detached, err := clone(reserved)
	if err != nil {
		return err
	}
	entries[settlement.CallKey] = detached
	if receipts == nil {
		receipts = make(map[string]string)
		tx.data.Receipts[key] = receipts
	}
	receipts[settlement.Receipt] = settlement.CallKey
	if err := tx.appendUsageEvent(experiment, "usage_"+string(reserved.State), detached, settlement.Lease.Owner); err != nil {
		return err
	}
	// Retain overruns in full. The worker handles stopping active environments;
	// the ledger blocks every subsequent reservation immediately.
	return nil
}

func (tx *transaction) appendUsageEvent(experiment optimization.Experiment, eventType string, value optimization.Reservation, owner string) error {
	payload := struct {
		CallKey       string                      `json:"call_key"`
		Operation     optimization.OperationClass `json:"operation"`
		Receipt       string                      `json:"receipt,omitempty"`
		Revision      int64                       `json:"reservation_revision"`
		Tokens        int64                       `json:"tokens_debited"`
		Money         int64                       `json:"usd_micros_debited"`
		RequestDigest string                      `json:"request_digest,omitempty"`
	}{CallKey: value.Request.CallKey, Operation: value.Request.Operation, Revision: value.Revision, Tokens: value.DebitedTokens, Money: value.DebitedUSDMicros}
	if value.Settlement != nil {
		payload.Receipt = value.Settlement.Receipt
	}
	if value.Call != nil {
		payload.RequestDigest = value.Call.RequestDigest
	}
	encoded, err := canonical.Marshal(payload)
	if err != nil {
		return err
	}
	event := tx.appendEvent(experiment, eventType, optimization.IdentityRef{Subject: owner})
	event.Resources = append(event.Resources, value.Request.AttemptID)
	event.Payload = encoded
	return nil
}

func debits(entries map[string]optimization.Reservation) (tokens, money int64) {
	for _, value := range entries {
		tokens = addDebit(tokens, value.DebitedTokens)
		money = addDebit(money, value.DebitedUSDMicros)
	}
	return tokens, money
}

func addDebit(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func same(a, b any) bool {
	left, leftErr := canonical.Marshal(a)
	right, rightErr := canonical.Marshal(b)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}
