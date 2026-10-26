package assurance

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

const kindEffectContinuation = "effect_continuation"
const kindEffectContinuationResult = "effect_continuation_result"

var _ execution.EffectContinuationStore = (*Repository)(nil)

func decodeEffectContinuation(row Record) (execution.EffectContinuation, error) {
	var a execution.EffectContinuation
	if row.Deleted || len(row.Body) > 80<<10 || json.Unmarshal([]byte(row.Body), &a) != nil || a.Validate() != nil || row.Kind != kindEffectContinuation || row.ID != a.Request.Key() || row.Digest != a.RequestDigest || row.RunID != a.Request.Original.RunID.String() || row.ScopeCanon != a.Request.Original.Scope.Canonical() {
		return a, execution.ErrInvalidEffect
	}
	return a, nil
}
func (s *Repository) effectContinuation(ctx context.Context, r execution.EffectContinuationRequest) (*execution.EffectContinuation, error) {
	row, err := s.get(ctx, Query{Kind: kindEffectContinuation, ID: r.Key()})
	if errors.Is(err, ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a, err := decodeEffectContinuation(row)
	if err != nil {
		return nil, err
	}
	_, digest, err := r.Freeze()
	if err != nil || digest != a.RequestDigest {
		return nil, execution.ErrEffectConflict
	}
	return &a, nil
}

// ListEffectContinuations reads the finite sequence without granting dispatch.
func (s *Repository) ListEffectContinuations(ctx context.Context, original execution.EffectRequest) ([]execution.EffectContinuation, error) {
	o, digest, err := original.Freeze()
	if err != nil {
		return nil, err
	}
	if checkErr := require(ctx, o.Scope); checkErr != nil {
		return nil, checkErr
	}
	prefix := evidence.Digest([]byte(o.OperationKey)) + ":"
	rows, err := s.driver.Select(ctx, Query{Kind: kindEffectContinuation, After: prefix + "00", Limit: execution.MaxEffectContinuations + 1})
	if err != nil {
		return nil, err
	}
	var out []execution.EffectContinuation
	for _, row := range rows {
		if !strings.HasPrefix(row.ID, prefix) {
			break
		}
		if checkErr := validateRecord(ctx, row); checkErr != nil {
			return nil, checkErr
		}
		a, decodeErr := decodeEffectContinuation(row)
		if decodeErr != nil {
			return nil, decodeErr
		}
		_, actual, freezeErr := a.Request.Original.Freeze()
		if freezeErr != nil || actual != digest || a.Request.Sequence != len(out)+1 || len(out) >= execution.MaxEffectContinuations {
			return nil, execution.ErrEffectConflict
		}
		out = append(out, a)
	}
	return out, nil
}

// AdmitEffectContinuation serializes a new charged attempt with recovering
// ownership and the permanently sealed original journal. Host authority is held
// by EffectExecutor; storage validates bindings and finite conservation rules.
func (s *Repository) AdmitEffectContinuation(ctx context.Context, request execution.EffectContinuationRequest) (execution.EffectContinuation, error) {
	req, digest, err := request.Freeze()
	if err != nil {
		return execution.EffectContinuation{}, err
	}
	if checkErr := require(ctx, req.Original.Scope); checkErr != nil {
		return execution.EffectContinuation{}, checkErr
	}
	var result execution.EffectContinuation
	err = s.planTransaction(ctx, req.Reservation.PlanID, func(tc context.Context, tx *Repository) error {
		result = execution.EffectContinuation{}
		old, readErr := tx.effectContinuation(tc, req)
		if readErr != nil {
			return readErr
		}
		if old != nil {
			result = *old
			return nil
		}
		claim, readErr := tx.GetTaskClaim(tc, req.Reservation.PlanID, req.Reservation.TaskID)
		if readErr != nil {
			return readErr
		}
		if claim.State != coordination.ClaimRecovering || claim.Digest != req.ClaimDigest || claim.ID != req.Original.RunCall.ClaimID || claim.RunID != req.Original.RunID || claim.Worker.Agent != req.Original.RunCall.AgentID.String() {
			return coordination.ErrClaimConflict
		}
		snapshot, readErr := tx.readTaskUsageSnapshot(tc, coordination.TaskUsageRequest{Claim: claim.Token(), Seal: req.Seal, SnapshotDigest: req.SnapshotDigest})
		if readErr != nil {
			return readErr
		}
		reader, supported := tx.driver.(SubmissionRunReader)
		if !supported {
			return coordination.ErrAssignmentUnavailable
		}
		currentRun, runErr := reader.LockSubmissionRun(tc, claim.RunID)
		if runErr != nil {
			return runErr
		}
		call := req.Original.RunCall
		if currentRun == nil || currentRun.Provenance.TaskClaimID != claim.ID.String() || currentRun.Provenance.PreparedDigest != call.PreparedDigest || currentRun.Provenance.AssignmentDigest != call.AssignmentDigest || currentRun.Provenance.ParentRunID != call.ParentRunID.String() || currentRun.AgentID != call.AgentID {
			return execution.ErrEffectConflict
		}
		_, originalDigest, originalErr := req.Original.Freeze()
		if originalErr != nil {
			return originalErr
		}
		found := false
		for _, entry := range snapshot.Journal.Effects {
			_, d, e := entry.Request.Freeze()
			if e == nil && d == originalDigest {
				found = true
			}
		}
		if !found {
			return execution.ErrEffectConflict
		}
		now := tx.clock.Now()
		if now.Before(req.Proof.ObservedAt) || !now.Before(req.Proof.ExpiresAt) || !now.Before(req.Operation.Deadline) {
			return execution.ErrEffectConflict
		}
		history, readErr := tx.ListEffectContinuations(tc, req.Original)
		if readErr != nil {
			return readErr
		}
		if req.Sequence != len(history)+1 {
			return execution.ErrEffectConflict
		}
		if checkErr := tx.checkEffectContinuationPredecessor(tc, req, history, now); checkErr != nil {
			return checkErr
		}
		// Bound all follow-up records within a run, including different operations.
		rows, readErr := tx.driver.Select(tc, Query{Kind: kindEffectContinuation, RunID: req.Original.RunID.String(), Limit: execution.MaxRunRecoveryEntries})
		if readErr != nil {
			return readErr
		}
		if len(rows) >= execution.MaxRunRecoveryEntries {
			return execution.ErrBudgetExceeded
		}
		for _, row := range rows {
			a, decodeErr := decodeEffectContinuation(row)
			if decodeErr != nil {
				return decodeErr
			}
			if a.Request.Operation.AttemptID == req.Operation.AttemptID || a.Request.ResultID == req.ResultID {
				return execution.ErrEffectConflict
			}
		}
		_, rd, reservationErr := req.Reservation.Freeze()
		if reservationErr != nil {
			return reservationErr
		}
		reservation, fresh, reserveErr := tx.reserveInTransaction(tc, req.Reservation, rd)
		if reserveErr != nil {
			return reserveErr
		}
		if !fresh {
			return execution.ErrReservationConflict
		}
		result = execution.EffectContinuation{Request: req, RequestDigest: digest, Reservation: reservation, At: reservation.CreatedAt}
		if result.Validate() != nil {
			return execution.ErrInvalidEffect
		}
		row, encodeErr := record(kindEffectContinuation, req.Key(), digest, req.Original.RunID.String(), req.Original.Scope, result)
		if encodeErr != nil {
			return encodeErr
		}
		if insertErr := tx.driver.Insert(tc, row); insertErr != nil {
			return insertErr
		}
		if !tx.clock.Now().Before(req.Proof.ExpiresAt) || !tx.clock.Now().Before(req.Operation.Deadline) {
			return execution.ErrEffectConflict
		}
		result.Fresh = true
		return tc.Err()
	})
	if err != nil {
		return execution.EffectContinuation{}, err
	}
	if checkErr := ctx.Err(); checkErr != nil {
		return execution.EffectContinuation{}, checkErr
	}
	return result, nil
}

func (s *Repository) checkEffectContinuationPredecessor(ctx context.Context, r execution.EffectContinuationRequest, history []execution.EffectContinuation, now time.Time) error {
	original, err := s.GetEffect(ctx, r.Original.OperationKey)
	if err != nil {
		return err
	}
	prior, receipt, at := original.Request, original.Receipt, r.Seal.At
	retries, compensations := 0, 0
	for _, entry := range history {
		if entry.Request.Action == execution.EffectActionRetry {
			retries++
		} else {
			compensations++
		}
	}
	firstCompensation := r.Action == execution.EffectActionCompensate && compensations == 0
	if len(history) > 0 {
		last := history[len(history)-1]
		result, readErr := s.GetEffectContinuationResult(ctx, last.Request)
		if readErr != nil {
			return readErr
		}
		prior = last.Request.Operation
		receipt = nil
		at = last.At
		if result != nil {
			receipt = &result.Receipt
			at = result.At
		}
	}
	digest, err := execution.EffectPredecessorDigest(prior, receipt)
	if err != nil || digest != r.PredecessorDigest || r.Proof.ObservedAt.Before(at) {
		return execution.ErrEffectConflict
	}
	if r.Action == execution.EffectActionRetry {
		if compensations > 0 || retries+1 >= r.Original.Requirement.Retry.MaxAttempts {
			return execution.ErrBudgetExceeded
		}
	} else if compensations >= r.Operation.Requirement.Retry.MaxAttempts {
		return execution.ErrBudgetExceeded
	}
	if !firstCompensation && r.Operation.Requirement != prior.Requirement {
		return execution.ErrEffectConflict
	}
	if !firstCompensation && r.Operation.ArgsDigest != prior.ArgsDigest {
		return execution.ErrEffectConflict
	}
	switch r.Proof.Mode {
	case execution.ContinuationApplied:
		if !firstCompensation || receipt == nil || receipt.Outcome != execution.EffectApplied {
			return execution.ErrEffectConflict
		}
	case execution.ContinuationAbsence:
		if firstCompensation || receipt == nil || receipt.Outcome != execution.EffectNotApplied {
			return execution.ErrEffectConflict
		}
	case execution.ContinuationIdempotent:
		if firstCompensation || (receipt != nil && receipt.Outcome == execution.EffectApplied) {
			return execution.ErrEffectConflict
		}
	default:
		return execution.ErrInvalidEffect
	}
	// The first compensation starts after an applied result; retry backoff doubles
	// within the pinned cap, with stable bounded jitter. No sleeping consumes an executor slot.
	if !firstCompensation {
		policy := r.Operation.Requirement.Retry
		delay := policy.InitialBackoff
		n := retries
		if r.Action == execution.EffectActionCompensate {
			n = compensations - 1
		}
		for i := 0; i < n; i++ {
			delay = min(delay*2, policy.MaxBackoff)
		}
		jitter := sha256.Sum256([]byte(r.Key()))
		delay = min(delay+(delay/4)*time.Duration(jitter[0])/255, policy.MaxBackoff)
		if now.Before(at.Add(delay)) {
			return execution.ErrEffectConflict
		}
	}
	if r.Action == execution.EffectActionCompensate {
		_, err = s.GetEffect(ctx, r.Operation.OperationKey)
		if !errors.Is(err, execution.ErrEffectNotFound) {
			return execution.ErrEffectConflict
		}
	}
	return nil
}

// GetEffectContinuationResult returns nil for an unresolved retained attempt.
func (s *Repository) GetEffectContinuationResult(ctx context.Context, request execution.EffectContinuationRequest) (*execution.EffectContinuationResult, error) {
	req, _, err := request.Freeze()
	if err != nil {
		return nil, err
	}
	if checkErr := require(ctx, req.Original.Scope); checkErr != nil {
		return nil, checkErr
	}
	a, err := s.effectContinuation(ctx, req)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, execution.ErrEffectNotFound
	}
	row, err := s.get(ctx, Query{Kind: kindEffectContinuationResult, ID: req.Key()})
	if errors.Is(err, ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result execution.EffectContinuationResult
	if row.Deleted || len(row.Body) > 32<<10 || json.Unmarshal([]byte(row.Body), &result) != nil || result.Validate(*a) != nil || row.Digest != evidence.Digest([]byte(row.Body)) || row.RunID != req.Original.RunID.String() {
		return nil, execution.ErrInvalidEffect
	}
	reservation, err := s.GetReservation(ctx, req.Reservation.ID)
	if err != nil {
		return nil, err
	}
	if reservation.Settlement == nil || !reflect.DeepEqual(*reservation.Settlement, result.Settlement) {
		return nil, execution.ErrReservationConflict
	}
	return &result, nil
}

// CompleteEffectContinuation records a new private observation and settles only
// its independent allowance. It never changes the original effect or task result.
func (s *Repository) CompleteEffectContinuation(ctx context.Context, request execution.EffectContinuationRequest, receipt execution.EffectReceipt) (execution.EffectContinuationResult, error) {
	req, digest, err := request.Freeze()
	if err != nil {
		return execution.EffectContinuationResult{}, err
	}
	if receipt.Validate(req.Operation) != nil {
		return execution.EffectContinuationResult{}, execution.ErrInvalidEffect
	}
	if checkErr := require(ctx, req.Original.Scope); checkErr != nil {
		return execution.EffectContinuationResult{}, checkErr
	}
	var result execution.EffectContinuationResult
	err = s.planTransaction(ctx, req.Reservation.PlanID, func(tc context.Context, tx *Repository) error {
		old, readErr := tx.GetEffectContinuationResult(tc, req)
		if readErr != nil {
			return readErr
		}
		if old != nil {
			if old.Receipt != receipt {
				return execution.ErrEffectConflict
			}
			result = *old
			return nil
		}
		a, readErr := tx.effectContinuation(tc, req)
		if readErr != nil {
			return readErr
		}
		if a == nil {
			return execution.ErrEffectNotFound
		}
		settlement := execution.Settlement{ReservationID: req.Reservation.ID, RequestDigest: a.Reservation.RequestDigest, Receipt: receipt.EvidenceRef, Usage: execution.ReservationUsage{Actual: execution.Resources{ToolCalls: 1, FanOut: 1}, Known: execution.KnownAll &^ execution.KnownMoney}}
		_, sd, freezeErr := settlement.Freeze()
		if freezeErr != nil {
			return freezeErr
		}
		settled, settleErr := tx.settleInTransaction(tc, settlement, sd)
		if settleErr != nil {
			return settleErr
		}
		body, encodeErr := json.Marshal(struct {
			RequestDigest string                  `json:"request_digest"`
			Action        execution.EffectAction  `json:"action"`
			Receipt       execution.EffectReceipt `json:"receipt"`
		}{digest, req.Action, receipt})
		if encodeErr != nil {
			return encodeErr
		}
		artifact, sealErr := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: req.ResultID, Scope: req.Original.Scope, MediaType: "application/json", Version: "1", Producer: "cortex.effect-continuation/v1", Sensitivity: evidence.SensitivityRestricted, CreatedAt: settled.At, RetainUntil: settled.At.Add(req.ResultRetention)}, Data: body})
		if sealErr != nil {
			return sealErr
		}
		if saveErr := tx.SaveEvidence(tc, artifact); saveErr != nil {
			return saveErr
		}
		result = execution.EffectContinuationResult{RequestDigest: digest, Receipt: receipt, Settlement: settled, Evidence: artifact.Ref, At: settled.At}
		if result.Validate(*a) != nil {
			return execution.ErrInvalidEffect
		}
		data, encodeErr := json.Marshal(result)
		if encodeErr != nil {
			return encodeErr
		}
		row, encodeErr := record(kindEffectContinuationResult, req.Key(), evidence.Digest(data), req.Original.RunID.String(), req.Original.Scope, result)
		if encodeErr != nil {
			return encodeErr
		}
		return tx.driver.Insert(tc, row)
	})
	if err != nil {
		return execution.EffectContinuationResult{}, err
	}
	return result, ctx.Err()
}

// checkRunEffectContinuations must run under the plan ownership lock. Acceptance
// refuses any follow-up; retirement/replan require every follow-up settled.
func (s *Repository) checkRunEffectContinuations(ctx context.Context, runID id.AgentRunID, accepting bool, observedAt time.Time) error {
	rows, err := s.driver.Select(ctx, Query{Kind: kindEffectContinuation, RunID: runID.String(), Limit: execution.MaxRunRecoveryEntries + 1})
	if err != nil {
		return err
	}
	if len(rows) > execution.MaxRunRecoveryEntries || (accepting && len(rows) > 0) {
		return execution.ErrEffectConflict
	}
	for _, row := range rows {
		if checkErr := validateRecord(ctx, row); checkErr != nil {
			return checkErr
		}
		a, decodeErr := decodeEffectContinuation(row)
		if decodeErr != nil {
			return decodeErr
		}
		result, readErr := s.GetEffectContinuationResult(ctx, a.Request)
		if readErr != nil {
			return readErr
		}
		if result == nil || observedAt.Before(result.At) {
			return execution.ErrEffectUncertain
		}
	}
	return nil
}
