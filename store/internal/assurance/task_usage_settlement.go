package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
)

const kindTaskUsageSettlement = "task_usage_settlement"

var _ coordination.TaskUsageSettlementStore = (*Repository)(nil)

// GetTaskUsageSettlement reads historical accounting without granting authority.
func (s *Repository) GetTaskUsageSettlement(ctx context.Context, runID id.AgentRunID) (*coordination.TaskUsageSettlement, error) {
	if runID.IsNil() || runID.Prefix() != id.PrefixAgentRun {
		return nil, coordination.ErrInvalidTaskSettlement
	}
	row, err := s.get(ctx, Query{Kind: kindTaskUsageSettlement, ID: runID.String()})
	if errors.Is(err, ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var settled coordination.TaskUsageSettlement
	if row.Deleted || len(row.Body) > 64<<10 || json.Unmarshal([]byte(row.Body), &settled) != nil || settled.Validate() != nil || row.Kind != kindTaskUsageSettlement || row.ID != runID.String() || row.RunID != runID.String() || settled.Request.Usage.Seal.Request.RunID != runID || row.ScopeCanon != settled.Request.Usage.Seal.Request.Scope.Canonical() || row.Digest != settled.Digest {
		return nil, coordination.ErrInvalidTaskSettlement
	}
	return &settled, nil
}

// SettleTaskUsage compares complete evidence and writes accounting and its guard
// receipt atomically. Callers must authenticate Billing and hold host authority.
func (s *Repository) SettleTaskUsage(ctx context.Context, request coordination.TaskUsageSettlementRequest) (coordination.TaskUsageSettlement, error) {
	req, digest, err := request.Freeze()
	if err != nil {
		return coordination.TaskUsageSettlement{}, err
	}
	if scopeErr := require(ctx, req.Usage.Seal.Request.Scope); scopeErr != nil {
		return coordination.TaskUsageSettlement{}, scopeErr
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result coordination.TaskUsageSettlement
	err = s.planTransaction(bounded, req.Usage.Claim.PlanID, func(tc context.Context, tx *Repository) error {
		result = coordination.TaskUsageSettlement{}
		retained, readErr := tx.GetTaskUsageSettlement(tc, req.Usage.Seal.Request.RunID)
		if readErr != nil {
			return readErr
		}
		if retained != nil {
			if retained.RequestDigest != digest {
				return coordination.ErrTaskSettlementConflict
			}
			allocation, allocationErr := tx.GetReservation(tc, req.ReservationID)
			if allocationErr != nil {
				return allocationErr
			}
			if allocation.Settlement == nil || allocation.Settlement.Digest != retained.Settlement.Digest || !allocation.Settlement.At.Equal(retained.Settlement.At) {
				return coordination.ErrTaskSettlementConflict
			}
			result = *retained
			return nil
		}
		snapshot, readErr := tx.readTaskUsageSnapshot(tc, req.Usage)
		if readErr != nil {
			return readErr
		}
		closed, readErr := tx.GetTaskUsageClosure(tc, req.Usage.Seal.Request.RunID)
		if readErr != nil {
			return readErr
		}
		if closed == nil {
			return coordination.ErrTaskSettlementConflict
		}
		if terminalErr := tx.checkTaskUsageTermination(tc, snapshot.Claim, closed.Request); terminalErr != nil {
			return terminalErr
		}
		binding, readErr := tx.getClaimBudget(tc, snapshot.Claim.ID)
		if errors.Is(readErr, ErrAbsent) {
			return coordination.ErrTaskSettlementConflict
		}
		if readErr != nil {
			return readErr
		}
		original, current := binding.Result.Claim, snapshot.Claim
		if original.PlanID != current.PlanID || original.PlanDigest != current.PlanDigest || original.PlanRevision != current.PlanRevision || original.TaskID != current.TaskID || original.Worker != current.Worker || original.Fence != current.Fence || original.Scope.Canonical() != current.Scope.Canonical() || binding.Result.Reservation.Request.ID != req.ReservationID || binding.Result.Reservation.RequestDigest != req.ReservationDigest {
			return coordination.ErrTaskSettlementConflict
		}
		reservation, readErr := tx.GetReservation(tc, req.ReservationID)
		if readErr != nil {
			return readErr
		}
		settlement, prepareErr := coordination.PrepareTaskUsageSettlement(req, snapshot, *closed, reservation, s.clock.Now())
		if prepareErr != nil {
			return prepareErr
		}
		settlement, settlementDigest, freezeErr := settlement.Freeze()
		if freezeErr != nil {
			return freezeErr
		}
		accounting, settleErr := tx.settleInTransaction(tc, settlement, settlementDigest)
		if settleErr != nil {
			return settleErr
		}
		sealed, sealErr := coordination.SealTaskUsageSettlement(coordination.TaskUsageSettlement{SchemaVersion: 1, Request: req, RequestDigest: digest, Settlement: accounting})
		if sealErr != nil {
			return sealErr
		}
		runID := req.Usage.Seal.Request.RunID.String()
		row, recordErr := record(kindTaskUsageSettlement, runID, sealed.Digest, runID, req.Usage.Seal.Request.Scope, sealed)
		if recordErr != nil {
			return recordErr
		}
		if insertErr := tx.driver.Insert(tc, row); insertErr != nil {
			return insertErr
		}
		now := s.clock.Now()
		if now.Before(accounting.At) || !now.Before(req.Billing.ExpiresAt) {
			return coordination.ErrTaskSettlementConflict
		}
		result = sealed
		result.Fresh = true
		return nil
	})
	if err != nil {
		return coordination.TaskUsageSettlement{}, err
	}
	if contextErr := bounded.Err(); contextErr != nil {
		return coordination.TaskUsageSettlement{}, contextErr
	}
	return result, nil
}
