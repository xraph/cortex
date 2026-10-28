package mongo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/suspension"
)

// SaveProgress comes from the shared assurance repository, which checks the
// run's state and lease owner under TransactRun and then calls
// UpsertProgress below in the same transaction. Mongo gets the same fence
// as the SQL stores.
var _ suspension.ProgressStore = (*Store)(nil)

// progressModel stores the continuation as native BSON, the way
// suspensionModel does, with the exact budget deadline beside it. Prepared
// is BSON binary, so its bytes come back unchanged.
type progressModel struct {
	RunID        string                      `bson:"_id"`
	Owner        string                      `bson:"owner"`
	Continuation suspension.Continuation     `bson:"continuation"`
	Started      []suspension.PendingCall    `bson:"started,omitempty"`
	Done         []suspension.ProgressResult `bson:"done,omitempty"`
	ScopeL0      string                      `bson:"scope_l0"`
	ScopeL1      string                      `bson:"scope_l1"`
	ScopeL2      string                      `bson:"scope_l2"`
	ScopeExtra   map[string]string           `bson:"scope_extra,omitempty"`
	ScopeCanon   string                      `bson:"scope_canon"`
	CreatedAt    time.Time                   `bson:"created_at"`
	UpdatedAt    time.Time                   `bson:"updated_at"`

	BudgetDeadlineExact string `bson:"budget_deadline_exact,omitempty"`
}

// UpsertProgress writes the snapshot inside TransactRun. The filter carries
// the exact scope, so a document stored at another scope is never
// replaced: the upsert collides with it on _id instead.
func (d assuranceDriver) UpsertProgress(ctx context.Context, p *suspension.Progress) error {
	session := mongo.SessionFromContext(ctx)
	if !d.resultTransaction || session == nil || !session.TransactionRunning() {
		return suspension.ErrVersionClaimUnavailable
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	l0, l1, l2, extra := scopeColumns(scope)
	set := bson.M{
		"owner":                 p.Owner,
		"continuation":          p.Cont,
		"started":               p.Started,
		"done":                  p.Done,
		"scope_l0":              l0,
		"scope_l1":              l1,
		"scope_l2":              l2,
		"scope_canon":           scope.Canonical(),
		"updated_at":            p.UpdatedAt,
		"budget_deadline_exact": exactBudgetDeadline(p.Cont.Config.Budget.Deadline),
	}
	if len(extra) > 0 {
		set["scope_extra"] = extra
	}
	filter := scopeFilter(scope, true)
	filter["_id"] = p.RunID.String()
	update := bson.M{"$set": set, "$setOnInsert": bson.M{"created_at": p.UpdatedAt}}
	result, err := d.store.mdb.Collection(colRunProgress).UpdateOne(ctx, filter, update, options.UpdateOne().SetUpsert(true))
	if err != nil {
		if isUniqueViolation(err) {
			return cortex.ErrRunLeaseLost
		}
		return fmt.Errorf("cortex/mongo: save run progress: %w", err)
	}
	if result.MatchedCount+result.UpsertedCount != 1 {
		return cortex.ErrRunLeaseLost
	}
	return nil
}

// GetProgress reads a run's snapshot at the exact context scope.
func (s *Store) GetProgress(ctx context.Context, runID id.AgentRunID) (*suspension.Progress, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	filter := scopeFilter(scope, true)
	filter["_id"] = runID.String()
	var m progressModel
	if err = s.mdb.Collection(colRunProgress).FindOne(ctx, filter).Decode(&m); err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrProgressNotFound
		}
		return nil, fmt.Errorf("cortex/mongo: get run progress: %w", err)
	}
	return progressFromModel(&m)
}

// DeleteProgress removes a run's snapshot at the exact context scope. A
// run with none is not an error.
func (s *Store) DeleteProgress(ctx context.Context, runID id.AgentRunID) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	filter := scopeFilter(scope, true)
	filter["_id"] = runID.String()
	if _, err = s.mdb.Collection(colRunProgress).DeleteOne(ctx, filter); err != nil {
		return fmt.Errorf("cortex/mongo: delete run progress: %w", err)
	}
	return nil
}

func progressFromModel(m *progressModel) (*suspension.Progress, error) {
	runID, err := id.ParseAgentRunID(m.RunID)
	if err != nil {
		return nil, err
	}
	scope, err := cortex.DecodeStoredScope(m.ScopeCanon, [3]string{m.ScopeL0, m.ScopeL1, m.ScopeL2}, len(m.ScopeExtra) > 0)
	if err != nil {
		return nil, fmt.Errorf("run progress %s: %w", runID, err)
	}
	cont := m.Continuation
	budget, err := restoreBudgetDeadline(cont.Config.Budget, m.BudgetDeadlineExact)
	if err != nil {
		return nil, err
	}
	cont.Config.Budget = budget
	return &suspension.Progress{
		RunID:     runID,
		Scope:     scope,
		Owner:     m.Owner,
		Cont:      cont,
		Started:   m.Started,
		Done:      m.Done,
		UpdatedAt: m.UpdatedAt.UTC(),
	}, nil
}
