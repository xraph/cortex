package planning

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

func replanSnapshot(t *testing.T) ProgressSnapshot {
	t.Helper()
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	p := fixturePlan(t)
	account := execution.BudgetSnapshot{Policy: execution.ReservationPolicy{SchemaVersion: 1, PlanID: p.ID, Scope: p.Scope, Limits: execution.Resources{Steps: int64(p.Budget.MaxSteps), Tokens: int64(p.Budget.MaxTokens), ToolCalls: int64(p.Budget.MaxToolCalls), FanOut: 16, Replans: 2}, Deadline: p.Budget.Deadline, TokenOnly: true}}
	return ProgressSnapshot{SchemaVersion: 1, Plan: p, Account: account, At: now, StartedAt: now.Add(-time.Minute), LastProgressAt: now.Add(-time.Minute)}
}
func failureAttempt(s ProgressSnapshot, offset time.Duration, fingerprint string) ProgressAttempt {
	return ProgressAttempt{PlanDigest: s.Plan.Digest, RunID: id.NewAgentRunID(), TaskID: s.Plan.Tasks[0].ID, FinishedAt: s.At.Add(offset), FailureDigest: evidence.Digest([]byte(fingerprint))}
}

func TestRepeatedFailureTriggersBoundedReplan(t *testing.T) {
	s := replanSnapshot(t)
	s.Attempts = []ProgressAttempt{failureAttempt(s, -2*time.Second, "same failure"), failureAttempt(s, -time.Second, "same failure")}
	decision := ProposeReplan(s, ReplanPolicy{})
	require.Equal(t, ReplanPropose, decision.Action)
	require.Equal(t, ReplanRepeatedFailure, decision.Reason)
	require.Equal(t, s.Plan.Digest, decision.PlanDigest)
	require.True(t, evidence.ValidDigest(decision.SnapshotDigest))
	s.Account.Charged.Replans = 2
	decision = ProposeReplan(s, ReplanPolicy{})
	require.Equal(t, ReplanStop, decision.Action)
	require.Equal(t, ReplanLimit, decision.Reason)
}

func TestNoProgressStopsAtReplanLimit(t *testing.T) {
	s := replanSnapshot(t)
	for i := 3; i > 0; i-- {
		a := failureAttempt(s, -time.Duration(i)*time.Second, "different")
		a.FailureDigest = ""
		s.Attempts = append(s.Attempts, a)
	}
	require.Equal(t, ReplanNoProgress, ProposeReplan(s, ReplanPolicy{}).Reason)
	s.Account.Charged.Replans = 2
	require.Equal(t, ReplanLimit, ProposeReplan(s, ReplanPolicy{}).Reason)
	s.Account.Charged.Replans = 0
	s.Attempts[2].Advanced = true
	s.LastProgressAt = s.Attempts[2].FinishedAt
	require.Equal(t, ReplanContinue, ProposeReplan(s, ReplanPolicy{}).Action)
}

func TestReplanCannotWeakenAcceptance(t *testing.T) {
	s := replanSnapshot(t)
	s.Attempts = []ProgressAttempt{failureAttempt(s, -2*time.Second, "same"), failureAttempt(s, -time.Second, "same")}
	proposal := Revision{Plan: s.Plan, Actor: "host", Reason: "change approach"}
	proposal.Plan.Tasks = append([]Task(nil), s.Plan.Tasks...)
	proposal.Plan.Tasks[0].Instruction = "try another bounded approach"
	require.Equal(t, ReplanPropose, ProposeReplan(s, ReplanPolicy{}).Action)
	next, err := ApplyRevision(s.Plan, proposal, s.Plan.Revision, Limits{})
	require.NoError(t, err)
	require.Equal(t, s.Plan.Contract.Digest, next.Contract.Digest)
	require.Equal(t, s.Plan.Revision+1, next.Revision)
	proposal.Plan.Budget.MaxTokens++
	_, err = ApplyRevision(s.Plan, proposal, s.Plan.Revision, Limits{})
	require.ErrorIs(t, err, ErrInvalid)
	proposal.Plan = s.Plan
	proposal.Plan.Tasks = nil
	_, err = ApplyRevision(s.Plan, proposal, s.Plan.Revision, Limits{})
	require.ErrorIs(t, err, ErrInvalid)
	s.Account.Charged.Replans = 2
	require.Equal(t, ReplanStop, ProposeReplan(s, ReplanPolicy{}).Action)
}

