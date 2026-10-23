package assurance

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
)

const (
	kindEffectRequest = "effect_request"
	kindEffectReceipt = "effect_receipt"
)

var _ execution.EffectStore = (*Repository)(nil)
var _ execution.EffectDiscoveryStore = (*Repository)(nil)

// BeginEffect grants dispatch only after a confirmed fresh insert. An identical
// existing row never grants permission, even after a lost insert acknowledgement.
func (s *Repository) BeginEffect(ctx context.Context, request execution.EffectRequest) (bool, error) {
	if err := require(ctx, request.Scope); err != nil {
		return false, err
	}
	r, digest, err := request.Freeze()
	if err != nil {
		return false, err
	}
	row, err := record(kindEffectRequest, evidence.Digest([]byte(r.OperationKey)), digest, r.RunID.String(), r.Scope, r)
	if err != nil {
		return false, err
	}
	// Deliberately bypass s.insert: its identical-replay success must not turn
	// a retained operation into permission to repeat an external mutation.
	err = s.driver.Insert(ctx, row)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, ErrExists) {
		return false, err
	}
	existing, readErr := s.GetEffect(ctx, r.OperationKey)
	if readErr != nil {
		return false, readErr
	}
	_, storedDigest, checkErr := existing.Request.Freeze()
	if checkErr != nil || storedDigest != digest {
		return false, execution.ErrEffectConflict
	}
	return false, nil
}

// GetEffect reads the retained request and terminal receipt at exact scope.
func (s *Repository) GetEffect(ctx context.Context, key string) (execution.EffectRecord, error) {
	if key == "" || len(key) > 256 {
		return execution.EffectRecord{}, execution.ErrInvalidEffect
	}
	identifier := evidence.Digest([]byte(key))
	row, err := s.get(ctx, Query{Kind: kindEffectRequest, ID: identifier})
	if errors.Is(err, ErrAbsent) {
		return execution.EffectRecord{}, execution.ErrEffectNotFound
	}
	if err != nil {
		return execution.EffectRecord{}, err
	}
	result, err := s.effectRecord(ctx, row)
	if err != nil {
		return execution.EffectRecord{}, err
	}
	if result.Request.OperationKey != key {
		return execution.EffectRecord{}, execution.ErrInvalidEffect
	}
	return result, nil
}

// ListEffects returns a bounded page including both pending and terminal records.
// Repeated scans are required: concurrent inserts can sort before the cursor.
func (s *Repository) ListEffects(ctx context.Context, req execution.EffectListRequest) ([]execution.EffectRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if _, err := cortex.RequireScope(ctx); err != nil {
		return nil, err
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindEffectRequest, RunID: req.RunID.String(), After: req.After, Limit: req.Limit})
	if err != nil {
		return nil, err
	}
	if len(rows) > req.Limit {
		return nil, execution.ErrInvalidEffect
	}
	result := make([]execution.EffectRecord, 0, len(rows))
	previous := req.After
	for _, row := range rows {
		if row.ID <= previous || (!req.RunID.IsNil() && row.RunID != req.RunID.String()) {
			return nil, execution.ErrInvalidEffect
		}
		item, readErr := s.effectRecord(ctx, row)
		if readErr != nil {
			return nil, readErr
		}
		result = append(result, item)
		previous = row.ID
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Repository) effectRecord(ctx context.Context, row Record) (execution.EffectRecord, error) {
	if err := validateRecord(ctx, row); err != nil {
		return execution.EffectRecord{}, err
	}
	var request execution.EffectRequest
	if row.Deleted || len(row.Body) > 16<<10 || json.Unmarshal([]byte(row.Body), &request) != nil {
		return execution.EffectRecord{}, execution.ErrInvalidEffect
	}
	r, digest, err := request.Freeze()
	identifier := evidence.Digest([]byte(r.OperationKey))
	if err != nil || row.Kind != kindEffectRequest || row.ID != identifier || row.Digest != digest || row.RunID != r.RunID.String() || row.ScopeCanon != r.Scope.Canonical() {
		return execution.EffectRecord{}, execution.ErrInvalidEffect
	}
	result := execution.EffectRecord{Request: r}
	terminal, err := s.get(ctx, Query{Kind: kindEffectReceipt, ID: identifier})
	if errors.Is(err, ErrAbsent) {
		return result, nil
	}
	if err != nil {
		return execution.EffectRecord{}, err
	}
	var receipt execution.EffectReceipt
	if terminal.Deleted || len(terminal.Body) > 16<<10 || json.Unmarshal([]byte(terminal.Body), &receipt) != nil || receipt.Validate(r) != nil || terminal.Kind != kindEffectReceipt || terminal.ID != identifier || terminal.Digest != evidence.Digest([]byte(terminal.Body)) || terminal.RunID != r.RunID.String() || terminal.ScopeCanon != r.Scope.Canonical() {
		return execution.EffectRecord{}, execution.ErrInvalidEffect
	}
	result.Receipt = &receipt
	return result, nil
}

// CompleteEffect appends terminal knowledge without replacing either history
// row. Conflicting external outcomes require separate host adjudication.
func (s *Repository) CompleteEffect(ctx context.Context, request execution.EffectRequest, receipt execution.EffectReceipt) error {
	if err := require(ctx, request.Scope); err != nil {
		return err
	}
	r, digest, err := request.Freeze()
	if err != nil {
		return err
	}
	if checkErr := receipt.Validate(r); checkErr != nil {
		return checkErr
	}
	existing, err := s.GetEffect(ctx, r.OperationKey)
	if err != nil {
		return err
	}
	_, storedDigest, err := existing.Request.Freeze()
	if err != nil || storedDigest != digest {
		return execution.ErrEffectConflict
	}
	body, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	row, err := record(kindEffectReceipt, evidence.Digest([]byte(r.OperationKey)), evidence.Digest(body), r.RunID.String(), r.Scope, receipt)
	if err != nil {
		return err
	}
	if err = s.insert(ctx, row); errors.Is(err, ErrExists) {
		return execution.ErrEffectConflict
	}
	return err
}
