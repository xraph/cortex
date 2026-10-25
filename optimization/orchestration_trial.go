package optimization

import (
	"context"
	"strings"
	"time"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
)

// OrchestrationTrialEnvironment is optional host support for isolated team trials.
// The host enforces the supplied shared policy and retains original child evidence.
// Inspect recovers the same team; it must never dispatch a replacement execution.
type OrchestrationTrialEnvironment interface {
	TrialEnvironment
	ExecuteOrchestration(context.Context, *engine.PreparedOrchestration, string, engine.PreparedRunOptions) (*ExecutionRecord, error)
}

func trialExecutionCall(environment TrialEnvironment, version HarnessVersion, request TrialRequest, candidate id.ID, tokenOnly bool) (func(context.Context) (*ExecutionRecord, error), error) {
	options := engine.PreparedRunOptions{Purpose: run.PurposeOptimizationTrial, Provenance: run.ExecutionProvenance{ExperimentID: request.ExperimentID.String(), TrialID: request.TrialID.String(), CandidateID: candidate.String(), EnvironmentReceiptID: environment.Receipt().ID, HarnessID: version.HarnessID.String(), HarnessVersion: version.ID.String()}}
	if version.Orchestration != nil {
		team, ok := environment.(OrchestrationTrialEnvironment)
		if !ok || !tokenOnly {
			return nil, ErrUnavailable
		}
		policy, err := orchestrationTrialBudget(version, request)
		if err != nil {
			return nil, err
		}
		options.OrchestrationBudget = &policy
		return func(ctx context.Context) (*ExecutionRecord, error) {
			return team.ExecuteOrchestration(ctx, version.Orchestration.Prepared, request.Task.Input, options)
		}, nil
	}
	session, err := id.ParseSessionID("ses_" + strings.SplitN(request.AttemptID.String(), "_", 2)[1])
	if err != nil {
		return nil, err
	}
	options.SessionID = session
	return func(ctx context.Context) (*ExecutionRecord, error) {
		return environment.Execute(ctx, version.Prepared, request.Task.Input, options)
	}, nil
}

func orchestrationTrialBudget(version HarnessVersion, request TrialRequest) (orchestration.BudgetPolicy, error) {
	var empty orchestration.BudgetPolicy
	if _, err := ParseID(request.AttemptID.String(), PrefixAttempt); err != nil {
		return empty, err
	}
	preset, err := orchestrationPreset(version.Policy, version.Orchestration.Preset)
	if err != nil {
		return empty, err
	}
	if preset.Limits.Tokens > request.Allowance.MaxTokens {
		return empty, ErrPolicyDenied
	}
	key, err := id.ParseOrchestrationID("orch_" + strings.SplitN(request.AttemptID.String(), "_", 2)[1])
	if err != nil {
		return empty, err
	}
	policy, err := orchestrationExecutionBudget(version, key, request.Deadline)
	if err != nil {
		return empty, err
	}
	if !time.Now().Before(policy.Deadline) {
		return empty, ErrUnknownAttempt
	}
	return policy, nil
}

func orchestrationExecutionBudget(version HarnessVersion, key id.OrchestrationID, deadline time.Time) (orchestration.BudgetPolicy, error) {
	var empty orchestration.BudgetPolicy
	preset, err := orchestrationPreset(version.Policy, version.Orchestration.Preset)
	if err != nil {
		return empty, err
	}
	view, err := version.Orchestration.Prepared.Inspect()
	if err != nil {
		return empty, err
	}
	policy := orchestration.BudgetPolicy{SchemaVersion: 1, ExecutionID: key, PreparedDigest: version.Orchestration.Prepared.Digest(), Scope: version.Scope, Limits: preset.Limits, Deadline: deadline, TokenOnly: true, Participants: map[string]execution.Resources{}, ParticipantDigests: map[string]string{}}
	for name, prepared := range view.Participants {
		child, inspectErr := prepared.Inspect()
		if inspectErr != nil {
			return empty, inspectErr
		}
		budget := child.Config.Budget
		policy.Participants[name] = participantResources(child)
		policy.ParticipantDigests[name] = prepared.Digest()
		if policy.Deadline.IsZero() || budget.Deadline.Before(policy.Deadline) {
			policy.Deadline = budget.Deadline
		}
	}
	frozen, _, err := policy.Freeze()
	return frozen, err
}
