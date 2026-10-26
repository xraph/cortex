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
	GetDiscoveryLessons(context.Context, id.ID) (DiscoveryLessons, error)
	ResolveDiscoveryLessons(context.Context, id.ID, DiscoveryRef) (DiscoveryLessons, error)
	GetArtifact(context.Context, ArtifactRef) (ArtifactRetention, error)
	ListArtifactWork(context.Context, time.Time, PageQuery) (Page[ArtifactRetention], error)
	ListExperiments(context.Context, ListQuery) (Page[Experiment], error)
	ListEvents(context.Context, id.ID, int64, int) (Page[Event], error)
	GetVersion(context.Context, id.ID) (HarnessVersion, error)
	GetReservation(context.Context, id.ID, string) (Reservation, error)
	GetValidationAllocation(context.Context, id.ID) (ValidationAllocation, error)
	GetCandidate(context.Context, id.ID) (Candidate, error)
	GetTrial(context.Context, id.ID) (Trial, error)
	GetEvaluation(context.Context, string) (Evaluation, error)
	GetDecision(context.Context, id.ID) (Decision, error)
	GetActivation(context.Context, id.AgentID, string) (Activation, error)
	GetActivationForKind(context.Context, id.AgentID, string, HarnessKind) (Activation, error)
	ResolveActivation(context.Context, id.ID) (Activation, error)
	GetActivationGeneration(context.Context, id.ID) (Activation, error)
	GetLesson(context.Context, id.ID) (Lesson, error)
	ResolveLesson(context.Context, LessonRef) (Lesson, error)
	GetPublication(context.Context, id.ID, int64) (Publication, error)
	ListVersions(context.Context, VersionQuery) (Page[HarnessVersion], error)
	ListCandidates(context.Context, id.ID, PageQuery) (Page[Candidate], error)
	ListTrials(context.Context, id.ID, TrialQuery) (Page[Trial], error)
	ListEvaluations(context.Context, id.ID, PageQuery) (Page[Evaluation], error)
	ListDecisions(context.Context, id.ID, PageQuery) (Page[Decision], error)
	ListReservations(context.Context, id.ID, PageQuery) (Page[Reservation], error)
	ListValidationAllocations(context.Context, id.ID, PageQuery) (Page[ValidationAllocation], error)
	ListLessons(context.Context, LessonQuery) (Page[Lesson], error)
	ListPublications(context.Context, PublicationQuery) (Page[Publication], error)
	ListActivationHistory(context.Context, id.AgentID, string, PageQuery) (Page[Activation], error)
	ListActivationHistoryForKind(context.Context, id.AgentID, string, HarnessKind, PageQuery) (Page[Activation], error)
	Migrate(context.Context) error
	Ping(context.Context) error
	Close() error
}

// Transaction is valid only during its synchronous callback. Callers must not
// retain it or use it from concurrent goroutines.
type Transaction interface {
	RegisterArtifact(ArtifactRegistration) error
	BeginArtifactDeletion(ArtifactDeletion) error
	CompleteArtifactDeletion(ArtifactRef, string) error
	CreateExperiment(ExperimentBundle) error
	PutDiscoveryLessons(DiscoveryLessonsRequest) (DiscoveryLessons, error)
	CommitTransition(Transition) error
	ClaimWork(ClaimRequest) (WorkLease, error)
	RenewLease(WorkLease) error
	Reserve(UsageReservation) (Reservation, error)
	BeginUsageCall(UsageReservation, string) (Reservation, error)
	Settle(UsageSettlement) error
	ReconcileUsage(UsageReconciliation) error
	AllocateValidation(ValidationAllocationRequest) (ValidationAllocation, error)
	ExposeValidation(ValidationAccess) error
	ReleaseValidation(ValidationAccess) error
	Deactivate(DeactivationChange) (Activation, error)
	CompareAndSwapActivation(ActivationChange) (Activation, error)
	PutPublication(PublicationRequest) error
	ClaimPublication(PublicationClaim) (Publication, error)
	RetryPublication(PublicationRetry) error
	CompletePublication(PublicationLease, PublicationReceipt) error
	FailPublication(PublicationLease, string, time.Time, bool) error
	RevokeLesson(LessonRevocation) error
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
	Retain *RetainDecision `json:"retain,omitempty"`
	// CommandAction binds operator retries to their lifecycle intent. Omitted
	// values retain the digest of existing host and worker transitions.
	CommandAction    Action               `json:"command_action,omitempty"`
	Decision         *Decision            `json:"decision,omitempty"`
	ExperimentID     id.ID                `json:"experiment_id"`
	ExpectedRevision int64                `json:"expected_revision"`
	NextState        State                `json:"next_state"`
	NextPhase        Phase                `json:"next_phase"`
	Lease            WorkLease            `json:"lease"`
	IdempotencyKey   string               `json:"idempotency_key"`
	Actor            IdentityRef          `json:"actor"`
	Candidates       []Candidate          `json:"candidates,omitempty"`
	Versions         []HarnessVersion     `json:"versions,omitempty"`
	Trials           []Trial              `json:"trials,omitempty"`
	TrialUpdates     []TrialUpdate        `json:"trial_updates,omitempty"`
	Evaluations      []Evaluation         `json:"evaluations,omitempty"`
	Publications     []PublicationRequest `json:"publications,omitempty"`
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
	ParentLease     *WorkLease       `json:"parent_lease,omitempty"`
	ExpectedLessons []LessonRef      `json:"expected_lessons,omitempty"`
	Experiment      Experiment       `json:"experiment"`
	Versions        []HarnessVersion `json:"versions"`
}
