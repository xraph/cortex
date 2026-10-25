package storetest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/scopespy"
)

type taskHandler struct {
	trials     *optimization.TrialPhaseHandler
	candidate  optimization.Candidate
	version    optimization.HarnessVersion
	repository store.Store
	artifacts  []optimization.Artifact
}

func (h taskHandler) Execute(ctx context.Context, item optimization.WorkItem) (optimization.Transition, error) {
	if item.Experiment.Phase == optimization.PhaseBaseline {
		if err := h.repository.Transact(ctx, func(tx optimization.Transaction) error {
			for _, artifact := range h.artifacts {
				if err := tx.RegisterArtifact(optimization.ArtifactRegistration{Ref: artifact.Ref, CreatedAt: artifact.CreatedAt, ExpiresAt: artifact.ExpiresAt, Actor: item.Experiment.Spec.Requester, Lease: item.Lease}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return optimization.Transition{}, err
		}
	}
	switch item.Experiment.Phase {
	case optimization.PhasePropose:
		candidate := h.candidate
		candidate.Round = item.Experiment.Round
		return optimization.Transition{NextState: optimization.StateRunning, NextPhase: optimization.PhaseProbe, Candidates: []optimization.Candidate{candidate}, Versions: []optimization.HarnessVersion{h.version}}, nil
	case optimization.PhaseReflect:
		decision, err := h.repository.GetDecision(ctx, item.Experiment.DecisionID)
		if err != nil {
			return optimization.Transition{}, err
		}
		publication := publicationRequest(optimization.ExperimentBundle{Experiment: item.Experiment}, item.Lease, decision)
		publication.Lesson.EvidenceClass = optimization.EvidenceContrastive
		return optimization.Transition{NextState: optimization.StateCompleted, Publications: []optimization.PublicationRequest{publication}}, nil
	default:
		return h.trials.Execute(ctx, item)
	}
}

type taskEvaluator struct {
	trialFixtureEvaluator
	baseline         string
	plan             optimization.EvaluationPlan
	requiresExpected bool
	freezeContext    func(context.Context)
}

func (e *taskEvaluator) Freeze(ctx context.Context, _ optimization.EvaluationSpec) (optimization.EvaluationPlan, error) {
	if e.freezeContext != nil {
		e.freezeContext(ctx)
	}
	plan := e.plan
	plan.Criteria = append([]optimization.CriterionDefinition(nil), plan.Criteria...)
	required := e.requiresExpected
	for i := range plan.Criteria {
		plan.Criteria[i].RequiresExpected = &required
	}
	return plan, nil
}

func taskBundle(t *testing.T) optimization.ExperimentBundle {
	t.Helper()
	bundle := candidateBundle(t)
	bundle.Experiment.Spec.Tasks = bundle.Experiment.Spec.Tasks[:1]
	bundle.Experiment.Spec.Limits.MaxRounds = 0
	bundle.Experiment.Spec.Limits.Repetitions = 1
	bundle.Experiment.Spec.Limits.MaxCandidates = 1
	bundle.Experiment.EvaluationPlan = fixtureEvaluationPlan(bundle.Experiment.Spec)
	requiresExpected := false
	bundle.Experiment.EvaluationPlan.Criteria[0].RequiresExpected = &requiresExpected
	return bundle
}

func taskCapabilities() optimization.IsolationCapabilities {
	return optimization.IsolationCapabilities{Isolated: true, Resettable: true, Recoverable: true, Stoppable: true, FrozenRetrieval: true, AtomicSources: true, DeterministicApproval: true}
}

type deadlineTaskHandler struct{ entered chan struct{} }

func (h deadlineTaskHandler) Execute(ctx context.Context, _ optimization.WorkItem) (optimization.Transition, error) {
	close(h.entered)
	<-ctx.Done()
	return optimization.Transition{}, ctx.Err()
}

type taskArtifacts struct {
	artifacts []optimization.Artifact
	onRead    func(optimization.ArtifactRef)
}

func (a *taskArtifacts) Get(_ context.Context, ref optimization.ArtifactRef) (optimization.Artifact, error) {
	if a.onRead != nil {
		a.onRead(ref)
	}
	for _, artifact := range a.artifacts {
		if artifact.Ref.Kind == ref.Kind {
			return artifact, nil
		}
	}
	return optimization.Artifact{}, optimization.ErrNotFound
}

type taskAuthority struct {
	publicationAuthority
	denyAdoption bool
}

func (a *taskAuthority) Authorize(ctx context.Context, action optimization.Action, resource optimization.Resource) error {
	if a.denyAdoption && action == optimization.ActionAdoptArtifacts {
		return optimization.ErrPolicyDenied
	}
	return a.publicationAuthority.Authorize(ctx, action, resource)
}

type taskAdopter struct {
	calls, effects int
	lost           bool
	requests       map[string]optimization.ArtifactAdoptionReceipt
}

func (a *taskAdopter) AdoptArtifacts(_ context.Context, r optimization.ArtifactAdoption) (optimization.ArtifactAdoptionReceipt, error) {
	a.calls++
	if prior, ok := a.requests[r.Request.Key]; ok {
		if prior.RequestDigest != r.RequestDigest {
			return optimization.ArtifactAdoptionReceipt{}, optimization.ErrConflict
		}
		return prior, nil
	}
	a.effects++
	receipt := optimization.ArtifactAdoptionReceipt{ID: r.Request.Key, RequestDigest: r.RequestDigest, Evidence: optimization.ContentRef{Ref: "host-commit", Revision: "1", Digest: r.RequestDigest}, AdoptedAt: time.Now().UTC()}
	a.requests[r.Request.Key] = receipt
	if a.lost {
		a.lost = false
		return optimization.ArtifactAdoptionReceipt{}, optimization.ErrUnknownAttempt
	}
	return receipt, nil
}

func (e *taskEvaluator) Evaluate(ctx context.Context, r optimization.EvaluationRequest) (optimization.Evaluation, error) {
	value, err := e.trialFixtureEvaluator.Evaluate(ctx, r)
	score := .8
	if r.Execution.PreparedDigest == e.baseline {
		score = .2
	}
	value.Score = &score
	value.Criteria[0].Score = &score
	return value, err
}

func taskLocal(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("TaskDeadlineIncludesEvaluatorQualification", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := taskBundle(t)
		bundle.Experiment.Spec.Limits.ExperimentTimeout = time.Second
		bundle.Experiment.Spec.Limits.TrialTimeout = time.Second
		authority := &publicationAuthority{actor: bundle.Experiment.Spec.Requester}
		started := time.Now()
		evaluator := &taskEvaluator{plan: bundle.Experiment.EvaluationPlan, requiresExpected: true, freezeContext: func(ctx context.Context) {
			deadline, ok := ctx.Deadline()
			if !ok || deadline.After(started.Add(2*time.Second)) {
				t.Errorf("evaluator qualification escaped total task deadline: %v %s", ok, deadline)
			}
		}}
		op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: unusedPhase{}, Worker: optimization.DefaultWorkerConfig(), TaskEvaluator: evaluator})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = op.OptimizeTask(scoped(bundle.Experiment.Spec.Scope), optimization.TaskSpec{Bundle: bundle, Isolation: taskCapabilities()}); err == nil {
			t.Fatal("answer-dependent evaluator accepted")
		}
	})
	t.Run("TaskRejectsRecursiveExecution", func(t *testing.T) {
		for _, purpose := range []run.ExecutionPurpose{run.PurposeOptimizationTrial, run.PurposeOptimizationController, run.PurposeEvaluation, run.PurposeNormal} {
			t.Run(string(purpose), func(t *testing.T) {
				s := factory(t, time.Now)
				bundle := taskBundle(t)
				ctx := scoped(bundle.Experiment.Spec.Scope)
				authority := &publicationAuthority{actor: bundle.Experiment.Spec.Requester}
				op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: unusedPhase{}, Worker: optimization.DefaultWorkerConfig()})
				if err != nil {
					t.Fatal(err)
				}
				called := false
				runtime, err := engine.New(engine.WithStore(scopespy.New()), engine.WithLLM(&activePauseModel{}), engine.WithModelRevision("fixture", "1"), engine.WithToolRevision("external", "1"), engine.WithTool(llm.Tool{Name: "external"}, func(toolCtx context.Context, _ cortex.Invocation) (string, error) {
					called = true
					_, callErr := op.OptimizeTask(toolCtx, optimization.TaskSpec{})
					if purpose == run.PurposeNormal {
						if errors.Is(callErr, optimization.ErrPolicyDenied) {
							t.Error("normal execution refused as recursive")
						}
					} else if !errors.Is(callErr, optimization.ErrPolicyDenied) {
						t.Errorf("recursive task admitted: %v", callErr)
					}
					return "checked", nil
				}))
				if err != nil {
					t.Fatal(err)
				}
				prepared, err := runtime.PrepareAgent(ctx, bundle.Experiment.Spec.AgentID, &engine.RunOverrides{SystemPrompt: "fixture", Model: "fixture", Tools: []string{"external"}, ToolsRestricted: true, MaxSteps: 3})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = runtime.RunPrepared(ctx, prepared, "work", engine.PreparedRunOptions{Purpose: purpose}); err != nil || !called {
					t.Fatalf("recursive boundary was not exercised: called=%v err=%v", called, err)
				}
				page, err := s.ListExperiments(ctx, optimization.ListQuery{Limit: 100})
				if err != nil || len(page.Items) != 0 {
					t.Fatalf("recursive/invalid task persisted: %+v %v", page, err)
				}
			})
		}
	})
	t.Run("TaskReturnsChosenOutputWithoutRerun", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := taskBundle(t)
		candidate, version := candidateVersion(t, bundle, "Check the captured evidence.")
		ctx := scoped(bundle.Experiment.Spec.Scope)
		authority := &taskAuthority{publicationAuthority: publicationAuthority{actor: bundle.Experiment.Spec.Requester}}
		data := []byte("task output artifact")
		artifact := optimization.Artifact{Ref: optimization.ArtifactRef{ExperimentID: bundle.Experiment.ID, Scope: bundle.Experiment.Spec.Scope, Digest: fmt.Sprintf("%x", sha256.Sum256(data)), Size: int64(len(data)), MediaType: "text/plain", Kind: "output", AccessPolicy: "restricted"}, Data: data, CreatedAt: time.Now().UTC().Add(-time.Second), ExpiresAt: time.Now().UTC().Add(time.Hour)}
		baselineArtifact := artifact
		baselineArtifact.Ref.Kind = "baseline-output"
		artifacts := &taskArtifacts{artifacts: []optimization.Artifact{baselineArtifact, artifact}}
		environments := &trialFixtureFactory{t: t, bundle: bundle, environments: map[id.ID]*trialFixtureEnvironment{}, sessions: map[id.ID]bool{}, artifacts: []optimization.ArtifactRef{artifact.Ref}, artifactsByPrepared: map[string][]optimization.ArtifactRef{bundle.Versions[0].Prepared.Digest(): {baselineArtifact.Ref}}}
		evaluator := &taskEvaluator{baseline: bundle.Versions[0].Prepared.Digest(), plan: bundle.Experiment.EvaluationPlan}
		handler, err := optimization.NewTrialPhaseHandler(optimization.TrialConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Environments: environments, Evaluator: evaluator, Allowance: optimization.TrialAllowance{MaxTokens: 100}})
		if err != nil {
			t.Fatal(err)
		}
		verifier, err := optimization.NewArtifactVerifier(artifacts, 1<<20, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		adopter := &taskAdopter{lost: true, requests: map[string]optimization.ArtifactAdoptionReceipt{}}
		op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: taskHandler{trials: handler, candidate: candidate, version: version, repository: s, artifacts: artifacts.artifacts}, Worker: optimization.DefaultWorkerConfig(), Compatibility: &activationCompatibility{}, ArtifactVerifier: verifier, TaskEvaluator: evaluator}, optimization.WithArtifactAdopter(adopter))
		if err != nil {
			t.Fatal(err)
		}
		policy := optimization.ContentRef{Ref: "artifact-adoption", Revision: "1", Digest: artifact.Ref.Digest}
		request := optimization.TaskSpec{Bundle: bundle, Isolation: taskCapabilities(), AdoptionPolicy: &policy}
		evaluator.requiresExpected = true
		if _, callErr := op.OptimizeTask(ctx, request); callErr == nil || environments.calls != 0 {
			t.Fatalf("forged expected-answer capability dispatched: %v", callErr)
		}
		if _, lookupErr := s.GetExperiment(ctx, bundle.Experiment.ID); !errors.Is(lookupErr, optimization.ErrNotFound) {
			t.Fatalf("unqualified plan persisted a task: %v", lookupErr)
		}
		evaluator.requiresExpected = false
		for _, kind := range []string{"nonrepeatable", "expected", "unknown"} {
			t.Run(kind, func(t *testing.T) {
				invalid := request
				switch kind {
				case "nonrepeatable":
					invalid.Isolation.Resettable = false
				case "expected":
					*bundle.Experiment.EvaluationPlan.Criteria[0].RequiresExpected = true
				case "unknown":
					invalid.Bundle.Experiment.EvaluationPlan.Criteria = []optimization.CriterionDefinition{{ID: "quality"}}
				}
				if _, callErr := op.OptimizeTask(ctx, invalid); callErr == nil || environments.calls != 0 {
					t.Fatalf("unsupported task dispatched: calls=%d err=%v", environments.calls, callErr)
				}
				*bundle.Experiment.EvaluationPlan.Criteria[0].RequiresExpected = false
			})
		}
		result, err := op.OptimizeTask(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if result.Trial.Version != candidate.Version || result.Output != result.Trial.Execution.Outcome.Output || result.Decision.TaskSelection == nil || result.Decision.TaskSelection.TrialID != result.Trial.ID || result.EvidenceClass != optimization.EvidenceContrastive || len(result.Evidence) != 2 || environments.calls != 2 {
			t.Fatalf("task result not captured selection: %+v calls=%d", result, environments.calls)
		}
		saved, err := s.GetExperiment(ctx, result.ExperimentID)
		if err != nil || saved.State != optimization.StateCompleted || saved.Spec.Mode != optimization.ModeTaskLocal || saved.Spec.Limits.MaxRounds != 1 || saved.Spec.Task == nil {
			t.Fatalf("task defaults or completion: %+v %v", saved, err)
		}
		taskSelectionChecks(t, saved, result)
		publications, err := s.ListPublications(ctx, optimization.PublicationQuery{})
		if err != nil || len(publications.Items) != 1 || publications.Items[0].Payload.EvidenceClass != optimization.EvidenceContrastive {
			t.Fatalf("task reflection lost contrastive provenance: %+v %v", publications, err)
		}
		if _, err = op.GetTaskResult(ctx, result.ExperimentID); err != nil || environments.calls != 2 {
			t.Fatalf("reading task result reran execution: %v", err)
		}
		if adopter.calls != 0 {
			t.Fatal("task selection adopted artifacts implicitly")
		}
		adoption := optimization.ArtifactAdoptionRequest{Key: "adopt-task-output", ExperimentID: result.ExperimentID, DecisionID: result.Decision.ID, TrialID: result.Trial.ID, Destination: optimization.ContentRef{Ref: "workspace-file", Revision: "1", Digest: artifact.Ref.Digest}, Policy: policy, Artifacts: result.Artifacts}
		authority.denyAdoption = true
		if _, err = op.AdoptArtifacts(ctx, adoption); !errors.Is(err, optimization.ErrPolicyDenied) || adopter.calls != 0 {
			t.Fatalf("artifact adoption lacked host permission: %v", err)
		}
		authority.denyAdoption = false
		for _, kind := range []string{"policy", "trial", "artifact"} {
			invalid := adoption
			switch kind {
			case "policy":
				invalid.Policy.Revision = "stale"
			case "trial":
				invalid.TrialID = id.New(optimization.PrefixTrial)
			case "artifact":
				invalid.Artifacts = append([]optimization.ArtifactRef(nil), adoption.Artifacts...)
				invalid.Artifacts[0].Kind = "foreign"
			}
			if _, err = op.AdoptArtifacts(ctx, invalid); err == nil || adopter.calls != 0 {
				t.Fatalf("adopted invalid %s: %v", kind, err)
			}
		}
		if _, err = op.AdoptArtifacts(ctx, adoption); !errors.Is(err, optimization.ErrUnknownAttempt) || adopter.effects != 1 {
			t.Fatalf("lost adoption acknowledgment: %v", err)
		}
		receipt, err := op.AdoptArtifacts(ctx, adoption)
		if err != nil || receipt.RequestDigest == "" || adopter.effects != 1 || environments.calls != 2 {
			t.Fatalf("adoption retry duplicated effects or trial: %+v %v", receipt, err)
		}
		adoption.Destination.Revision = "changed"
		if _, err = op.AdoptArtifacts(ctx, adoption); !errors.Is(err, optimization.ErrConflict) || adopter.effects != 1 {
			t.Fatalf("adoption key accepted changed destination: %v", err)
		}
		activation := optimization.ActivationRequest{ID: id.New(optimization.PrefixActivation), AgentID: saved.Spec.AgentID, Version: result.Trial.Version, DecisionID: result.Decision.ID, Reason: "task result cannot activate"}
		if _, err = op.Activate(ctx, activation); err == nil {
			t.Fatal("task-local result activated a global harness")
		}
		authority.denied = true
		if _, err = op.GetTaskResult(ctx, result.ExperimentID); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("revoked result access: %v", err)
		}
		authority.denied = false
		deletion := optimization.ArtifactDeletion{Ref: baselineArtifact.Ref, RequestID: "withdraw-task-evidence", Actor: authority.actor, Reason: "source withdrawn"}
		withdrawn := false
		artifacts.onRead = func(ref optimization.ArtifactRef) {
			if ref.Kind == artifact.Ref.Kind && !withdrawn {
				withdrawn = true
				if deletionErr := s.Transact(ctx, func(tx optimization.Transaction) error { return tx.BeginArtifactDeletion(deletion) }); deletionErr != nil {
					t.Fatal(deletionErr)
				}
			}
		}
		if _, err = op.GetTaskResult(ctx, result.ExperimentID); err == nil || !withdrawn {
			t.Fatalf("earlier evidence withdrawn during later content read remained eligible: %v", err)
		}
		calls := adopter.calls
		if _, err = op.AdoptArtifacts(ctx, adoption); err == nil || adopter.calls != calls {
			t.Fatalf("withdrawn task evidence adopted: %v", err)
		}
	})
	t.Run("TaskCallerCancellationStopsDurableWork", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := taskBundle(t)
		authority := &publicationAuthority{actor: bundle.Experiment.Spec.Requester}
		entered := make(chan struct{})
		op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: deadlineTaskHandler{entered}, Worker: optimization.DefaultWorkerConfig(), TaskEvaluator: &taskEvaluator{plan: bundle.Experiment.EvaluationPlan}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(scoped(bundle.Experiment.Spec.Scope), 10*time.Second)
		defer cancel()
		deadline, _ := ctx.Deadline()
		done := make(chan error, 1)
		go func() {
			_, callErr := op.OptimizeTask(ctx, optimization.TaskSpec{Bundle: bundle, Isolation: taskCapabilities()})
			done <- callErr
		}()
		select {
		case <-entered:
		case callErr := <-done:
			t.Fatalf("task never started: %v", callErr)
		case <-ctx.Done():
			t.Fatal("task did not reach phase handler")
		}
		current, err := s.GetExperiment(ctx, bundle.Experiment.ID)
		if err != nil || current.Deadline.After(deadline) {
			cancel()
			<-done
			t.Fatalf("durable task outlived caller deadline: %s > %s: %v", current.Deadline, deadline, err)
		}
		cancel()
		if err = <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("task cancellation: %v", err)
		}
		current, err = s.GetExperiment(scoped(bundle.Experiment.Spec.Scope), bundle.Experiment.ID)
		if err != nil || current.State != optimization.StateCancelled {
			t.Fatalf("caller cancellation left executable task: %+v %v", current, err)
		}
	})
}

