package storetest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

type improvementEvaluator struct {
	trialFixtureEvaluator
	plans    map[optimization.ContentRef]optimization.EvaluationPlan
	calls    int
	onFreeze func(optimization.EvaluationSpec)
}

func (e *improvementEvaluator) Freeze(_ context.Context, spec optimization.EvaluationSpec) (optimization.EvaluationPlan, error) {
	e.calls++
	if e.onFreeze != nil {
		e.onFreeze(spec)
	}
	plan, ok := e.plans[spec.Source]
	if !ok {
		return optimization.EvaluationPlan{}, optimization.ErrNotFound
	}
	if len(spec.Tasks) != len(plan.CaseDigests) {
		return optimization.EvaluationPlan{}, optimization.ErrIncompleteEvidence
	}
	for _, task := range spec.Tasks {
		if plan.CaseDigests[task.ID] != task.Digest {
			return optimization.EvaluationPlan{}, optimization.ErrIncompleteEvidence
		}
	}
	return plan, nil
}

type improvementCreationAuthority struct {
	controllerAuthority
	denied  id.ID
	creates map[id.ID]int
}

func (a *improvementCreationAuthority) Authorize(_ context.Context, action optimization.Action, resource optimization.Resource) error {
	if action == optimization.ActionCreate {
		a.creates[resource.ID]++
		if resource.ID == a.denied {
			return optimization.ErrPolicyDenied
		}
	}
	return nil
}

