package assurance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/run"
)

const (
	kindPlanReplan        = "plan_replan"
	kindPlanReplanOutcome = "plan_replan_outcome"
	kindPlanReplanCommit  = "plan_replan_commit"
)

var _ coordination.PlanReplanStore = (*Repository)(nil)

// GetPlanReplan returns nil for an absent attempt, never fresh dispatch permission.
func (s *Repository) GetPlanReplan(ctx context.Context, key id.ID) (*coordination.PlanReplanAdmission, error) {
	if key.IsNil() || key.Prefix() != coordination.PrefixPlanReplan {
		return nil, planning.ErrInvalid
	}
	row, err := s.get(ctx, Query{Kind: kindPlanReplan, ID: key.String()})
	if errors.Is(err, ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodePlanReplan(row)
}
func decodePlanReplan(row Record) (*coordination.PlanReplanAdmission, error) {
	var a coordination.PlanReplanAdmission
	if row.Deleted || len(row.Body) > 2<<20 || json.Unmarshal([]byte(row.Body), &a) != nil || a.Validate() != nil || row.Kind != kindPlanReplan || row.ID != a.Request.ID.String() || row.Digest != a.RequestDigest || row.RunID != a.Request.Progress.Plan.ID.String() || row.ScopeCanon != a.Request.Progress.Plan.Scope.Canonical() {
		return nil, planning.ErrInvalid
	}
	return &a, nil
}

// ListPlanReplans returns the bounded lifetime attempt history in exact scope.
func (s *Repository) ListPlanReplans(ctx context.Context, planID id.ID) ([]coordination.PlanReplanAdmission, error) {
	if planID.IsNil() || planID.Prefix() != planning.PrefixPlan {
		return nil, planning.ErrInvalid
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindPlanReplan, RunID: planID.String(), Limit: planning.MaxReplanAttempts + 1})
	if err != nil {
		return nil, err
	}
	if len(rows) > planning.MaxReplanAttempts {
		return nil, planning.ErrInvalid
	}
	out := make([]coordination.PlanReplanAdmission, 0, len(rows))
	for _, row := range rows {
		if checkErr := validateRecord(ctx, row); checkErr != nil {
			return nil, checkErr
		}
		a, decodeErr := decodePlanReplan(row)
		if decodeErr != nil {
			return nil, decodeErr
		}
		out = append(out, *a)
	}
	return out, nil
}