func TestReplanPrioritizesUnresolvedEffectsAndFiniteBudgets(t *testing.T) {
	s := replanSnapshot(t)
	s.UnresolvedEffects = true
	s.Account.Charged.Replans = 2
	s.At = s.Plan.Budget.Deadline.Add(time.Second)
	decision := ProposeReplan(s, ReplanPolicy{})
	require.Equal(t, ReplanReconcile, decision.Action)
	require.Equal(t, ReplanUnresolvedEffects, decision.Reason)
	s.UnresolvedEffects = false
	require.Equal(t, ReplanDeadline, ProposeReplan(s, ReplanPolicy{}).Reason)
	s.At = s.Plan.Budget.Deadline.Add(-time.Second)
	s.Account.Charged.Tokens = int64(s.Plan.Budget.MaxTokens)
	require.Equal(t, ReplanBudget, ProposeReplan(s, ReplanPolicy{}).Reason)
}

func TestReplanWaitingIdleConflictAndDeadline(t *testing.T) {
	for _, mode := range []string{"waiting", "stalled_wait", "idle", "conflict", "deadline_pressure", "paused_deadline"} {
		t.Run(mode, func(t *testing.T) {
			s := replanSnapshot(t)
			policy := ReplanPolicy{MaxIdle: 30 * time.Second}
			var action ReplanAction
			var reason ReplanReason
			switch mode {
			case "waiting":
				s.WaitingSince = s.At.Add(-time.Second)
				action, reason = ReplanWait, ReplanWaiting
			case "stalled_wait":
				s.WaitingSince = s.StartedAt
				action, reason = ReplanPropose, ReplanStalledWait
			case "idle":
				action, reason = ReplanPropose, ReplanIdle
			case "conflict":
				s.Conflicts = []ProgressConflict{{PlanDigest: s.Plan.Digest, TaskID: s.Plan.Tasks[0].ID, LeftDigest: evidence.Digest([]byte("left")), RightDigest: evidence.Digest([]byte("right"))}}
				action, reason = ReplanPropose, ReplanContradiction
			case "deadline_pressure":
				s.At = s.Plan.Budget.Deadline.Add(-time.Second)
				action, reason = ReplanPropose, ReplanDeadlinePressure
			case "paused_deadline":
				s.WaitingSince = s.StartedAt
				s.At = s.Plan.Budget.Deadline
				action, reason = ReplanStop, ReplanDeadline
			}
			decision := ProposeReplan(s, policy)
			require.Equal(t, action, decision.Action)
			require.Equal(t, reason, decision.Reason)
		})
	}
}

