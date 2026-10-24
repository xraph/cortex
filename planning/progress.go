package planning

import (
	"encoding/json"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

// ProgressAttempt is a host-observed attempt, not a model's self-assessment.
// Advanced means new accepted artifacts or retained checks were produced.
// FailureDigest groups equivalent failures without retaining private error text.
type ProgressAttempt struct {
	PlanDigest    string        `json:"plan_digest"`
	RunID         id.AgentRunID `json:"run_id"`
	TaskID        id.ID         `json:"task_id"`
	FinishedAt    time.Time     `json:"finished_at"`
	FailureDigest string        `json:"failure_digest,omitempty"`
	Advanced      bool          `json:"advanced"`
}

// ProgressConflict references contradictory observations about one task.
// The host must establish the contradiction from retained, authorized evidence.
type ProgressConflict struct {
	PlanDigest  string `json:"plan_digest"`
	TaskID      id.ID  `json:"task_id"`
	LeftDigest  string `json:"left_digest"`
	RightDigest string `json:"right_digest"`
}

// ProgressSnapshot binds a policy decision to the current sealed graph. The
// host reconstructs progress from retained observations and reads Account from
// execution.BudgetStore. Conservative replan charges include outstanding holds;
// they must not be replaced by a model's count or reset after a plan revision.
// The pure policy establishes no authority, capacity reservation or acceptance.
type ProgressSnapshot struct {
	SchemaVersion     int                      `json:"schema_version"`
	Plan              Plan                     `json:"plan"`
	At                time.Time                `json:"at"`
	StartedAt         time.Time                `json:"started_at"`
	LastProgressAt    time.Time                `json:"last_progress_at"`
	WaitingSince      time.Time                `json:"waiting_since,omitzero"`
	Account           execution.BudgetSnapshot `json:"account"`
	Attempts          []ProgressAttempt        `json:"attempts,omitempty"`
	Conflicts         []ProgressConflict       `json:"conflicts,omitempty"`
	UnresolvedEffects bool                     `json:"unresolved_effects"`
}

// Validate rejects inconsistent, unbounded or stale graph observations. It does
// not authenticate the observer or certify the meaning of a progress signal.
func (s ProgressSnapshot) Validate() error {
	if s.SchemaVersion != 1 || Validate(s.Plan, Limits{}) != nil || s.At.IsZero() || s.StartedAt.IsZero() || s.LastProgressAt.IsZero() || s.LastProgressAt.Before(s.StartedAt) || s.LastProgressAt.After(s.At) || len(s.Attempts) > MaxProgressAttempts || len(s.Conflicts) > MaxProgressAttempts {
		return invalid("invalid progress snapshot")
	}
	policy, _, policyErr := s.Account.Policy.Freeze()
	if policyErr != nil || policy.PlanID != s.Plan.ID || policy.Scope.Canonical() != s.Plan.Scope.Canonical() || policy.Deadline.After(s.Plan.Budget.Deadline) || policy.Limits.Steps > int64(s.Plan.Budget.MaxSteps) || policy.Limits.Tokens > int64(s.Plan.Budget.MaxTokens) || policy.Limits.ToolCalls > int64(s.Plan.Budget.MaxToolCalls) || s.Account.Charged.Validate() != nil || s.Account.Reservations < 0 || s.Account.Reservations > execution.MaxReservations {
		return invalid("invalid progress account")
	}
	if !s.WaitingSince.IsZero() && (s.WaitingSince.Before(s.StartedAt) || s.WaitingSince.After(s.At)) {
		return invalid("invalid waiting observation")
	}
	tasks := make(map[id.ID]bool, len(s.Plan.Tasks))
	for _, task := range s.Plan.Tasks {
		tasks[task.ID] = true
	}
	seen := make(map[id.ID]bool, len(s.Attempts))
	previous := s.StartedAt
	for _, attempt := range s.Attempts {
		if attempt.PlanDigest != s.Plan.Digest || attempt.RunID.IsNil() || attempt.RunID.Prefix() != id.PrefixAgentRun || seen[attempt.RunID] || !tasks[attempt.TaskID] || attempt.FinishedAt.Before(previous) || attempt.FinishedAt.After(s.At) || (attempt.Advanced && attempt.FinishedAt.After(s.LastProgressAt)) || (attempt.FailureDigest != "" && !evidence.ValidDigest(attempt.FailureDigest)) {
			return invalid("invalid progress attempt")
		}
		seen[attempt.RunID] = true
		previous = attempt.FinishedAt
	}
	conflicts := make(map[string]bool, len(s.Conflicts))
	for _, conflict := range s.Conflicts {
		if conflict.PlanDigest != s.Plan.Digest || !tasks[conflict.TaskID] || !evidence.ValidDigest(conflict.LeftDigest) || !evidence.ValidDigest(conflict.RightDigest) || conflict.LeftDigest == conflict.RightDigest {
			return invalid("invalid progress conflict")
		}
		left, right := conflict.LeftDigest, conflict.RightDigest
		if left > right {
			left, right = right, left
		}
		key := conflict.TaskID.String() + left + right
		if conflicts[key] {
			return invalid("duplicate progress conflict")
		}
		conflicts[key] = true
	}
	return nil
}

func (s ProgressSnapshot) digest() (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(s)
	if err != nil || len(data) > 2<<20 {
		return "", invalid("progress snapshot exceeds bounds")
	}
	return evidence.Digest(data), nil
}
