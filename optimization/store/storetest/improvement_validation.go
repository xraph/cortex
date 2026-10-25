package storetest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

func improvementValidation(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("ImprovementStreamCannotRepeatHeldOutTasks", func(t *testing.T) {
		parent, _ := improvementFixture(t)
		plan := parent.Experiment.Spec.Improvement
		for _, original := range append([]optimization.ImprovementChild(nil), plan.Children...) {
			child := original
			child.ExperimentID = id.New(optimization.PrefixExperiment)
			link := *child.Spec.Parent
			link.Sequence = 1
			child.Spec.Parent = &link
			plan.Children = append(plan.Children, child)
		}
		if err := optimization.ValidateSpec(parent.Experiment.Spec); !errors.Is(err, optimization.ErrInvalidSpec) {
			t.Fatalf("task stream repeats holdout across positions: %v", err)
		}
	})
	t.Run("ImprovementPlanCountsBothControllerArms", func(t *testing.T) {
		parent, children := improvementFixture(t)
		childPlan, err := optimization.Preflight(children[0].Experiment.Spec)
		if err != nil {
			t.Fatal(err)
		}
		parentPlan, err := optimization.Preflight(parent.Experiment.Spec)
		if err != nil || parentPlan.DiscoveryTrials != childPlan.DiscoveryTrials*2 || parentPlan.ValidationTrials != childPlan.ValidationTrials*2 || parentPlan.TotalTrials != childPlan.TotalTrials*2 || parentPlan.DistinctValidationCases != childPlan.DistinctValidationCases {
			t.Fatalf("parent trial plan undercounts arms: %+v %+v %v", parentPlan, childPlan, err)
		}
	})

	t.Run("ImprovementChildWorkerUsesAssignedCasesAndWaits", func(t *testing.T) {
		f := frozenValidationFamily(t, factory, 2)
		allocation := allocate(f.ctx, t, f.s, f.request)
		authority := controllerAuthority{actor: f.parent.Experiment.Spec.Requester}
		environments := &trialFixtureFactory{t: t}
		evaluator := &trialFixtureEvaluator{}
		handler, err := optimization.NewTrialPhaseHandler(optimization.TrialConfig{Repository: f.s, Authorizer: authority, IdentityResolver: authority, Environments: environments, Evaluator: evaluator, Allowance: optimization.TrialAllowance{MaxTokens: 10}})
		if err != nil {
			t.Fatal(err)
		}
		for i, child := range f.children {
			current, getErr := f.s.GetExperiment(f.ctx, child.Experiment.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			change, executeErr := handler.Execute(f.ctx, optimization.WorkItem{Experiment: current, Lease: f.leases[i+1]})
			if executeErr != nil || len(change.Trials) != child.Experiment.Spec.Limits.Repetitions {
				t.Fatalf("child scheduling: %d %v", len(change.Trials), executeErr)
			}
			for _, trial := range change.Trials {
				if trial.AllocationID != allocation.ID || trial.CaseDigest != child.Experiment.Spec.Tasks[1].Digest {
					t.Fatalf("wrong assigned trial: %+v", trial)
				}
			}
			fillVersionTrials(f.ctx, t, f.s, child, f.leases[i+1], child.Experiment.Spec.Baseline, id.ID{}, optimization.SplitValidation, allocation.ID, nil)
			current, getErr = f.s.GetExperiment(f.ctx, child.Experiment.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			change, executeErr = handler.Execute(f.ctx, optimization.WorkItem{Experiment: current, Lease: f.leases[i+1]})
			if i < len(f.children)-1 {
				if !errors.Is(executeErr, optimization.ErrConflict) {
					t.Fatalf("child completed before full stream: %+v %v", change, executeErr)
				}
			} else if executeErr != nil || change.NextPhase != optimization.PhaseDecide {
				t.Fatalf("completed stream stayed blocked: %+v %v", change, executeErr)
			}
		}
		if environments.calls != 0 || evaluator.calls != 0 {
			t.Fatal("scheduling or barrier replayed captured execution")
		}
	})

	t.Run("ImprovementPairedStreamWithholdsAllFeedback", func(t *testing.T) {
		f := frozenValidationFamily(t, factory, 2)
		allocation := allocate(f.ctx, t, f.s, f.request)
		if len(allocation.PairedRoster) != 4 || len(allocation.CaseDigests) != 2 {
			t.Fatalf("incomplete stream allocation: %+v", allocation)
		}
		for i, arm := range allocation.PairedRoster {
			if len(arm.CaseDigests) != 1 || arm.CaseDigests[0] != f.children[i].Experiment.Spec.Tasks[1].Digest {
				t.Fatalf("wrong case assignment: %+v", arm)
			}
		}
		if err := f.s.Transact(f.ctx, func(tx store.Tx) error {
			return tx.ExposeValidation(optimization.ValidationAccess{AllocationID: allocation.ID, ExperimentID: f.children[0].Experiment.ID, Lease: f.leases[1]})
		}); err != nil {
			t.Fatal(err)
		}
		for i, child := range f.children {
			fillVersionTrials(f.ctx, t, f.s, child, f.leases[i+1], child.Experiment.Spec.Baseline, id.ID{}, optimization.SplitValidation, allocation.ID, nil)
			if i < len(f.children)-1 {
				for j := 0; j <= i; j++ {
					assertPhaseRefused(f.ctx, t, f.s, f.children[j], f.leases[j+1], optimization.PhaseDecide)
				}
				assertPhaseRefused(f.ctx, t, f.s, f.parent, f.leases[0], optimization.PhaseDecide)
			}
		}
		for i, child := range f.children {
			advanceFixture(f.ctx, t, f.s, child, f.leases[i+1], optimization.PhaseDecide)
		}
		advanceFixture(f.ctx, t, f.s, f.parent, f.leases[0], optimization.PhaseDecide)
	})
	t.Run("ImprovementChildrenCannotAllocateTheirOwnHoldout", func(t *testing.T) {
		f := frozenValidationFamily(t, factory, 1)
		for i, child := range f.children {
			if err := f.s.Transact(f.ctx, func(tx store.Tx) error {
				_, allocationErr := tx.AllocateValidation(allocationRequest(child, f.leases[i+1]))
				return allocationErr
			}); !errors.Is(err, optimization.ErrPolicyDenied) {
				t.Fatalf("child allocated private holdout: %v", err)
			}
		}
	})
	for _, mode := range []string{"missing-child", "changed-controller", "changed-assignment", "reordered", "repeated-child"} {
		t.Run("ImprovementValidationFrozenRoster/"+mode, func(t *testing.T) {
			f := frozenValidationFamily(t, factory, 2)
			request := f.request
			request.PairedRoster = append([]optimization.ValidationArm(nil), request.PairedRoster...)
			switch mode {
			case "missing-child":
				request.PairedRoster = request.PairedRoster[:2]
			case "changed-controller":
				request.PairedRoster[1].Controller = request.PairedRoster[0].Controller
			case "changed-assignment":
				request.PairedRoster[0].CaseDigests = []string{f.children[2].Experiment.Spec.Tasks[1].Digest}
			case "reordered":
				request.PairedRoster[0], request.PairedRoster[1] = request.PairedRoster[1], request.PairedRoster[0]
			case "repeated-child":
				request.PairedRoster[1] = request.PairedRoster[0]
			}
			if err := f.s.Transact(f.ctx, func(tx store.Tx) error { _, allocationErr := tx.AllocateValidation(request); return allocationErr }); !errors.Is(err, optimization.ErrPolicyDenied) {
				t.Fatalf("%s roster accepted: %v", mode, err)
			}
		})
	}

	t.Run("ImprovementValidationRejectsUnrelatedChildren", func(t *testing.T) {
		s := factory(t, time.Now)
		parent := Fixture(t, "unrelated-pair")
		ctx := scoped(parent.Experiment.Spec.Scope)
		parentLease := validationExperiment(ctx, t, s, parent)
		request := allocationRequest(parent, parentLease)
		request.PairedRoster = nil
		for range 2 {
			child := parent
			child.Experiment.ID = id.New(optimization.PrefixExperiment)
			child.Versions = nil
			lease := validationExperiment(ctx, t, s, child)
			request.PairedRoster = append(request.PairedRoster, allocationRequest(child, lease).PairedRoster[0])
		}
		err := s.Transact(ctx, func(tx store.Tx) error { _, allocationErr := tx.AllocateValidation(request); return allocationErr })
		if !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("unrelated paired roster accepted: %v", err)
		}
	})
}

type validationFamily struct {
	s        store.Store
	ctx      context.Context
	parent   optimization.ExperimentBundle
	children []optimization.ExperimentBundle
	leases   []optimization.WorkLease
	request  optimization.ValidationAllocationRequest
}

func validationFamilyBundles(t *testing.T, pairs int) (parent optimization.ExperimentBundle, children []optimization.ExperimentBundle) {
	t.Helper()
	parent, initial := improvementFixture(t)
	children = append([]optimization.ExperimentBundle(nil), initial...)
	parent.Experiment.Spec.Tasks = append([]optimization.TaskCase(nil), parent.Experiment.Spec.Tasks...)
	for sequence := 1; sequence < pairs; sequence++ {
		for arm := range 2 {
			child := initial[arm]
			child.Experiment.ID = id.New(optimization.PrefixExperiment)
			child.Experiment.Spec.Parent = &optimization.ParentExperiment{ExperimentID: parent.Experiment.ID, Arm: arm, Sequence: sequence}
			child.Experiment.Spec.Tasks = append([]optimization.TaskCase(nil), initial[arm].Experiment.Spec.Tasks...)
			for index, task := range child.Experiment.Spec.Tasks {
				task.ID += fmt.Sprintf("-task-%d", sequence)
				task.Input += fmt.Sprintf(" task %d", sequence)
				var err error
				child.Experiment.Spec.Tasks[index], err = optimization.SealTaskCase(task)
				if err != nil {
					t.Fatal(err)
				}
			}
			child.Experiment.Spec.Evaluation.Revision = fmt.Sprintf("task-%d", sequence)
			child.Experiment.EvaluationPlan = fixtureEvaluationPlan(child.Experiment.Spec)
			children = append(children, child)
			if arm == 0 {
				parent.Experiment.Spec.Tasks = append(parent.Experiment.Spec.Tasks, child.Experiment.Spec.Tasks...)
			}
		}
	}
	parent.Experiment.Spec.Evaluation.Revision = "parent-stream"
	parent.Experiment.EvaluationPlan = fixtureEvaluationPlan(parent.Experiment.Spec)
	parent.Experiment.Spec.Improvement.Children = nil
	for _, child := range children {
		parent.Experiment.Spec.Improvement.Children = append(parent.Experiment.Spec.Improvement.Children, optimization.ImprovementChild{ExperimentID: child.Experiment.ID, Spec: child.Experiment.Spec, EvaluationPlan: child.Experiment.EvaluationPlan})
	}
	return parent, children
}

func frozenValidationFamily(t *testing.T, factory Factory, pairs int) validationFamily {
	t.Helper()
	parent, children := validationFamilyBundles(t, pairs)
	f := validationFamily{s: factory(t, time.Now), ctx: scoped(parent.Experiment.Spec.Scope), parent: parent, children: children, leases: make([]optimization.WorkLease, len(children)+1)}
	if err := f.s.Transact(f.ctx, func(tx store.Tx) error {
		var err error
		f.leases[0], err = startFixture(tx, parent, "parent")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for i := range f.children {
		f.children[i].ParentLease = &f.leases[0]
		f.leases[i+1] = validationExperiment(f.ctx, t, f.s, f.children[i])
		if err := f.s.Transact(f.ctx, func(tx store.Tx) error {
			_, err := tx.PutDiscoveryLessons(optimization.DiscoveryLessonsRequest{ExperimentID: f.children[i].Experiment.ID, Lease: f.leases[i+1], Domain: "comparison", Lessons: []optimization.LessonDraft{}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	advanceFixture(f.ctx, t, f.s, parent, f.leases[0], optimization.PhaseValidate)
	f.request = allocationRequest(parent, f.leases[0])
	f.request.PairedRoster = nil
	for i, child := range f.children {
		f.request.PairedRoster = append(f.request.PairedRoster, allocationRequest(child, f.leases[i+1]).PairedRoster[0])
	}
	return f
}

// ImprovementValidationRecovery resumes the exact paired stream after reopening storage.
func ImprovementValidationRecovery(t *testing.T, open func(*testing.T, func() time.Time) store.Store) {
	t.Helper()
	now := time.Now().UTC()
	f := frozenValidationFamily(t, func(t *testing.T, _ func() time.Time) store.Store { return open(t, func() time.Time { return now }) }, 2)
	allocation := allocate(f.ctx, t, f.s, f.request)
	if err := f.s.Transact(f.ctx, func(tx store.Tx) error {
		return tx.ExposeValidation(optimization.ValidationAccess{AllocationID: allocation.ID, ExperimentID: f.children[0].Experiment.ID, Lease: f.leases[1]})
	}); err != nil {
		t.Fatal(err)
	}
	fillVersionTrials(f.ctx, t, f.s, f.children[0], f.leases[1], f.children[0].Experiment.Spec.Baseline, id.ID{}, optimization.SplitValidation, allocation.ID, nil)
	prior, err := f.s.GetValidationAllocation(f.ctx, allocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.Close(); err != nil {
		t.Fatal(err)
	}
	now = f.leases[0].ExpiresAt.Add(time.Second)
	f.s = open(t, func() time.Time { return now })
	if err = f.s.Transact(f.ctx, func(tx store.Tx) error {
		var claimErr error
		f.leases[0], claimErr = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: f.parent.Experiment.ID, Owner: "recovered-parent", Duration: time.Minute})
		if claimErr != nil {
			return claimErr
		}
		for i, child := range f.children {
			f.leases[i+1], claimErr = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: child.Experiment.ID, Owner: "recovered-child", Duration: time.Minute})
			if claimErr != nil {
				return claimErr
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.request.Lease = f.leases[0]
	recovered := allocate(f.ctx, t, f.s, f.request)
	if recovered.ID != prior.ID || recovered.Revision != prior.Revision || !recovered.ExposedAt.Equal(prior.ExposedAt) || !reflect.DeepEqual(recovered.PairedRoster, prior.PairedRoster) {
		t.Fatalf("recovery changed frozen allocation: %+v", recovered)
	}
	assertPhaseRefused(f.ctx, t, f.s, f.children[0], f.leases[1], optimization.PhaseDecide)
	for i := 1; i < len(f.children); i++ {
		fillVersionTrials(f.ctx, t, f.s, f.children[i], f.leases[i+1], f.children[i].Experiment.Spec.Baseline, id.ID{}, optimization.SplitValidation, allocation.ID, nil)
	}
	for i, child := range f.children {
		advanceFixture(f.ctx, t, f.s, child, f.leases[i+1], optimization.PhaseDecide)
	}
	advanceFixture(f.ctx, t, f.s, f.parent, f.leases[0], optimization.PhaseDecide)
	if err = f.s.Close(); err != nil {
		t.Fatal(err)
	}
	final := open(t, func() time.Time { return now })
	saved, err := final.GetValidationAllocation(f.ctx, allocation.ID)
	if err != nil || !reflect.DeepEqual(saved, prior) {
		t.Fatalf("completed stream changed allocation: %+v %v", saved, err)
	}
	for _, child := range f.children {
		current, getErr := final.GetExperiment(f.ctx, child.Experiment.ID)
		if getErr != nil || current.Phase != optimization.PhaseDecide {
			t.Fatalf("child resume lost: %+v %v", current, getErr)
		}
	}
}
