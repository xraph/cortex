package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
)

type controllerComparisonEvaluator struct {
	optimization.Evaluator
	compare func(context.Context, optimization.ControllerComparisonRequest) (optimization.Comparison, error)
}

func (e controllerComparisonEvaluator) CompareControllers(ctx context.Context, request optimization.ControllerComparisonRequest) (optimization.Comparison, error) {
	return e.compare(ctx, request)
}

type controllerDecisionAuthority struct {
	controllerAuthority
	denied id.ID
}

func (a *controllerDecisionAuthority) Authorize(_ context.Context, _ optimization.Action, resource optimization.Resource) error {
	if resource.ID == a.denied {
		return optimization.ErrPolicyDenied
	}
	return nil
}

// ControllerDecisionWorker verifies comparison isolation and authority at commit preparation.
func ControllerDecisionWorker(t *testing.T, factory Factory) {
	t.Helper()
	f, allocation, evidence, expected := controllerDecisionFixture(t, factory)
	request, err := optimization.BuildControllerComparison(f.parent.Experiment, allocation, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if missing, buildErr := optimization.BuildControllerInconclusive(f.parent.Experiment, allocation, evidence); buildErr != nil || missing != nil {
		t.Fatalf("complete scores marked missing: %+v %v", missing, buildErr)
	}
	for _, mode := range []string{"missing-capture", "incomplete-score"} {
		var incomplete []optimization.ControllerEvidence
		data, marshalErr := json.Marshal(evidence)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if decodeErr := json.Unmarshal(data, &incomplete); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		item := &incomplete[1].Trials[0]
		if mode == "missing-capture" {
			item.Trial.State, item.Trial.Error = optimization.TrialFailed, "execution failed"
			item.Trial.Execution, item.Trial.EvaluationID, item.Evaluation = nil, "", nil
		} else {
			item.Evaluation.Complete, item.Evaluation.Score = false, nil
			item.Evaluation.Errors = []string{"score unavailable"}
		}
		missing, buildErr := optimization.BuildControllerInconclusive(f.parent.Experiment, allocation, incomplete)
		if buildErr != nil || missing == nil {
			t.Fatalf("missing evidence: %+v %v", missing, buildErr)
		}
		decision := expected
		decision.Comparison = *missing
		decision.EvaluationIDs = append(append([]string(nil), missing.BaselineIDs...), missing.CandidateIDs...)
		if validationErr := optimization.ValidateControllerDecisionEvidence(f.parent.Experiment, allocation, decision, incomplete); validationErr != nil {
			t.Fatalf("invalid incomplete summary: %v", validationErr)
		}
	}
	authority := &controllerDecisionAuthority{controllerAuthority: controllerAuthority{actor: f.parent.Experiment.Spec.Requester}}
	calls := 0
	revoke, corrupt := false, false
	evaluator := controllerComparisonEvaluator{compare: func(_ context.Context, got optimization.ControllerComparisonRequest) (optimization.Comparison, error) {
		calls++
		if !reflect.DeepEqual(got, request) {
			t.Fatal("worker changed original comparison roster")
		}
		got.Groups[0].Plan.CaseDigests["mutated"] = "not authoritative"
		if revoke {
			authority.denied = evidence[0].Experiment.ID
		}
		result := expected.Comparison
		if corrupt {
			result.MeanDelta = 1
		}
		return result, nil
	}}
	config := optimization.OptimizerConfig{Repository: f.s, Authorizer: authority, IdentityResolver: authority, Caller: authority, ImprovementEvaluator: evaluator, Handler: unusedPhase{}, Worker: optimization.DefaultWorkerConfig()}
	handler, err := optimization.NewImprovementPhaseHandler(config)
	if err != nil {
		t.Fatal(err)
	}
	item := optimization.WorkItem{Experiment: f.parent.Experiment, Lease: f.leases[0]}
	authority.denied = evidence[0].Experiment.ID
	if _, err = handler.Execute(f.ctx, item); !errors.Is(err, optimization.ErrPolicyDenied) || calls != 0 {
		t.Fatalf("denied child compared: %v calls=%d", err, calls)
	}
	authority.denied = id.ID{}
	revoke = true
	if _, err = handler.Execute(f.ctx, item); !errors.Is(err, optimization.ErrPolicyDenied) || calls != 1 {
		t.Fatalf("revoked child returned decision: %v calls=%d", err, calls)
	}
	authority.denied, revoke, corrupt = id.ID{}, false, true
	if _, err = handler.Execute(f.ctx, item); !errors.Is(err, optimization.ErrIncompleteEvidence) {
		t.Fatalf("accepted fabricated comparison: %v", err)
	}
	corrupt = false
	change, err := handler.Execute(f.ctx, item)
	if err != nil || change.Decision == nil || change.NextPhase != optimization.PhaseReflect || change.Decision.SelectionKind != string(optimization.HarnessImprovement) {
		t.Fatalf("parent decision: %+v %v", change, err)
	}
	if err = optimization.ValidateControllerDecisionEvidence(f.parent.Experiment, allocation, *change.Decision, evidence); err != nil {
		t.Fatal(err)
	}
	commitTrialChange(f.ctx, t, f.s, decisionChange(f.ctx, t, f.s, f.parent, f.leases[0], *change.Decision))
}
