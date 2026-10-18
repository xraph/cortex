package optimization

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
)

// Repository owns exactly scoped persistence. Transaction implementations must
// publish all changes together and return detached resource values.
type Repository interface {
	Transact(context.Context, func(Transaction) error) error
	GetExperiment(context.Context, id.ID) (Experiment, error)
	ListExperiments(context.Context, ListQuery) (Page[Experiment], error)
	ListEvents(context.Context, id.ID, int64, int) (Page[Event], error)
	GetVersion(context.Context, id.ID) (HarnessVersion, error)
	Migrate(context.Context) error
	Ping(context.Context) error
	Close() error
}

// Transaction is valid only during its synchronous callback. Callers must not
// retain it or use it from concurrent goroutines.
type Transaction interface {
	CreateExperiment(ExperimentBundle) error
	CommitTransition(Transition) error
	ClaimWork(ClaimRequest) (WorkLease, error)
	RenewLease(WorkLease) error
}

// ClaimRequest names one queued or expired experiment and the claiming worker.
type ClaimRequest struct {
	ExperimentID id.ID         `json:"experiment_id"`
	Owner        string        `json:"owner"`
	Duration     time.Duration `json:"duration"`
}

// Transition binds a requested state change to its prior revision and actor.
// Idempotency keys can be reused only with identical request content.
type Transition struct {
	ExperimentID     id.ID       `json:"experiment_id"`
	ExpectedRevision int64       `json:"expected_revision"`
	NextState        State       `json:"next_state"`
	NextPhase        Phase       `json:"next_phase"`
	Lease            WorkLease   `json:"lease"`
	IdempotencyKey   string      `json:"idempotency_key"`
	Actor            IdentityRef `json:"actor"`
}

// ExperimentBundle atomically creates a draft and its frozen version content.
type ExperimentBundle struct {
	Experiment Experiment       `json:"experiment"`
	Versions   []HarnessVersion `json:"versions"`
}