func improvementService(t *testing.T, s store.Store, authority *improvementCreationAuthority, evaluator optimization.Evaluator) *optimization.Optimizer {
	t.Helper()
	op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, ImprovementEvaluator: evaluator, Handler: unusedPhase{}, Worker: optimization.DefaultWorkerConfig()})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func improvementCreation(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("ImprovementCreationQualifiesEveryDistinctPlan", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle, _ := improvementFixture(t)
		childPlan := bundle.Experiment.EvaluationPlan
		bundle.Experiment.Spec.Evaluation.Revision = "parent-stream"
		bundle.Experiment.EvaluationPlan.Ref = bundle.Experiment.Spec.Evaluation
		authority := &improvementCreationAuthority{controllerAuthority: controllerAuthority{actor: bundle.Experiment.Spec.Requester}, creates: map[id.ID]int{}}
		evaluator := &improvementEvaluator{plans: map[optimization.ContentRef]optimization.EvaluationPlan{bundle.Experiment.EvaluationPlan.Ref: bundle.Experiment.EvaluationPlan}}
		op := improvementService(t, s, authority, evaluator)
		ctx := scoped(bundle.Experiment.Spec.Scope)
		if _, err := op.CreateImprovementExperiment(ctx, optimization.ImprovementSpec{Bundle: bundle}); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("unqualified child plan accepted: %v", err)
		}
		evaluator.plans[childPlan.Ref] = childPlan
		evaluator.calls = 0
		originalInput := bundle.Experiment.Spec.Tasks[0].Input
		evaluator.onFreeze = func(spec optimization.EvaluationSpec) { spec.Tasks[0].Input = "adapter mutation" }
		created, err := op.CreateImprovementExperiment(ctx, optimization.ImprovementSpec{Bundle: bundle})
		if err != nil || evaluator.calls != 2 {
			t.Fatalf("distinct qualification: calls=%d err=%v", evaluator.calls, err)
		}
		if created.Spec.Tasks[0].Input != originalInput || bundle.Experiment.Spec.Tasks[0].Input != originalInput || created.Spec.Improvement.Children[0].Spec.Tasks[0].Input != originalInput {
			t.Fatal("evaluator mutated frozen input")
		}
	})
	t.Run("ImprovementCreationAuthenticatesAndFreezes", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle, children := improvementFixture(t)
		ctx := scoped(bundle.Experiment.Spec.Scope)
		authority := &improvementCreationAuthority{controllerAuthority: controllerAuthority{actor: bundle.Experiment.Spec.Requester}, creates: map[id.ID]int{}}
		evaluator := &improvementEvaluator{plans: map[optimization.ContentRef]optimization.EvaluationPlan{bundle.Experiment.Spec.Evaluation: bundle.Experiment.EvaluationPlan}}
		op := improvementService(t, s, authority, evaluator)
		created, err := op.CreateImprovementExperiment(ctx, optimization.ImprovementSpec{Bundle: bundle})
		if err != nil {
			t.Fatal(err)
		}
		if created.State != optimization.StateDraft || created.ID != bundle.Experiment.ID || !reflect.DeepEqual(created.Spec.Improvement, bundle.Experiment.Spec.Improvement) {
			t.Fatalf("changed frozen comparison: %+v", created)
		}
		if evaluator.calls != 1 {
			t.Fatalf("same frozen source qualified %d times", evaluator.calls)
		}
		for _, child := range children {
			if authority.creates[child.Experiment.ID] < 2 {
				t.Fatal("child authority not rechecked after qualification")
			}
			if _, getErr := s.GetExperiment(ctx, child.Experiment.ID); !errors.Is(getErr, optimization.ErrNotFound) {
				t.Fatalf("creation scheduled child: %v", getErr)
			}
		}
		bundle.Experiment.Spec.Improvement.Children[0].Spec.Tasks[0].Input = "changed after creation"
		stored, err := s.GetExperiment(ctx, created.ID)
		if err != nil || stored.Spec.Improvement.Children[0].Spec.Tasks[0].Input == "changed after creation" {
			t.Fatalf("mutable template persisted: %v", err)
		}
	})
	for _, mode := range []string{"unavailable", "denied-parent", "denied-child", "revoked-parent", "revoked-child", "changed-caller", "changed-plan", "missing-plan"} {
		t.Run("ImprovementCreationRejects/"+mode, func(t *testing.T) {
			s := factory(t, time.Now)
			bundle, children := improvementFixture(t)
			ctx := scoped(bundle.Experiment.Spec.Scope)
			authority := &improvementCreationAuthority{controllerAuthority: controllerAuthority{actor: bundle.Experiment.Spec.Requester}, creates: map[id.ID]int{}}
			evaluator := &improvementEvaluator{plans: map[optimization.ContentRef]optimization.EvaluationPlan{bundle.Experiment.Spec.Evaluation: bundle.Experiment.EvaluationPlan}}
			bundle.Experiment.Spec.Evaluation.Revision = "parent-stream"
			bundle.Experiment.EvaluationPlan.Ref = bundle.Experiment.Spec.Evaluation
			evaluator.plans[bundle.Experiment.Spec.Evaluation] = bundle.Experiment.EvaluationPlan
			var adapter optimization.Evaluator = evaluator
			want := optimization.ErrPolicyDenied
			switch mode {
			case "unavailable":
				adapter = nil
				want = optimization.ErrUnavailable
			case "denied-parent":
				authority.denied = bundle.Experiment.ID
			case "denied-child":
				authority.denied = children[1].Experiment.ID
			case "revoked-parent":
				evaluator.onFreeze = func(optimization.EvaluationSpec) { authority.denied = bundle.Experiment.ID }
			case "revoked-child":
				evaluator.onFreeze = func(optimization.EvaluationSpec) { authority.denied = children[1].Experiment.ID }
			case "changed-caller":
				evaluator.onFreeze = func(optimization.EvaluationSpec) { authority.actor.Subject = "replacement" }
			case "missing-plan":
				bundle.Experiment.Spec.Improvement = nil
				want = optimization.ErrInvalidSpec
			case "changed-plan":
				plan := bundle.Experiment.EvaluationPlan
				plan.FrozenAt = plan.FrozenAt.Add(time.Second)
				evaluator.plans[plan.Ref] = plan
				want = optimization.ErrIncompleteEvidence
			}
			op := improvementService(t, s, authority, adapter)
			if _, err := op.CreateImprovementExperiment(ctx, optimization.ImprovementSpec{Bundle: bundle}); !errors.Is(err, want) {
				t.Fatalf("%s accepted: %v", mode, err)
			}
			if _, err := s.GetExperiment(ctx, bundle.Experiment.ID); !errors.Is(err, optimization.ErrNotFound) {
				t.Fatalf("failed creation persisted: %v", err)
			}
			if (mode == "denied-child" || mode == "denied-parent" || mode == "missing-plan") && evaluator.calls != 0 {
				t.Fatal("unauthorized evaluator access")
			}
			if (mode == "revoked-child" || mode == "revoked-parent" || mode == "changed-caller") && evaluator.calls != 1 {
				t.Fatalf("read a second plan after authority changed: %d calls", evaluator.calls)
			}
		})
	}
	for _, mode := range []string{"objective", "repetitions", "scorers", "criteria"} {
		t.Run("ImprovementCreationRejectsIncompatible/"+mode, func(t *testing.T) {
			s := factory(t, time.Now)
			bundle, _ := improvementFixture(t)
			for i := range bundle.Experiment.Spec.Improvement.Children {
				child := &bundle.Experiment.Spec.Improvement.Children[i]
				switch mode {
				case "objective":
					child.Spec.Objective.MinImprovement += 0.01
				case "repetitions":
					child.Spec.Limits.Repetitions++
				case "scorers":
					child.EvaluationPlan.Scorers = append(child.EvaluationPlan.Scorers[:0:0], child.EvaluationPlan.Scorers...)
					child.EvaluationPlan.Scorers[0].Revision = "other"
				case "criteria":
					child.EvaluationPlan.Criteria = append(child.EvaluationPlan.Criteria[:0:0], child.EvaluationPlan.Criteria...)
					child.EvaluationPlan.Criteria[0].Required = true
				}
			}
			err := s.Transact(scoped(bundle.Experiment.Spec.Scope), func(tx store.Tx) error { return tx.CreateExperiment(bundle) })
			if !errors.Is(err, optimization.ErrInvalidSpec) {
				t.Fatalf("incompatible %s accepted: %v", mode, err)
			}
		})
	}
}
