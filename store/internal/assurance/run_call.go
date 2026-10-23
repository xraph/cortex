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
)

const (
	kindRunCall       = "run_call"
	kindRunCallReturn = "run_call_return"
)

var _ execution.RunCallStore = (*Repository)(nil)

type runCallReturn struct {
	Completion execution.RunCallCompletion `json:"completion"`
	At         time.Time                   `json:"at"`
}

// BeginRunCall grants one dispatch only after a confirmed fresh insert. It
// preserves unresolved calls indefinitely rather than treating expiry as absence.
func (s *Repository) BeginRunCall(ctx context.Context, request execution.RunCallRequest) (bool, error) {
	if err := require(ctx, request.Scope); err != nil {
		return false, err
	}
	r, digest, err := request.Freeze()
	if err != nil {
		return false, err
	}
	at := s.clock.Now().UTC()
	if !at.Before(r.Deadline) {
		return false, context.DeadlineExceeded
	}
	value := execution.RunCallRecord{Request: r, StartedAt: at}
	body, err := json.Marshal(value)
	if err != nil {
		return false, err
	}
	row, err := record(kindRunCall, r.Key(), evidence.Digest(body), r.RunID.String(), r.Scope, value)
	if err != nil {
		return false, err
	}
	// The identical replay behavior of s.insert must never grant a fresh call.
	err = s.driver.Insert(ctx, row)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, ErrExists) {
		return false, err
	}
	existing, err := s.GetRunCall(ctx, r.RunID, r.OperationDigest)
	if err != nil {
		return false, err
	}
	_, existingDigest, err := existing.Request.Freeze()
	if err != nil || existingDigest != digest {
		return false, execution.ErrRunCallConflict
	}
	return false, nil
}

// GetRunCall retrieves the original invocation and any immutable local return.
func (s *Repository) GetRunCall(ctx context.Context, runID id.AgentRunID, operationDigest string) (execution.RunCallRecord, error) {
	if (execution.RunCallListRequest{RunID: runID, Limit: 1}).Validate() != nil || !evidence.ValidDigest(operationDigest) {
		return execution.RunCallRecord{}, execution.ErrInvalidRunCall
	}
	row, err := s.get(ctx, Query{Kind: kindRunCall, ID: execution.RunCallKey(runID, operationDigest)})
	if errors.Is(err, ErrAbsent) {
		return execution.RunCallRecord{}, execution.ErrRunCallNotFound
	}
	if err != nil {
		return execution.RunCallRecord{}, err
	}
	value, err := s.readRunCall(ctx, row)
	if err != nil {
		return execution.RunCallRecord{}, err
	}
	if value.Request.RunID != runID || value.Request.OperationDigest != operationDigest {
		return execution.RunCallRecord{}, execution.ErrInvalidRunCall
	}
	return value, nil
}

func (s *Repository) readRunCall(ctx context.Context, row Record) (execution.RunCallRecord, error) {
	if err := validateRecord(ctx, row); err != nil {
		return execution.RunCallRecord{}, err
	}
	var value execution.RunCallRecord
	if row.Deleted || row.Kind != kindRunCall || len(row.Body) > 32<<10 || row.Digest != evidence.Digest([]byte(row.Body)) || json.Unmarshal([]byte(row.Body), &value) != nil || value.Validate() != nil || value.Completion != nil || !value.CompletedAt.IsZero() || row.ID != value.Request.Key() || row.RunID != value.Request.RunID.String() || row.ScopeCanon != value.Request.Scope.Canonical() {
		return execution.RunCallRecord{}, execution.ErrInvalidRunCall
	}
	returned, err := s.get(ctx, Query{Kind: kindRunCallReturn, ID: row.ID})
	if errors.Is(err, ErrAbsent) {
		return value, nil
	}
	if err != nil {
		return execution.RunCallRecord{}, err
	}
	var completion runCallReturn
	if returned.Deleted || returned.Kind != kindRunCallReturn || returned.ID != row.ID || returned.RunID != row.RunID || returned.ScopeCanon != row.ScopeCanon || len(returned.Body) > 16<<10 || returned.Digest != evidence.Digest([]byte(returned.Body)) || json.Unmarshal([]byte(returned.Body), &completion) != nil {
		return execution.RunCallRecord{}, execution.ErrInvalidRunCall
	}
	value.Completion, value.CompletedAt = &completion.Completion, completion.At
	if value.Validate() != nil {
		return execution.RunCallRecord{}, execution.ErrInvalidRunCall
	}
	return value, nil
}

// CompleteRunCall records a normal local return. It neither resolves effects
// nor refunds shared capacity. Conflicting return observations are retained as
// an error rather than silently replacing the first completion.
func (s *Repository) CompleteRunCall(ctx context.Context, request execution.RunCallRequest, completion execution.RunCallCompletion) error {
	if err := require(ctx, request.Scope); err != nil {
		return err
	}
	r, digest, err := request.Freeze()
	if err != nil {
		return err
	}
	if validationErr := completion.Validate(r); validationErr != nil {
		return validationErr
	}
	existing, err := s.GetRunCall(ctx, r.RunID, r.OperationDigest)
	if err != nil {
		return err
	}
	_, existingDigest, err := existing.Request.Freeze()
	if err != nil || existingDigest != digest {
		return execution.ErrRunCallConflict
	}
	if existing.Completion != nil {
		if *existing.Completion != completion {
			return execution.ErrRunCallConflict
		}
		return nil
	}
	value := runCallReturn{Completion: completion, At: s.clock.Now().UTC()}
	if value.At.Before(existing.StartedAt) {
		return execution.ErrInvalidRunCall
	}
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	row, err := record(kindRunCallReturn, r.Key(), evidence.Digest(body), r.RunID.String(), r.Scope, value)
	if err != nil {
		return err
	}
	err = s.driver.Insert(ctx, row)
	if !errors.Is(err, ErrExists) {
		return err
	}
	// Equal concurrent completions have different observation times. Keep the
	// first time and compare the completion itself for idempotent acknowledgment.
	stored, err := s.GetRunCall(ctx, r.RunID, r.OperationDigest)
	if err != nil {
		return err
	}
	if stored.Completion == nil || *stored.Completion != completion {
		return execution.ErrRunCallConflict
	}
	return nil
}

// ListRunCalls returns a bounded exact-scope page, including unresolved calls.
func (s *Repository) ListRunCalls(ctx context.Context, request execution.RunCallListRequest) ([]execution.RunCallRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	if _, err := cortex.RequireScope(ctx); err != nil {
		return nil, err
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindRunCall, RunID: request.RunID.String(), After: request.After, Limit: request.Limit})
	if err != nil {
		return nil, err
	}
	if len(rows) > request.Limit {
		return nil, execution.ErrInvalidRunCall
	}
	result := make([]execution.RunCallRecord, 0, len(rows))
	previous := request.After
	for _, row := range rows {
		if row.ID <= previous || row.RunID != request.RunID.String() {
			return nil, execution.ErrInvalidRunCall
		}
		value, readErr := s.readRunCall(ctx, row)
		if readErr != nil {
			return nil, readErr
		}
		result = append(result, value)
		previous = row.ID
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
