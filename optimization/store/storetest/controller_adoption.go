package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

// ControllerAdoption checks explicit review and separation from execution routes.
func ControllerAdoption(t *testing.T, factory Factory) {
	t.Helper()
	f, _, _, decision := controllerDecisionFixture(t, factory, controllerDecisionOptions{qualified: true, improved: true})
	commitTrialChange(f.ctx, t, f.s, decisionChange(f.ctx, t, f.s, f.parent, f.leases[0], decision))
	authority := &controllerAdoptionAuthority{publicationAuthority: publicationAuthority{actor: optimization.IdentityRef{Subject: "reviewer"}}}
	compatibility := &controllerAdoptionCompatibility{}
	verifier, err := optimization.NewArtifactVerifier(unavailableActivationArtifacts{}, 1<<20, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	config := optimization.OptimizerConfig{Repository: f.s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: unusedPhase{}, Worker: optimization.DefaultWorkerConfig(), Compatibility: compatibility, ControllerCompatibility: compatibility, ArtifactVerifier: verifier}
	op, err := optimization.NewOptimizer(config)
	if err != nil {
		t.Fatal(err)
	}
	request := optimization.ActivationRequest{ID: id.New(optimization.PrefixActivation), AgentID: f.parent.Experiment.Spec.AgentID, Kind: optimization.HarnessImprovement, Version: decision.Candidate, DecisionID: decision.ID, Reason: "reviewed controller improvement"}
	if _, err = op.Activate(f.ctx, request); !errors.Is(err, optimization.ErrIncompleteEvidence) {
		t.Fatalf("adopted before review: %v", err)
	}
	finishControllerReview(t, f)
	authority.denied = true
	if _, err = op.Activate(f.ctx, request); !errors.Is(err, optimization.ErrPolicyDenied) {
		t.Fatalf("unauthorized adoption: %v", err)
	}
	authority.denied = false
	authority.child = f.children[0].Experiment.ID
	if _, err = op.Activate(f.ctx, request); !errors.Is(err, optimization.ErrPolicyDenied) {
		t.Fatalf("denied child adopted: %v", err)
	}
	authority.child = id.ID{}
	compatibility.incompatible = true
	if _, err = op.Activate(f.ctx, request); !errors.Is(err, optimization.ErrStaleVersion) {
		t.Fatalf("stale controller adopted: %v", err)
	}
	compatibility.incompatible = false
	compatibility.after = func() { authority.child = f.children[0].Experiment.ID }
	if _, err = op.Activate(f.ctx, request); !errors.Is(err, optimization.ErrPolicyDenied) {
		t.Fatalf("child revoked during compatibility: %v", err)
	}
	compatibility.after, authority.child = nil, id.ID{}
	execution := request
	execution.Kind = ""
	if _, err = op.Activate(f.ctx, execution); !errors.Is(err, optimization.ErrIncompleteEvidence) {
		t.Fatalf("controller entered execution route: %v", err)
	}
	active, err := op.Activate(f.ctx, request)
	if err != nil || active.Kind != optimization.HarnessImprovement || active.Generation != 1 {
		t.Fatalf("controller activation: %+v %v", active, err)
	}
	again, err := op.Activate(f.ctx, request)
	if err != nil || again.ID != active.ID {
		t.Fatalf("adoption retry: %+v %v", again, err)
	}
	if _, err = f.s.GetActivation(f.ctx, request.AgentID, ""); !errors.Is(err, optimization.ErrNotFound) {
		t.Fatalf("controller resolved as execution: %v", err)
	}
	resolved, err := f.s.GetActivationForKind(f.ctx, request.AgentID, "", optimization.HarnessImprovement)
	if err != nil || resolved.ID != active.ID {
		t.Fatalf("controller head: %+v %v", resolved, err)
	}
	history, err := f.s.ListActivationHistory(f.ctx, request.AgentID, "", optimization.PageQuery{Limit: 10})
	if err != nil || len(history.Items) != 0 {
		t.Fatalf("controller leaked into execution history: %+v %v", history, err)
	}
	history, err = f.s.ListActivationHistoryForKind(f.ctx, request.AgentID, "", optimization.HarnessImprovement, optimization.PageQuery{Limit: 10})
	if err != nil || len(history.Items) != 1 {
		t.Fatalf("controller history: %+v %v", history, err)
	}
	selection, err := op.ResolveActiveController(f.ctx, optimization.ActiveControllerRequest{AgentID: request.AgentID})
	if err != nil || selection.Activation.ID != active.ID || selection.Version.ID != decision.Candidate.ID || selection.Version.Kind != optimization.HarnessImprovement {
		t.Fatalf("controller resolution: %+v %v", selection, err)
	}
	foreign := scoped(Fixture(t, "foreign-controller-route").Experiment.Spec.Scope)
	if _, err = f.s.GetActivationForKind(foreign, request.AgentID, "", optimization.HarnessImprovement); !errors.Is(err, optimization.ErrNotFound) {
		t.Fatalf("foreign controller route: %v", err)
	}
	if _, err = f.s.GetActivationForKind(context.Background(), request.AgentID, "", optimization.HarnessImprovement); !errors.Is(err, cortex.ErrNoScope) {
		t.Fatalf("unscoped controller route: %v", err)
	}
	if _, err = f.s.ListActivationHistoryForKind(context.Background(), request.AgentID, "", optimization.HarnessImprovement, optimization.PageQuery{}); !errors.Is(err, cortex.ErrNoScope) {
		t.Fatalf("unscoped controller history: %v", err)
	}
	rollback := optimization.RollbackRequest{ID: id.New(optimization.PrefixActivation), AgentID: request.AgentID, Kind: optimization.HarnessImprovement, TargetID: active.ID, ExpectedGeneration: 1, Reason: "restore reviewed controller"}
	wrong := rollback
	wrong.Kind = ""
	if _, err = op.Rollback(f.ctx, wrong); !errors.Is(err, optimization.ErrIncompleteEvidence) {
		t.Fatalf("controller rollback crossed kind: %v", err)
	}
	rolled, err := op.Rollback(f.ctx, rollback)
	if err != nil || rolled.Generation != 2 || rolled.Kind != optimization.HarnessImprovement {
		t.Fatalf("controller rollback: %+v %v", rolled, err)
	}
	history, err = f.s.ListActivationHistoryForKind(f.ctx, request.AgentID, "", optimization.HarnessImprovement, optimization.PageQuery{Limit: 1})
	if err != nil || history.NextCursor == "" {
		t.Fatalf("controller history pagination: %v", err)
	}
	if _, err = f.s.ListActivationHistory(f.ctx, request.AgentID, "", optimization.PageQuery{Limit: 1, Cursor: history.NextCursor}); !errors.Is(err, optimization.ErrInvalidSpec) {
		t.Fatalf("controller cursor crossed kind: %v", err)
	}
	disabled, err := op.Deactivate(f.ctx, optimization.DeactivationRequest{ID: id.New(optimization.PrefixActivation), AgentID: request.AgentID, Kind: optimization.HarnessImprovement, ExpectedGeneration: 2, Reason: "disable controller"})
	if err != nil || disabled.Generation != 3 {
		t.Fatalf("controller deactivation: %+v %v", disabled, err)
	}
	if _, err = f.s.GetActivationForKind(f.ctx, request.AgentID, "", optimization.HarnessImprovement); !errors.Is(err, optimization.ErrInactiveActivation) {
		t.Fatalf("disabled controller resolves: %v", err)
	}
	if _, err = op.ResolveActiveController(f.ctx, optimization.ActiveControllerRequest{AgentID: request.AgentID}); !errors.Is(err, optimization.ErrInactiveActivation) {
		t.Fatalf("disabled controller selected: %v", err)
	}
	if selection.Activation.ID != active.ID || selection.Version.ID != decision.Candidate.ID {
		t.Fatal("activation update changed frozen selection")
	}
}

func finishControllerReview(t *testing.T, f validationFamily) {
	t.Helper()
	current, err := f.s.GetExperiment(f.ctx, f.parent.Experiment.ID)
	if err != nil {
		t.Fatal(err)
	}
	authority := controllerAuthority{actor: current.Spec.Requester}
	handler, err := optimization.NewImprovementPhaseHandler(optimization.OptimizerConfig{Repository: f.s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: unusedPhase{}, Worker: optimization.DefaultWorkerConfig()})
	if err != nil {
		t.Fatal(err)
	}
	finish, err := handler.Execute(f.ctx, optimization.WorkItem{Experiment: current, Lease: f.leases[0]})
	if err != nil || finish.NextState != optimization.StateReviewRequired || len(finish.Publications) != 0 {
		t.Fatalf("parent review: %+v %v", finish, err)
	}
	finish.ExperimentID, finish.ExpectedRevision, finish.Lease, finish.Actor, finish.IdempotencyKey = current.ID, current.Revision, f.leases[0], current.Spec.Requester, "controller-review"
	commitTrialChange(f.ctx, t, f.s, finish)
}

// ControllerAdoptionRecovery preserves controller routes and retry keys across reopen.
func ControllerAdoptionRecovery(t *testing.T, open func(*testing.T, func() time.Time) store.Store) {
	t.Helper()
	f, _, _, decision := controllerDecisionFixture(t, open, controllerDecisionOptions{qualified: true, improved: true})
	commitTrialChange(f.ctx, t, f.s, decisionChange(f.ctx, t, f.s, f.parent, f.leases[0], decision))
	finishControllerReview(t, f)
	request := optimization.ActivationChange{ID: id.New(optimization.PrefixActivation), AgentID: f.parent.Experiment.Spec.AgentID, Kind: optimization.HarnessImprovement, Version: decision.Candidate, DecisionID: decision.ID, PolicyDigest: decision.PolicyDigest, Actor: optimization.IdentityRef{Subject: "reviewer"}, Reason: "reviewed controller"}
	first := activate(f.ctx, t, f.s, request)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	f.s = open(t, time.Now)
	again := activate(f.ctx, t, f.s, request)
	if again.ID != first.ID || again.Generation != 1 || again.Kind != optimization.HarnessImprovement {
		t.Fatalf("changed controller retry: %+v", again)
	}
	if _, err := f.s.GetActivation(f.ctx, request.AgentID, ""); !errors.Is(err, optimization.ErrNotFound) {
		t.Fatalf("reopen changed route kind: %v", err)
	}
	current, err := f.s.GetActivationForKind(f.ctx, request.AgentID, "", optimization.HarnessImprovement)
	if err != nil || current.ID != first.ID {
		t.Fatalf("reopen lost controller: %+v %v", current, err)
	}
	stale := request
	stale.ID = id.New(optimization.PrefixActivation)
	if err = f.s.Transact(f.ctx, func(tx store.Tx) error { _, casErr := tx.CompareAndSwapActivation(stale); return casErr }); !errors.Is(err, optimization.ErrConflict) {
		t.Fatalf("stale controller generation: %v", err)
	}
	mixed := request
	mixed.Kind = optimization.HarnessExecution
	if err = f.s.Transact(f.ctx, func(tx store.Tx) error { _, casErr := tx.CompareAndSwapActivation(mixed); return casErr }); !errors.Is(err, optimization.ErrConflict) {
		t.Fatalf("retry changed route kind: %v", err)
	}
}

type controllerAdoptionAuthority struct {
	publicationAuthority
	child id.ID
}

func (a *controllerAdoptionAuthority) Authorize(ctx context.Context, action optimization.Action, resource optimization.Resource) error {
	if resource.ID == a.child && !a.child.IsNil() {
		return optimization.ErrPolicyDenied
	}
	return a.publicationAuthority.Authorize(ctx, action, resource)
}

// ControllerAdoptionArtifactDenial requires live child artifacts at the service boundary.
func ControllerAdoptionArtifactDenial(t *testing.T, factory Factory) {
	t.Helper()
	f, _, _, decision := controllerDecisionFixture(t, factory, controllerDecisionOptions{qualified: true, improved: true, artifacts: true})
	for index, child := range f.children {
		registerArtifact(f.ctx, t, f.s, artifactRegistration(child, f.leases[index+1], time.Now()))
	}
	commitTrialChange(f.ctx, t, f.s, decisionChange(f.ctx, t, f.s, f.parent, f.leases[0], decision))
	finishControllerReview(t, f)
	authority := controllerAuthority{actor: f.parent.Experiment.Spec.Requester}
	verifier, err := optimization.NewArtifactVerifier(unavailableActivationArtifacts{}, 1<<20, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: f.s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: unusedPhase{}, Worker: optimization.DefaultWorkerConfig(), ControllerCompatibility: &activationCompatibility{}, ArtifactVerifier: verifier})
	if err != nil {
		t.Fatal(err)
	}
	request := optimization.ActivationRequest{ID: id.New(optimization.PrefixActivation), AgentID: f.parent.Experiment.Spec.AgentID, Kind: optimization.HarnessImprovement, Version: decision.Candidate, DecisionID: decision.ID, Reason: "reviewed controller"}
	if _, err = op.Activate(f.ctx, request); !errors.Is(err, optimization.ErrIncompleteEvidence) {
		t.Fatalf("unavailable child artifact adopted: %v", err)
	}
	if _, err = f.s.GetActivationForKind(f.ctx, request.AgentID, "", optimization.HarnessImprovement); !errors.Is(err, optimization.ErrNotFound) {
		t.Fatalf("failed adoption changed route: %v", err)
	}
}

type controllerAdoptionCompatibility struct {
	activationCompatibility
	after func()
}

func (c *controllerAdoptionCompatibility) Check(ctx context.Context, version optimization.HarnessVersion) (optimization.CompatibilityReport, error) {
	report, err := c.activationCompatibility.Check(ctx, version)
	if c.after != nil {
		c.after()
	}
	return report, err
}
