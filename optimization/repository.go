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
	GetReservation(context.Context, id.ID, string) (Reservation, error)
	GetValidationAllocation(context.Context, id.ID) (ValidationAllocation, error)
	GetCandidate(context.Context, id.ID) (Candidate, error)
	GetTrial(context.Context, id.ID) (Trial, error)
	GetEvaluation(context.Context, string) (Evaluation, error)
	GetDecision(context.Context, id.ID) (Decision, error)
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
	Reserve(UsageReservation) (Reservation, error)
	Settle(UsageSettlement) error
	AllocateValidation(ValidationAllocationRequest) (ValidationAllocation, error)
	ExposeValidation(ValidationAccess) error
	ReleaseValidation(ValidationAccess) error
}

// ValidationAllocationRequest freezes one experiment or a parent's paired child
// roster. Cases are taken from the stored specification, never caller hashes.
type ValidationAllocationRequest struct {
	ID           id.ID           `json:"id"`
	ExperimentID id.ID           `json:"experiment_id"`
	PairedRoster []ValidationArm `json:"paired_roster"`
	Lease        WorkLease       `json:"lease"`
}

// ValidationAccess identifies the current worker consuming or releasing holdout.
// Only the owner can release an allocation; an exposure must name a frozen arm.
type ValidationAccess struct {
	AllocationID id.ID     `json:"allocation_id"`
	ExperimentID id.ID     `json:"experiment_id"`
	Lease        WorkLease `json:"lease"`
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
	Decision         *Decision        `json:"decision,omitempty"`
	ExperimentID     id.ID            `json:"experiment_id"`
	ExpectedRevision int64            `json:"expected_revision"`
	NextState        State            `json:"next_state"`
	NextPhase        Phase            `json:"next_phase"`
	Lease            WorkLease        `json:"lease"`
	IdempotencyKey   string           `json:"idempotency_key"`
	Actor            IdentityRef      `json:"actor"`
	Candidates       []Candidate      `json:"candidates,omitempty"`
	Versions         []HarnessVersion `json:"versions,omitempty"`
	Trials           []Trial          `json:"trials,omitempty"`
	TrialUpdates     []TrialUpdate    `json:"trial_updates,omitempty"`
	Evaluations      []Evaluation     `json:"evaluations,omitempty"`
}

// TrialUpdate changes execution state without replacing the frozen trial identity.
// StartAttemptID creates a new attempt; all other writes fence the current one.
type TrialUpdate struct {
	TrialID                 id.ID               `json:"trial_id"`
	ExpectedRevision        int64               `json:"expected_revision"`
	State                   TrialState          `json:"state"`
	StartAttemptID          id.ID               `json:"start_attempt_id,omitempty"`
	AttemptID               id.ID               `json:"attempt_id,omitempty"`
	ExpectedAttemptRevision int64               `json:"expected_attempt_revision"`
	Receipt                 *EnvironmentReceipt `json:"receipt,omitempty"`
	Stop                    *StopReceipt        `json:"stop,omitempty"`
	Execution               *ExecutionRecord    `json:"execution,omitempty"`
	RetryApproval           *ContentRef         `json:"retry_approval,omitempty"`
	Error                   string              `json:"error,omitempty"`
}

// ExperimentBundle atomically creates a draft and its frozen version content.
type ExperimentBundle struct {
	Experiment Experiment       `json:"experiment"`
	Versions   []HarnessVersion `json:"versions"`
}