func taskSelectionChecks(t *testing.T, experiment optimization.Experiment, original *optimization.TaskResult) {
	t.Helper()
	body, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	copyResult := func(t *testing.T) optimization.TaskResult {
		t.Helper()
		var value optimization.TaskResult
		if decodeErr := json.Unmarshal(body, &value); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		return value
	}
	for _, kind := range []string{"selected-trial", "score", "activation", "missing-evidence"} {
		t.Run("tampered-"+kind, func(t *testing.T) {
			value := copyResult(t)
			switch kind {
			case "selected-trial":
				value.Decision.TaskSelection.TrialID = id.New(optimization.PrefixTrial)
			case "score":
				value.Decision.TaskSelection.Score = 1
			case "activation":
				value.Decision.Outcome = optimization.DecisionRecommend
			case "missing-evidence":
				value.Evidence = value.Evidence[:1]
			}
			if validationErr := optimization.ValidateTaskDecision(experiment, value.Decision, value.Evidence); validationErr == nil {
				t.Fatal("tampered task decision accepted")
			}
		})
	}
	t.Run("completion-order", func(t *testing.T) {
		value := copyResult(t)
		slices.Reverse(value.Evidence)
		if validationErr := optimization.ValidateTaskDecision(experiment, value.Decision, value.Evidence); validationErr != nil {
			t.Fatal(validationErr)
		}
	})
	for _, kind := range []string{"failed", "blocked", "tie"} {
		t.Run("retain-baseline-"+kind, func(t *testing.T) {
			value := copyResult(t)
			for i := range value.Evidence {
				probe := &value.Evidence[i]
				if probe.Trial.Version == experiment.Spec.Baseline {
					continue
				}
				switch kind {
				case "failed":
					probe.Trial.State = optimization.TrialFailed
					probe.Trial.Execution.State = run.StateFailed
					probe.Trial.Execution.Outcome.StopReason = run.StopExecutionError
					sealed, sealErr := optimization.SealExecution(*probe.Trial.Execution)
					if sealErr != nil {
						t.Fatal(sealErr)
					}
					probe.Trial.Execution = &sealed
					probe.Evaluation.ExecutionDigest = sealed.Digest
					probe.Evaluation.Complete = false
					probe.Evaluation.Score = nil
				case "blocked":
					probe.Trial.State, probe.Trial.Error = optimization.TrialBlocked, "approval unavailable"
					probe.Trial.Execution, probe.Trial.EvaluationID, probe.Evaluation = nil, "", nil
				case "tie":
					score := .2
					probe.Evaluation.Score, probe.Evaluation.Criteria[0].Score = &score, &score
				}
			}
			decision, selectionErr := optimization.BuildTaskDecision(experiment, value.Evidence)
			if selectionErr != nil || decision.TaskSelection == nil || decision.Candidate != experiment.Spec.Baseline || len(value.Evidence) != 2 {
				t.Fatalf("baseline fallback lost outcomes: %+v %v", decision, selectionErr)
			}
		})
	}
}
