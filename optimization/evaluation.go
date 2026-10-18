package optimization

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

// Evaluator scores captured output and owns paired statistical calculations.
type Evaluator interface {
	Freeze(context.Context, EvaluationSpec) (EvaluationPlan, error)
	Evaluate(context.Context, EvaluationRequest) (Evaluation, error)
	Compare(context.Context, ComparisonRequest) (Comparison, error)
}

// EvaluationSpec names a host-approved source and exact frozen task identities.
type EvaluationSpec struct {
	Source           ContentRef `json:"source"`
	Tasks            []TaskCase `json:"tasks"`
	RequiredCriteria []string   `json:"required_criteria"`
}

// EvaluationPlan stores an immutable evaluator reference and scorer revisions.
// Private rubric bytes remain in evaluator storage.
type EvaluationPlan struct {
	Ref         ContentRef        `json:"ref"`
	CaseDigests map[string]string `json:"case_digests"`
	Scorers     []run.Revision    `json:"scorers"`
	FrozenAt    time.Time         `json:"frozen_at"`
}

// EvaluationRequest contains recorded output, never a callback to execute a task.
type EvaluationRequest struct {
	ExperimentID   id.ID           `json:"experiment_id"`
	TrialID        id.ID           `json:"trial_id"`
	CandidateID    id.ID           `json:"candidate_id,omitempty"`
	CaseID         string          `json:"case_id"`
	CaseDigest     string          `json:"case_digest"`
	Repetition     int             `json:"repetition"`
	Plan           EvaluationPlan  `json:"plan"`
	Execution      ExecutionRecord `json:"execution"`
	IdempotencyKey string          `json:"idempotency_key"`
}

// CriterionValue keeps scorer failure separate from a valid zero score.
type CriterionValue struct {
	ID       string   `json:"id"`
	Score    *float64 `json:"score,omitempty"`
	Required bool     `json:"required"`
	Passed   bool     `json:"passed"`
	Error    string   `json:"error,omitempty"`
}

// Evaluation identifies the durable external job, distinct from its report ID.
type Evaluation struct {
	ID              string           `json:"id"`
	ReportID        string           `json:"report_id"`
	ExperimentID    id.ID            `json:"experiment_id"`
	TrialID         id.ID            `json:"trial_id"`
	CaseID          string           `json:"case_id"`
	CaseDigest      string           `json:"case_digest"`
	Repetition      int              `json:"repetition"`
	PlanDigest      string           `json:"plan_digest"`
	ExecutionDigest string           `json:"execution_digest"`
	Scorers         []run.Revision   `json:"scorers"`
	Criteria        []CriterionValue `json:"criteria"`
	Complete        bool             `json:"complete"`
	Errors          []string         `json:"errors"`
	JudgeUsage      Usage            `json:"judge_usage"`
	StartedAt       time.Time        `json:"started_at"`
	CompletedAt     time.Time        `json:"completed_at"`
}

// ComparisonRequest pairs exact evidence IDs under the frozen objective.
type ComparisonRequest struct {
	Plan         EvaluationPlan `json:"plan"`
	BaselineIDs  []string       `json:"baseline_ids"`
	CandidateIDs []string       `json:"candidate_ids"`
	Objective    Objective      `json:"objective"`
}

// CaseDelta averages repetitions within a case before statistical resampling.
type CaseDelta struct {
	CaseID      string             `json:"case_id"`
	CaseDigest  string             `json:"case_digest"`
	Repetitions int                `json:"repetitions"`
	Delta       float64            `json:"delta"`
	Criteria    map[string]float64 `json:"criteria"`
}

// Comparison reports evidence completeness separately from selection policy.
type Comparison struct {
	BaselineIDs          []string           `json:"baseline_ids"`
	CandidateIDs         []string           `json:"candidate_ids"`
	PlanDigest           string             `json:"plan_digest"`
	Cases                []CaseDelta        `json:"cases"`
	MeanDelta            float64            `json:"mean_delta"`
	LowerBound           float64            `json:"lower_bound"`
	UpperBound           float64            `json:"upper_bound"`
	Confidence           float64            `json:"confidence"`
	BootstrapSeed        uint64             `json:"bootstrap_seed"`
	BootstrapSamples     int                `json:"bootstrap_samples"`
	RequiredFailures     []string           `json:"required_failures"`
	ProtectedRegressions map[string]float64 `json:"protected_regressions"`
	CostRatio            *float64           `json:"cost_ratio,omitempty"`
	LatencyRatio         *float64           `json:"latency_ratio,omitempty"`
	Complete             bool               `json:"complete"`
	MissingReasons       []string           `json:"missing_reasons"`
}
