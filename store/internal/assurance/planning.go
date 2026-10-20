package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

const (
	kindPlan         = "plan"
	kindPlanRevision = "plan_revision"
)

var _ planning.Store = (*Repository)(nil)

// CreatePlan persists revision one. Identical retries preserve its identity.
func (s *Repository) CreatePlan(ctx context.Context, p planning.Plan) error {
	if err := require(ctx, p.Scope); err != nil {
		return err
	}
	if p.Revision != 1 {
		return planning.ErrInvalid
	}
	if err := planning.Validate(p, planning.Limits{}); err != nil {
		return err
	}
	row, err := planRecord(p)
	if err != nil {
		return err
	}
	if err = s.insert(ctx, row); errors.Is(err, ErrExists) {
		return planning.ErrConflict
	}
	return err
}

func planRecord(p planning.Plan) (Record, error) {
	kind, key := kindPlan, p.ID.String()
	if p.Revision > 1 {
		kind = kindPlanRevision
		key = planRevisionKey(p.ID, p.Revision)
	}
	return record(kind, key, p.Digest, p.ID.String(), p.Scope, p)
}

func planRevisionKey(identifier id.ID, revision int64) string {
	return fmt.Sprintf("%s:%020d", identifier.String(), revision)
}

func decodePlan(row Record) (planning.Plan, error) {
	if row.Deleted || len(row.Body) > 1<<20 {
		return planning.Plan{}, planning.ErrInvalid
	}
	var p planning.Plan
	if err := json.Unmarshal([]byte(row.Body), &p); err != nil {
		return p, err
	}
	if err := planning.Validate(p, planning.Limits{}); err != nil {
		return p, err
	}
	expectedKind, key := kindPlan, p.ID.String()
	if p.Revision > 1 {
		expectedKind = kindPlanRevision
		key = planRevisionKey(p.ID, p.Revision)
	}
	if row.Kind != expectedKind || row.ID != key || row.RunID != p.ID.String() || row.Digest != p.Digest || row.ScopeCanon != p.Scope.Canonical() {
		return p, planning.ErrInvalid
	}
	return p, nil
}

// GetPlanRevision reads one immutable revision at the exact caller scope.
func (s *Repository) GetPlanRevision(ctx context.Context, identifier id.ID, revision int64) (planning.Plan, error) {
	if identifier.IsNil() || identifier.Prefix() != planning.PrefixPlan || revision < 1 {
		return planning.Plan{}, planning.ErrInvalid
	}
	query := Query{Kind: kindPlan, ID: identifier.String()}
	if revision > 1 {
		query.Kind = kindPlanRevision
		query.ID = planRevisionKey(identifier, revision)
	}
	row, err := s.get(ctx, query)
	if errors.Is(err, ErrAbsent) {
		return planning.Plan{}, planning.ErrNotFound
	}
	if err != nil {
		return planning.Plan{}, err
	}
	return decodePlan(row)
}

// GetPlan reads the highest committed revision, including revision one.
func (s *Repository) GetPlan(ctx context.Context, identifier id.ID) (planning.Plan, error) {
	initial, err := s.GetPlanRevision(ctx, identifier, 1)
	if err != nil {
		return planning.Plan{}, err
	}
	row, err := s.get(ctx, Query{Kind: kindPlanRevision, RunID: identifier.String(), Reverse: true})
	if errors.Is(err, ErrAbsent) {
		return initial, nil
	}
	if err != nil {
		return planning.Plan{}, err
	}
	latest, err := decodePlan(row)
	if err != nil {
		return planning.Plan{}, err
	}
	if latest.ID != initial.ID || latest.Contract.Digest != initial.Contract.Digest {
		return planning.Plan{}, planning.ErrInvalid
	}
	return latest, nil
}

// CommitRevision appends a validated successor. The existing unique key on
// (kind, scope, ID) serializes competing proposals for the same successor.
// There is no mutable head or second write to lose after a process crash.
func (s *Repository) CommitRevision(ctx context.Context, proposal planning.Revision, expected int64) (planning.Plan, error) {
	if err := require(ctx, proposal.Plan.Scope); err != nil {
		return planning.Plan{}, err
	}
	if expected < 1 {
		return planning.Plan{}, planning.ErrConflict
	}
	previous, err := s.GetPlanRevision(ctx, proposal.Plan.ID, expected)
	if errors.Is(err, planning.ErrNotFound) {
		return planning.Plan{}, planning.ErrConflict
	}
	if err != nil {
		return planning.Plan{}, err
	}
	next, err := planning.ApplyRevision(previous, proposal, expected, planning.Limits{})
	if err != nil {
		return planning.Plan{}, err
	}
	row, err := planRecord(next)
	if err != nil {
		return planning.Plan{}, err
	}
	if err = s.insert(ctx, row); errors.Is(err, ErrExists) {
		return planning.Plan{}, planning.ErrConflict
	}
	if err != nil {
		return planning.Plan{}, err
	}
	return next, nil
}

// ListPlans pages stable plan identities and returns each current revision.
// Concurrent commits may advance individual plans during this non-snapshot list.
func (s *Repository) ListPlans(ctx context.Context, req planning.ListRequest) ([]planning.Plan, error) {
	if req.Limit < 1 || req.Limit > 256 || !req.After.IsNil() && req.After.Prefix() != planning.PrefixPlan {
		return nil, planning.ErrInvalid
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindPlan, After: req.After.String(), Limit: req.Limit})
	if err != nil {
		return nil, err
	}
	plans := make([]planning.Plan, 0, len(rows))
	for _, row := range rows {
		if checkErr := validateRecord(ctx, row); checkErr != nil {
			return nil, checkErr
		}
		initial, decodeErr := decodePlan(row)
		if decodeErr != nil {
			return nil, decodeErr
		}
		current, readErr := s.GetPlan(ctx, initial.ID)
		if readErr != nil {
			return nil, readErr
		}
		plans = append(plans, current)
	}
	return plans, nil
}

// ReadTasks returns current definitions, detached from persisted records.
func (s *Repository) ReadTasks(ctx context.Context, identifier id.ID) ([]planning.Task, error) {
	p, err := s.GetPlan(ctx, identifier)
	if err != nil {
		return nil, err
	}
	return p.Tasks, nil
}
