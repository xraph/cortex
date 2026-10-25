package optimization

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

type teamOptionEnvironment struct {
	TrialEnvironment
	receipt EnvironmentReceipt
	options engine.PreparedRunOptions
	calls   int
}

func (e *teamOptionEnvironment) Receipt() EnvironmentReceipt { return e.receipt }
func (e *teamOptionEnvironment) ExecuteOrchestration(_ context.Context, _ *engine.PreparedOrchestration, _ string, options engine.PreparedRunOptions) (*ExecutionRecord, error) {
	e.calls++
	e.options = options
	return nil, nil
}

type agentOnlyEnvironment struct {
	TrialEnvironment
	receipt EnvironmentReceipt
}

func (e agentOnlyEnvironment) Receipt() EnvironmentReceipt { return e.receipt }

func TestTeamDispatchRequiresHostSupportAndBoundedAllowance(t *testing.T) {
	version, record := orchestrationCaptureFixture(t)
	request := TrialRequest{ExperimentID: id.New(PrefixExperiment), TrialID: id.New(PrefixTrial), AttemptID: record.Environment.AttemptID, Scope: version.Scope, Deadline: time.Now().Add(time.Minute), Allowance: TrialAllowance{MaxTokens: 200}}
	for _, name := range []string{"unsupported", "money", "tokens", "expired"} {
		t.Run(name, func(t *testing.T) {
			host := &teamOptionEnvironment{receipt: record.Environment}
			var environment TrialEnvironment = host
			r := request
			tokenOnly := true
			switch name {
			case "unsupported":
				environment = agentOnlyEnvironment{receipt: record.Environment}
			case "money":
				tokenOnly = false
			case "tokens":
				r.Allowance.MaxTokens = 199
			case "expired":
				r.Deadline = time.Now().Add(-time.Second)
			}
			call, err := trialExecutionCall(environment, version, r, id.Nil, tokenOnly)
			if err == nil || call != nil || host.calls != 0 {
				t.Fatal("unqualified team reached dispatch")
			}
			if name == "unsupported" || name == "money" {
				if !errors.Is(err, ErrUnavailable) {
					t.Fatal(err)
				}
			}
		})
	}
	host := &teamOptionEnvironment{receipt: record.Environment}
	call, err := trialExecutionCall(host, version, request, id.Nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = call(context.Background()); err != nil {
		t.Fatal(err)
	}
	options := host.options
	policy := options.OrchestrationBudget
	if options.Purpose != run.PurposeOptimizationTrial || !options.SessionID.IsNil() || options.Provenance.TrialID != request.TrialID.String() || policy == nil || policy.Deadline.After(request.Deadline) || policy.PreparedDigest != version.Orchestration.Prepared.Digest() || len(policy.ParticipantDigests) != 2 {
		t.Fatal("team dispatch lost host bounds or provenance")
	}
	again, err := orchestrationTrialBudget(version, request)
	if err != nil || again.ExecutionID != policy.ExecutionID {
		t.Fatalf("attempt changed execution identity: %v", err)
	}
	request.AttemptID = id.New(PrefixAttempt)
	other, err := orchestrationTrialBudget(version, request)
	if err != nil || other.ExecutionID == policy.ExecutionID {
		t.Fatal("different attempts share a team")
	}
}
