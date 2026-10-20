package planning

import (
	"context"
	"errors"

	"github.com/xraph/cortex/id"
)

// ErrNotFound means the plan or revision does not exist in the exact scope.
var ErrNotFound = errors.New("cortex: plan not found")

// ListRequest pages plan identities in lexical ID order. Limit is 1..256.
type ListRequest struct {
	After id.ID
	Limit int
}

// Store is optional immutable plan persistence. Every operation requires exact
// context scope. Commits serialize by plan identity and expected revision;
// identical retries return the accepted revision, even if later revisions exist.
// These methods persist definitions, not task claims or verification decisions.
type Store interface {
	CreatePlan(context.Context, Plan) error
	GetPlan(context.Context, id.ID) (Plan, error)
	GetPlanRevision(context.Context, id.ID, int64) (Plan, error)
	ListPlans(context.Context, ListRequest) ([]Plan, error)
	CommitRevision(context.Context, Revision, int64) (Plan, error)
	ReadTasks(context.Context, id.ID) ([]Task, error)
}
