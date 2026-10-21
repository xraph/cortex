package optimization

import (
	"encoding/json"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Harness groups immutable versions for one agent within an exact scope.
type Harness struct {
	ID        id.ID        `json:"id"`
	Scope     cortex.Scope `json:"scope"`
	AgentID   id.AgentID   `json:"agent_id"`
	Name      string       `json:"name"`
	Kind      HarnessKind  `json:"kind"`
	Revision  int64        `json:"revision"`
	CreatedAt time.Time    `json:"created_at"`
	CreatedBy IdentityRef  `json:"created_by"`
}

// State records experiment lifecycle independently of the active phase.
type State string

const (
	StateDraft           State = "draft"
	StateQueued          State = "queued"
	StateRunning         State = "running"
	StatePaused          State = "paused"
	StateReviewRequired  State = "review_required"
	StateCompleted       State = "completed"
	StateCancelled       State = "cancelled"
	StateFailed          State = "failed"
	StateBudgetExhausted State = "budget_exhausted"
)

// Phase records the durable point from which an experiment resumes.
type Phase string

const (
	PhaseBaseline Phase = "baseline"
	PhasePropose  Phase = "propose"
	PhaseProbe    Phase = "probe"
	PhaseCompose  Phase = "compose"
	PhaseValidate Phase = "validate"
	PhaseDecide   Phase = "decide"
	PhaseReflect  Phase = "reflect"
)

// Experiment combines a frozen specification with revision-fenced work state.
type Experiment struct {
	DecisionID     id.ID          `json:"decision_id,omitempty"`
	ID             id.ID          `json:"id"`
	Spec           ExperimentSpec `json:"spec"`
	SpecDigest     string         `json:"spec_digest"`
	EvaluationPlan EvaluationPlan `json:"evaluation_plan"`
	State          State          `json:"state"`
	Phase          Phase          `json:"phase"`
	Round          int            `json:"round"`
	Revision       int64          `json:"revision"`
	Lease          WorkLease      `json:"lease"`
	Plan           TrialPlan      `json:"plan"`
	LessonIDs      []id.ID        `json:"lesson_ids"`
	FrozenLessons  []LessonRef    `json:"frozen_lessons"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	Deadline       time.Time      `json:"deadline"`
	Failure        string         `json:"failure,omitempty"`
}

// WorkLease fences persisted writes from replaced workers.
type WorkLease struct {
	ExperimentID id.ID     `json:"experiment_id"`
	Owner        string    `json:"owner"`
	Generation   int64     `json:"generation"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// CandidateOrigin distinguishes proposed instructions from executed composition.
type CandidateOrigin string

const (
	OriginGeneral  CandidateOrigin = "general"
	OriginDomain   CandidateOrigin = "domain"
	OriginComposed CandidateOrigin = "composed"
)

// Candidate retains rejected proposals as well as executable frozen versions.
type Candidate struct {
	ID               id.ID           `json:"id"`
	ExperimentID     id.ID           `json:"experiment_id"`
	Parents          []id.ID         `json:"parents"`
	Origin           CandidateOrigin `json:"origin"`
	Round            int             `json:"round"`
	Proposal         Proposal        `json:"proposal"`
	PredictedEffects []string        `json:"predicted_effects"`
	Version          VersionRef      `json:"version"`
	Accepted         bool            `json:"accepted"`
	Rejection        string          `json:"rejection,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
}

// TrialState distinguishes an unknown external effect from a confirmed failure.
type TrialState string

const (
	TrialQueued      TrialState = "queued"
	TrialRunning     TrialState = "running"
	TrialCompleted   TrialState = "completed"
	TrialFailed      TrialState = "failed"
	TrialCancelled   TrialState = "cancelled"
	TrialBlocked     TrialState = "blocked"
	TrialUnknown     TrialState = "unknown"
	TrialInvalidated TrialState = "invalidated"
)

// Trial identifies one arm, case and repetition. A nil candidate is the baseline.
type Trial struct {
	ID           id.ID            `json:"id"`
	ExperimentID id.ID            `json:"experiment_id"`
	CandidateID  id.ID            `json:"candidate_id,omitempty"`
	AllocationID id.ID            `json:"allocation_id,omitempty"`
	Split        Split            `json:"split"`
	Version      VersionRef       `json:"version"`
	CaseID       string           `json:"case_id"`
	CaseDigest   string           `json:"case_digest"`
	Repetition   int              `json:"repetition"`
	State        TrialState       `json:"state"`
	Revision     int64            `json:"revision"`
	Attempts     []Attempt        `json:"attempts"`
	Execution    *ExecutionRecord `json:"execution,omitempty"`
	EvaluationID string           `json:"evaluation_id,omitempty"`
	Error        string           `json:"error,omitempty"`
	CreatedAt    time.Time        `json:"created_at"`
}

// Attempt preserves every external dispatch and its durable environment receipt.
type Attempt struct {
	ID            id.ID               `json:"id"`
	State         TrialState          `json:"state"`
	Revision      int64               `json:"revision"`
	Receipt       *EnvironmentReceipt `json:"receipt,omitempty"`
	Fence         Fence               `json:"fence"`
	Stop          *StopReceipt        `json:"stop,omitempty"`
	Replaces      id.ID               `json:"replaces,omitempty"`
	RetryApproval *ContentRef         `json:"retry_approval,omitempty"`
	Execution     *ExecutionRecord    `json:"execution,omitempty"`
	StartedAt     time.Time           `json:"started_at"`
	CompletedAt   time.Time           `json:"completed_at"`
	Usage         Usage               `json:"usage"`
	Error         string              `json:"error,omitempty"`
}

// DecisionOutcome is a recommendation, never an implicit activation command.
type DecisionOutcome string

const (
	DecisionRecommend    DecisionOutcome = "recommend"
	DecisionRetain       DecisionOutcome = "retain"
	DecisionInconclusive DecisionOutcome = "inconclusive"
	DecisionReject       DecisionOutcome = "reject"
)

// Decision binds selection policy to the exact compared evidence.
type Decision struct {
	AllocationID  id.ID           `json:"allocation_id"`
	SpecDigest    string          `json:"spec_digest"`
	ID            id.ID           `json:"id"`
	ExperimentID  id.ID           `json:"experiment_id"`
	Baseline      VersionRef      `json:"baseline"`
	Candidate     VersionRef      `json:"candidate"`
	SelectionKind string          `json:"selection_kind"`
	Outcome       DecisionOutcome `json:"outcome"`
	EvidenceClass EvidenceClass   `json:"evidence_class"`
	EvaluationIDs []string        `json:"evaluation_ids"`
	PolicyDigest  string          `json:"policy_digest"`
	Comparison    Comparison      `json:"comparison"`
	Reasons       []string        `json:"reasons"`
	CreatedAt     time.Time       `json:"created_at"`
}

// Activation is an append-only generation for an exact scope, agent and channel.
type Activation struct {
	RollbackTo         id.ID        `json:"rollback_to,omitempty"`
	ID                 id.ID        `json:"id"`
	Scope              cortex.Scope `json:"scope"`
	AgentID            id.AgentID   `json:"agent_id"`
	Channel            string       `json:"channel"`
	Version            VersionRef   `json:"version"`
	Generation         int64        `json:"generation"`
	ExpectedGeneration int64        `json:"expected_generation"`
	Actor              IdentityRef  `json:"actor"`
	PolicyDigest       string       `json:"policy_digest"`
	DecisionID         id.ID        `json:"decision_id"`
	CreatedAt          time.Time    `json:"created_at"`
}

// ValidationAllocation permanently tracks holdout exposure per scope and agent.
type ValidationAllocation struct {
	CreatedAt    time.Time       `json:"created_at"`
	ID           id.ID           `json:"id"`
	Scope        cortex.Scope    `json:"scope"`
	AgentID      id.AgentID      `json:"agent_id"`
	ExperimentID id.ID           `json:"experiment_id"`
	CaseDigests  []string        `json:"case_digests"`
	InputDigests []string        `json:"input_digests"`
	DatasetItems []ContentRef    `json:"dataset_items"`
	PairedRoster []ValidationArm `json:"paired_roster"`
	ExposedAt    time.Time       `json:"exposed_at"`
	ReleasedAt   time.Time       `json:"released_at"`
	Revision     int64           `json:"revision"`
}

// ValidationArm pins the only child allowed to use an arm of a paired allocation.
type ValidationArm struct {
	Controller   VersionRef `json:"controller"`
	ExperimentID id.ID      `json:"experiment_id"`
	Baseline     VersionRef `json:"baseline"`
	Finalist     VersionRef `json:"finalist"`
}

// Event is redacted durable history with a monotonic experiment sequence.
type Event struct {
	ID           id.ID           `json:"id"`
	ExperimentID id.ID           `json:"experiment_id"`
	Sequence     int64           `json:"sequence"`
	Type         string          `json:"type"`
	Actor        IdentityRef     `json:"actor"`
	Resources    []id.ID         `json:"resources"`
	Payload      json.RawMessage `json:"payload"`
	CreatedAt    time.Time       `json:"created_at"`
}

// ListQuery requests a bounded page within the context's exact scope.
type ListQuery struct {
	Limit   int        `json:"limit"`
	Cursor  string     `json:"cursor,omitempty"`
	AgentID id.AgentID `json:"agent_id,omitempty"`
	State   State      `json:"state,omitempty"`
}

// Page carries stable cursor pagination without exposing backend offsets.
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}
