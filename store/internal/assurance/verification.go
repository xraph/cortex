package assurance

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

var _ verification.Store = (*Repository)(nil)

// CreateContract persists a sealed contract with immutable scope and identity.
func (s *Repository) CreateContract(ctx context.Context, c verification.Contract) error {
	if err := require(ctx, c.Scope); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	row, err := record(KindContract, c.ID.String(), c.Digest, "", c.Scope, c)
	if err != nil {
		return err
	}
	if err = s.insert(ctx, row); errors.Is(err, ErrExists) {
		return verification.ErrConflict
	}
	return err
}

func decodeContract(row Record) (verification.Contract, error) {
	if len(row.Body) > 1<<20 {
		return verification.Contract{}, verification.ErrInvalid
	}
	var c verification.Contract
	if err := json.Unmarshal([]byte(row.Body), &c); err != nil {
		return c, err
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	if c.ID.String() != row.ID || c.Digest != row.Digest || c.Scope.Canonical() != row.ScopeCanon {
		return c, verification.ErrInvalid
	}
	return c, nil
}

// GetContract reads a contract only at the exact caller scope.
func (s *Repository) GetContract(ctx context.Context, identifier id.ID) (verification.Contract, error) {
	if identifier.IsNil() || identifier.Prefix() != verification.PrefixContract {
		return verification.Contract{}, verification.ErrInvalid
	}
	row, err := s.get(ctx, Query{Kind: KindContract, ID: identifier.String()})
	if errors.Is(err, ErrAbsent) {
		return verification.Contract{}, verification.ErrNotFound
	}
	if err != nil {
		return verification.Contract{}, err
	}
	return decodeContract(row)
}

// SaveReport validates the persisted contract before accepting an immutable
// report. Contracts cannot change, so this read needs no update transaction.
func (s *Repository) SaveReport(ctx context.Context, r verification.Report) error {
	if err := require(ctx, r.Binding.Scope); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return err
	}
	contractRow, err := s.get(ctx, Query{Kind: KindContract, Digest: r.Binding.ContractDigest})
	if errors.Is(err, ErrAbsent) {
		return verification.ErrNotFound
	}
	if err != nil {
		return err
	}
	c, err := decodeContract(contractRow)
	if err != nil {
		return err
	}
	if checkErr := verification.ValidateReportForContract(c, r); checkErr != nil {
		return checkErr
	}
	if _, loadErr := s.loadReportObservations(ctx, r, 4<<20); loadErr != nil {
		return loadErr
	}
	row, err := record(KindReport, r.ID.String(), r.Digest, r.Binding.RunID.String(), r.Binding.Scope, r)
	if err != nil {
		return err
	}
	if err = s.insert(ctx, row); errors.Is(err, ErrExists) {
		return verification.ErrConflict
	}
	return err
}

func decodeReport(row Record) (verification.Report, error) {
	if len(row.Body) > 1<<20 {
		return verification.Report{}, verification.ErrInvalid
	}
	var r verification.Report
	if err := json.Unmarshal([]byte(row.Body), &r); err != nil {
		return r, err
	}
	if err := r.Validate(); err != nil {
		return r, err
	}
	if r.ID.String() != row.ID || r.Digest != row.Digest || r.Binding.Scope.Canonical() != row.ScopeCanon || r.Binding.RunID.String() != row.RunID {
		return r, verification.ErrInvalid
	}
	return r, nil
}

// GetReport reads a report at the exact caller scope, including historical ones.
func (s *Repository) GetReport(ctx context.Context, identifier id.ID) (verification.Report, error) {
	if identifier.IsNil() || identifier.Prefix() != verification.PrefixReport {
		return verification.Report{}, verification.ErrInvalid
	}
	row, err := s.get(ctx, Query{Kind: KindReport, ID: identifier.String()})
	if errors.Is(err, ErrAbsent) {
		return verification.Report{}, verification.ErrNotFound
	}
	if err != nil {
		return verification.Report{}, err
	}
	return decodeReport(row)
}

// ListReports returns bounded report history for one scoped run.
func (s *Repository) ListReports(ctx context.Context, req verification.ListRequest) ([]verification.Report, error) {
	if req.RunID.IsNil() || req.RunID.Prefix() != id.PrefixAgentRun || req.Limit < 1 || req.Limit > 256 || (!req.After.IsNil() && req.After.Prefix() != verification.PrefixReport) {
		return nil, verification.ErrInvalid
	}
	rows, err := s.driver.Select(ctx, Query{Kind: KindReport, RunID: req.RunID.String(), After: req.After.String(), Limit: req.Limit})
	if err != nil {
		return nil, err
	}
	reports := make([]verification.Report, 0, len(rows))
	for _, row := range rows {
		if checkErr := validateRecord(ctx, row); checkErr != nil {
			return nil, checkErr
		}
		report, decodeErr := decodeReport(row)
		if decodeErr != nil {
			return nil, decodeErr
		}
		reports = append(reports, report)
	}
	return reports, nil
}