func TestReplanRejectsMalformedObservationsAndPolicies(t *testing.T) {
	for name, mutate := range map[string]func(*ProgressSnapshot){
		"schema":              func(s *ProgressSnapshot) { s.SchemaVersion = 2 },
		"changed graph":       func(s *ProgressSnapshot) { s.Plan.Objective = "changed" },
		"future progress":     func(s *ProgressSnapshot) { s.LastProgressAt = s.At.Add(time.Second) },
		"future waiting":      func(s *ProgressSnapshot) { s.WaitingSince = s.At.Add(time.Second) },
		"negative count":      func(s *ProgressSnapshot) { s.Account.Charged.Replans = -1 },
		"negative usage":      func(s *ProgressSnapshot) { s.Account.Charged.Tokens = -1 },
		"wrong account":       func(s *ProgressSnapshot) { s.Account.Policy.PlanID = id.New(PrefixPlan) },
		"duplicate attempt":   func(s *ProgressSnapshot) { s.Attempts = append(s.Attempts, s.Attempts[0]) },
		"stale attempt":       func(s *ProgressSnapshot) { s.Attempts[0].PlanDigest = evidence.Digest([]byte("prior plan")) },
		"unknown task":        func(s *ProgressSnapshot) { s.Attempts[0].TaskID = id.New(PrefixTask) },
		"invalid attempt":     func(s *ProgressSnapshot) { s.Attempts[0].RunID = id.NewAgentID() },
		"future attempt":      func(s *ProgressSnapshot) { s.Attempts[0].FinishedAt = s.At.Add(time.Second) },
		"unbounded history":   func(s *ProgressSnapshot) { s.Attempts = make([]ProgressAttempt, MaxProgressAttempts+1) },
		"invalid failure":     func(s *ProgressSnapshot) { s.Attempts[0].FailureDigest = "private error" },
		"unrecorded progress": func(s *ProgressSnapshot) { s.Attempts[0].Advanced = true },
		"same evidence": func(s *ProgressSnapshot) {
			digest := evidence.Digest(nil)
			s.Conflicts = []ProgressConflict{{PlanDigest: s.Plan.Digest, TaskID: s.Plan.Tasks[0].ID, LeftDigest: digest, RightDigest: digest}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := replanSnapshot(t)
			s.Attempts = []ProgressAttempt{failureAttempt(s, -time.Second, "failure")}
			mutate(&s)
			decision := ProposeReplan(s, ReplanPolicy{})
			require.Equal(t, ReplanStop, decision.Action)
			require.Equal(t, ReplanInvalidSnapshot, decision.Reason)
			require.Empty(t, decision.SnapshotDigest)
		})
	}
	for _, policy := range []ReplanPolicy{{SchemaVersion: 2}, {MaxReplans: -1}, {MaxReplans: MaxReplanAttempts + 1}, {FailureThreshold: -1}, {NoProgressThreshold: MaxProgressAttempts + 1}, {MaxIdle: -time.Second}, {DeadlineReserve: 25 * time.Hour}} {
		decision := ProposeReplan(replanSnapshot(t), policy)
		require.Equal(t, ReplanStop, decision.Action)
		require.Equal(t, ReplanInvalidPolicy, decision.Reason)
	}
}

func TestReplanDecisionIsDeterministicAndDoesNotMutateInputs(t *testing.T) {
	s := replanSnapshot(t)
	s.Attempts = []ProgressAttempt{failureAttempt(s, -2*time.Second, "same"), failureAttempt(s, -time.Second, "same")}
	before, err := json.Marshal(s)
	require.NoError(t, err)
	first := ProposeReplan(s, ReplanPolicy{})
	normalized, err := (ReplanPolicy{}).Normalize()
	require.NoError(t, err)
	require.Equal(t, first, ProposeReplan(s, normalized))
	for range 20 {
		require.Equal(t, first, ProposeReplan(s, ReplanPolicy{}))
	}
	after, err := json.Marshal(s)
	require.NoError(t, err)
	require.Equal(t, before, after)
	changed := ProposeReplan(s, ReplanPolicy{MaxReplans: 3})
	require.NotEqual(t, first.PolicyDigest, changed.PolicyDigest)
	require.Equal(t, first.SnapshotDigest, changed.SnapshotDigest)
	require.Equal(t, ReplanDisabled, ProposeReplan(s, ReplanPolicy{Disabled: true}).Reason)
	s.Attempts[1].FailureDigest = evidence.Digest([]byte("different"))
	require.Equal(t, ReplanContinue, ProposeReplan(s, ReplanPolicy{}).Action)
}

func TestReplanDoesNotRequestChangesForHealthyWork(t *testing.T) {
	s := replanSnapshot(t)
	require.Equal(t, ReplanContinue, ProposeReplan(s, ReplanPolicy{}).Action)
	proposal := Revision{Plan: s.Plan, Actor: "host", Reason: "revise"}
	_, err := ApplyRevision(s.Plan, proposal, s.Plan.Revision+1, Limits{})
	require.ErrorIs(t, err, ErrConflict)
}

func TestReplanPolicyCannotWidenTheDurableAccount(t *testing.T) {
	s := replanSnapshot(t)
	s.Attempts = []ProgressAttempt{failureAttempt(s, -2*time.Second, "same"), failureAttempt(s, -time.Second, "same")}
	require.Equal(t, ReplanInvalidPolicy, ProposeReplan(s, ReplanPolicy{MaxReplans: 1}).Reason)
	s.Account.Charged.Replans = 2
	require.Equal(t, ReplanLimit, ProposeReplan(s, ReplanPolicy{MaxReplans: 3}).Reason)
	s.Account.Overflow = true
	require.Equal(t, ReplanBudget, ProposeReplan(s, ReplanPolicy{}).Reason)
}