// AdmitPlanReplan serializes immutable proposal intent with a charged reservation.
// A lost acknowledgment never returns a fresh permission on a retry.
func (s *Repository) AdmitPlanReplan(ctx context.Context, request coordination.PlanReplanRequest) (coordination.PlanReplanAdmission, error) {
	req, digest, err := request.Freeze()
	if err != nil {
		return coordination.PlanReplanAdmission{}, err
	}
	if checkErr := require(ctx, req.Progress.Plan.Scope); checkErr != nil {
		return coordination.PlanReplanAdmission{}, checkErr
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result coordination.PlanReplanAdmission
	err = s.planTransaction(bounded, req.Progress.Plan.ID, func(tc context.Context, tx *Repository) error {
		result = coordination.PlanReplanAdmission{}
		retained, readErr := tx.GetPlanReplan(tc, req.ID)
		if readErr != nil {
			return readErr
		}
		if retained != nil {
			if retained.RequestDigest != digest {
				return planning.ErrConflict
			}
			result = *retained
			return nil
		}
		current, readErr := tx.GetPlan(tc, req.Progress.Plan.ID)
		if readErr != nil {
			return readErr
		}
		if current.Digest != req.Progress.Plan.Digest {
			return planning.ErrConflict
		}
		now := s.clock.Now()
		if now.Before(req.Progress.At) || now.Sub(req.Progress.At) > 30*time.Second || !now.Before(req.Reservation.Deadline) {
			return planning.ErrConflict
		}
		attempts, readErr := tx.ListPlanReplans(tc, current.ID)
		if readErr != nil {
			return readErr
		}
		if len(attempts) >= req.Policy.MaxReplans {
			return execution.ErrBudgetExceeded
		}
		for _, attempt := range attempts {
			outcome, outErr := tx.GetPlanReplanOutcome(tc, attempt.Request.ID)
			if outErr != nil {
				return outErr
			}
			if outcome == nil {
				return planning.ErrConflict
			}
			if outcome.Proposed != nil && attempt.Request.Progress.Plan.Digest == current.Digest {
				_, commitErr := tx.get(tc, Query{Kind: kindPlanReplanCommit, ID: attempt.Request.ID.String()})
				if errors.Is(commitErr, ErrAbsent) {
					return planning.ErrConflict
				}
				if commitErr != nil {
					return commitErr
				}
			}
		}
		policy, _, readErr := tx.effectiveReservationPolicy(tc, current.ID)
		if readErr != nil {
			return readErr
		}
		reservations, readErr := tx.accountReservations(tc, current.ID)
		if readErr != nil {
			return readErr
		}
		account, readErr := execution.AccountReservations(policy, reservations)
		if readErr != nil {
			return readErr
		}
		if !reflect.DeepEqual(account, req.Progress.Account) {
			return planning.ErrConflict
		}
		progress := req.Progress
		progress.At = now
		progress.Account = account
		if planning.ProposeReplan(progress, req.Policy).Action != planning.ReplanPropose {
			return planning.ErrConflict
		}
		_, reservationDigest, freezeErr := req.Reservation.Freeze()
		if freezeErr != nil {
			return freezeErr
		}
		reservation, fresh, reserveErr := tx.reserveInTransaction(tc, req.Reservation, reservationDigest)
		if reserveErr != nil {
			return reserveErr
		}
		if !fresh {
			return execution.ErrReservationConflict
		}
		result = coordination.PlanReplanAdmission{SchemaVersion: 1, Request: req, RequestDigest: digest, Reservation: reservation, At: reservation.CreatedAt}
		if result.Validate() != nil {
			return planning.ErrInvalid
		}
		row, encodeErr := record(kindPlanReplan, req.ID.String(), digest, current.ID.String(), current.Scope, result)
		if encodeErr != nil {
			return encodeErr
		}
		if insertErr := tx.driver.Insert(tc, row); insertErr != nil {
			return insertErr
		}
		result.Fresh = true
		return nil
	})
	if err != nil {
		return coordination.PlanReplanAdmission{}, err
	}
	if checkErr := bounded.Err(); checkErr != nil {
		return coordination.PlanReplanAdmission{}, checkErr
	}
	return result, nil
}

// GetPlanReplanOutcome returns immutable observed disposition and accounting.
func (s *Repository) GetPlanReplanOutcome(ctx context.Context, key id.ID) (*coordination.PlanReplanOutcome, error) {
	if key.IsNil() || key.Prefix() != coordination.PrefixPlanReplan {
		return nil, planning.ErrInvalid
	}
	row, err := s.get(ctx, Query{Kind: kindPlanReplanOutcome, ID: key.String()})
	if errors.Is(err, ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var o coordination.PlanReplanOutcome
	if row.Deleted || len(row.Body) > 2<<20 || json.Unmarshal([]byte(row.Body), &o) != nil || o.Validate() != nil || row.ID != o.AttemptID.String() || row.Digest != o.Digest {
		return nil, planning.ErrInvalid
	}
	admission, readErr := s.GetPlanReplan(ctx, key)
	if readErr != nil {
		return nil, readErr
	}
	if admission == nil || o.AdmissionDigest != admission.RequestDigest || row.Kind != kindPlanReplanOutcome || row.ID != key.String() || row.RunID != admission.Request.Progress.Plan.ID.String() || row.ScopeCanon != admission.Request.Progress.Plan.Scope.Canonical() || o.Settlement.Request.ReservationID != admission.Reservation.Request.ID || o.Settlement.Request.RequestDigest != admission.Reservation.RequestDigest {
		return nil, planning.ErrInvalid
	}
	return &o, nil
}

// ObservePlanReplan reads the exact retained runtime under its write lock. A
// missing, running or paused run stays pending unless permanent abandonment is
// authenticated by the caller. Model JSON is bounded and strictly decoded.
func (s *Repository) ObservePlanReplan(ctx context.Context, control coordination.PlanReplanControl) (*coordination.PlanReplanOutcome, error) {
	req, _, err := control.Freeze()
	if err != nil {
		return nil, err
	}
	if req.Operation != "observe" {
		return nil, planning.ErrInvalid
	}
	if checkErr := require(ctx, req.Request.Progress.Plan.Scope); checkErr != nil {
		return nil, checkErr
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result *coordination.PlanReplanOutcome
	err = s.planTransaction(bounded, req.Request.Progress.Plan.ID, func(tc context.Context, tx *Repository) error {
		result = nil
		a, readErr := tx.requireReplan(tc, req.Request)
		if readErr != nil {
			return readErr
		}
		retained, readErr := tx.GetPlanReplanOutcome(tc, a.Request.ID)
		if readErr != nil {
			return readErr
		}
		if retained != nil {
			if retained.AdmissionDigest != a.RequestDigest {
				return planning.ErrConflict
			}
			result = retained
			return nil
		}
		reader, ok := tx.driver.(SubmissionRunReader)
		if !ok {
			return coordination.ErrAssignmentUnavailable
		}
		current, readErr := reader.LockSubmissionRun(tc, a.Request.RunID)
		if readErr != nil && !errors.Is(readErr, cortex.ErrRunNotFound) {
			return readErr
		}
		now := s.clock.Now()
		out := coordination.PlanReplanOutcome{SchemaVersion: 1, AttemptID: a.Request.ID, AdmissionDigest: a.RequestDigest, At: now}
		var minimum execution.Resources
		if req.Abandonment != nil {
			proof := req.Abandonment
			if now.Before(proof.ObservedAt) || !now.Before(proof.ExpiresAt) || proof.ObservedAt.Before(a.At) {
				return planning.ErrConflict
			}
			out.Abandonment = proof
			if current != nil {
				if checkErr := checkReplanRun(*a, current); checkErr != nil {
					return checkErr
				}
				minimum = preparedProposalUsage(current)
			}
		} else {
			if current == nil {
				return nil
			}
			if checkErr := checkReplanRun(*a, current); checkErr != nil {
				return checkErr
			}
			switch current.State {
			case run.StateCompleted, run.StateFailed, run.StateCancelled, run.StateIncomplete:
			default:
				return nil
			}
			if current.CompletedAt == nil || current.CompletedAt.Before(a.At) || current.CompletedAt.After(now) {
				return planning.ErrConflict
			}
			data, encodeErr := json.Marshal(current)
			if encodeErr != nil {
				return encodeErr
			}
			out.RunDigest = evidence.Digest(data)
			out.RunRevision = current.Revision
			minimum = preparedProposalUsage(current)
			if current.State == run.StateCompleted {
				out.Proposed = parseReplanOutput(*a, current.Output)
			}
		}
		settlement := execution.Settlement{ReservationID: a.Reservation.Request.ID, RequestDigest: a.Reservation.RequestDigest, Usage: execution.ReservationUsage{Known: execution.KnownReplans, Actual: execution.Resources{Replans: 1}}, Minimum: &minimum, Receipt: a.RequestDigest}
		frozen, digest, freezeErr := settlement.Freeze()
		if freezeErr != nil {
			return freezeErr
		}
		receipt, settleErr := tx.settleInTransaction(tc, frozen, digest)
		if settleErr != nil {
			return settleErr
		}
		out.Settlement = receipt
		out.At = s.clock.Now()
		sealed, sealErr := coordination.SealPlanReplanOutcome(out)
		if sealErr != nil {
			return sealErr
		}
		row, encodeErr := record(kindPlanReplanOutcome, a.Request.ID.String(), sealed.Digest, a.Request.Progress.Plan.ID.String(), a.Request.Progress.Plan.Scope, sealed)
		if encodeErr != nil {
			return encodeErr
		}
		if insertErr := tx.driver.Insert(tc, row); insertErr != nil {
			return insertErr
		}
		if req.Abandonment != nil && !s.clock.Now().Before(req.Abandonment.ExpiresAt) {
			return planning.ErrConflict
		}
		result = &sealed
		return nil
	})
	if err != nil {
		return nil, err
	}
	if checkErr := bounded.Err(); checkErr != nil {
		return nil, checkErr
	}
	return result, nil
}
func (s *Repository) requireReplan(ctx context.Context, req coordination.PlanReplanRequest) (*coordination.PlanReplanAdmission, error) {
	_, digest, err := req.Freeze()
	if err != nil {
		return nil, err
	}
	a, err := s.GetPlanReplan(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	if a == nil || a.RequestDigest != digest {
		return nil, planning.ErrConflict
	}
	return a, nil
}
func checkReplanRun(a coordination.PlanReplanAdmission, r *run.Run) error {
	req := a.Request
	return validatePreparedProposalRun(preparedProposalBinding{RunID: req.RunID, SessionID: req.SessionID, AgentID: req.AgentID, Scope: req.Progress.Plan.Scope, PreparedDigest: req.PreparedDigest, InputDigest: req.InputDigest, NotBefore: a.At, Reservation: req.Reservation}, r)
}
func parseReplanOutput(a coordination.PlanReplanAdmission, output string) *planning.Plan {
	if len(output) > 1<<20 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewBufferString(output))
	decoder.DisallowUnknownFields()
	var proposed planning.Plan
	if decoder.Decode(&proposed) != nil {
		return nil
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil
	}
	next, err := planning.ApplyRevision(a.Request.Progress.Plan, planning.Revision{Plan: proposed, Actor: a.Request.Actor, Reason: a.Request.Reason}, a.Request.Progress.Plan.Revision, planning.Limits{})
	if err != nil {
		return nil
	}
	return &next
}
