package planning

import (
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

// Hard bounds keep policy work and proposal counts finite.
const (
	MaxReplanAttempts   = 16
	MaxProgressAttempts = 256
)

// ReplanPolicy bounds proposal selection. Zero fields use two proposal attempts,
// two equivalent failures, three attempts without progress, five minutes idle
// and thirty seconds of deadline reserve. Disabled stops proposal selection.
// The account replan allowance must not exceed MaxReplans, so an atomic
// reservation can enforce this ceiling across competing workers. These settings
// do not authorize retries, cancel work or change acceptance.
type ReplanPolicy struct {
	SchemaVersion       int           `json:"schema_version"`
	Disabled            bool          `json:"disabled"`
	MaxReplans          int           `json:"max_replans"`
	FailureThreshold    int           `json:"failure_threshold"`
	NoProgressThreshold int           `json:"no_progress_threshold"`
	MaxIdle             time.Duration `json:"max_idle"`
	DeadlineReserve     time.Duration `json:"deadline_reserve"`
}

// Normalize resolves defaults and rejects settings beyond finite host bounds.
func (p ReplanPolicy) Normalize() (ReplanPolicy, error) {
	if p.SchemaVersion == 0 {
		p.SchemaVersion = 1
	}
	if p.MaxReplans == 0 {
		p.MaxReplans = 2
	}
	if p.FailureThreshold == 0 {
		p.FailureThreshold = 2
	}
	if p.NoProgressThreshold == 0 {
		p.NoProgressThreshold = 3
	}
	if p.MaxIdle == 0 {
		p.MaxIdle = 5 * time.Minute
	}
	if p.DeadlineReserve == 0 {
		p.DeadlineReserve = 30 * time.Second
	}
	if p.SchemaVersion != 1 || p.MaxReplans < 1 || p.MaxReplans > MaxReplanAttempts || p.FailureThreshold < 1 || p.FailureThreshold > MaxProgressAttempts || p.NoProgressThreshold < 1 || p.NoProgressThreshold > MaxProgressAttempts || p.MaxIdle < 0 || p.MaxIdle > 24*time.Hour || p.DeadlineReserve < 0 || p.DeadlineReserve > 24*time.Hour {
		return ReplanPolicy{}, invalid("invalid replan policy")
	}
	return p, nil
}

// ReplanAction is a proposed next step, never permission to execute it.
type ReplanAction string

const (
	ReplanContinue  ReplanAction = "continue"
	ReplanWait      ReplanAction = "wait"
	ReplanReconcile ReplanAction = "reconcile"
	ReplanPropose   ReplanAction = "propose"
	ReplanStop      ReplanAction = "stop"
)

// ReplanReason is a bounded explanation without private failure content.
type ReplanReason string

const (
	ReplanHealthy           ReplanReason = "healthy"
	ReplanWaiting           ReplanReason = "waiting"
	ReplanStalledWait       ReplanReason = "stalled_wait"
	ReplanIdle              ReplanReason = "idle"
	ReplanRepeatedFailure   ReplanReason = "repeated_failure"
	ReplanNoProgress        ReplanReason = "no_progress"
	ReplanContradiction     ReplanReason = "contradictory_evidence"
	ReplanUnresolvedEffects ReplanReason = "unresolved_effects"
	ReplanDeadlinePressure  ReplanReason = "deadline_pressure"
	ReplanDeadline          ReplanReason = "deadline"
	ReplanBudget            ReplanReason = "budget"
	ReplanLimit             ReplanReason = "replan_limit"
	ReplanDisabled          ReplanReason = "disabled"
	ReplanInvalidSnapshot   ReplanReason = "invalid_snapshot"
	ReplanInvalidPolicy     ReplanReason = "invalid_policy"
)

// ReplanDecision binds a proposal to exact observations and normalized policy.
// Reserve a proposal attempt under current host authorization before requesting
// model output. Persist its consumption even if the model or validation fails.
type ReplanDecision struct {
	SchemaVersion  int          `json:"schema_version"`
	Action         ReplanAction `json:"action"`
	Reason         ReplanReason `json:"reason"`
	PlanID         id.ID        `json:"plan_id"`
	PlanRevision   int64        `json:"plan_revision"`
	PlanDigest     string       `json:"plan_digest"`
	SnapshotDigest string       `json:"snapshot_digest"`
	PolicyDigest   string       `json:"policy_digest"`
}

// ProposeReplan selects a deterministic response to trusted progress. It performs
// no I/O, does not mutate the snapshot and never claims that a task succeeded.
// Unknown effects take priority even after deadlines: stopping dispatch must
// not discard the obligation to reconcile an external operation.
func ProposeReplan(snapshot ProgressSnapshot, policy ReplanPolicy) ReplanDecision {
	resolved, err := policy.Normalize()
	if err != nil {
		return ReplanDecision{SchemaVersion: 1, Action: ReplanStop, Reason: ReplanInvalidPolicy}
	}
	digest, err := snapshot.digest()
	if err != nil {
		return ReplanDecision{SchemaVersion: 1, Action: ReplanStop, Reason: ReplanInvalidSnapshot}
	}
	policyData, err := encode(resolved)
	if err != nil {
		return ReplanDecision{SchemaVersion: 1, Action: ReplanStop, Reason: ReplanInvalidPolicy}
	}
	decision := ReplanDecision{SchemaVersion: 1, Action: ReplanContinue, Reason: ReplanHealthy, PlanID: snapshot.Plan.ID, PlanRevision: snapshot.Plan.Revision, PlanDigest: snapshot.Plan.Digest, SnapshotDigest: digest, PolicyDigest: evidence.Digest(policyData)}
	if snapshot.UnresolvedEffects {
		decision.Action, decision.Reason = ReplanReconcile, ReplanUnresolvedEffects
		return decision
	}
	if !snapshot.At.Before(snapshot.Account.Policy.Deadline) || !snapshot.At.Before(snapshot.Plan.Budget.Deadline) {
		decision.Action, decision.Reason = ReplanStop, ReplanDeadline
		return decision
	}
	account := snapshot.Account
	if account.Policy.Limits.Replans > int64(resolved.MaxReplans) {
		decision.Action, decision.Reason = ReplanStop, ReplanInvalidPolicy
		return decision
	}
	if account.Overflow || !account.Policy.Limits.Covers(account.Charged) || account.Charged.Steps >= account.Policy.Limits.Steps || account.Charged.Tokens >= account.Policy.Limits.Tokens || account.Charged.FanOut >= account.Policy.Limits.FanOut || (!account.Policy.TokenOnly && account.Charged.USDMicros >= account.Policy.Limits.USDMicros) {
		decision.Action, decision.Reason = ReplanStop, ReplanBudget
		return decision
	}
	reason := replanTrigger(snapshot, resolved)
	if reason == ReplanHealthy || reason == ReplanWaiting {
		decision.Reason = reason
		if reason == ReplanWaiting {
			decision.Action = ReplanWait
		}
		return decision
	}
	switch {
	case resolved.Disabled:
		decision.Action, decision.Reason = ReplanStop, ReplanDisabled
	case account.Charged.Replans >= account.Policy.Limits.Replans:
		decision.Action, decision.Reason = ReplanStop, ReplanLimit
	default:
		decision.Action, decision.Reason = ReplanPropose, reason
	}
	return decision
}

func replanTrigger(s ProgressSnapshot, p ReplanPolicy) ReplanReason {
	if len(s.Conflicts) > 0 {
		return ReplanContradiction
	}
	if s.Plan.Budget.Deadline.Sub(s.At) <= p.DeadlineReserve {
		return ReplanDeadlinePressure
	}
	if !s.WaitingSince.IsZero() {
		if s.At.Sub(s.WaitingSince) >= p.MaxIdle {
			return ReplanStalledWait
		}
		return ReplanWaiting
	}
	type streak struct {
		digest string
		count  int
	}
	failures := make(map[id.ID]streak)
	unchanged := 0
	for _, attempt := range s.Attempts {
		prior := failures[attempt.TaskID]
		switch {
		case attempt.FailureDigest == "":
			prior = streak{}
		case prior.digest == attempt.FailureDigest:
			prior.count++
		default:
			prior = streak{digest: attempt.FailureDigest, count: 1}
		}
		failures[attempt.TaskID] = prior
		if attempt.Advanced || !attempt.FinishedAt.After(s.LastProgressAt) {
			unchanged = 0
		} else {
			unchanged++
		}
	}
	for _, failure := range failures {
		if failure.count >= p.FailureThreshold {
			return ReplanRepeatedFailure
		}
	}
	if unchanged >= p.NoProgressThreshold {
		return ReplanNoProgress
	}
	if s.At.Sub(s.LastProgressAt) >= p.MaxIdle {
		return ReplanIdle
	}
	return ReplanHealthy
}
