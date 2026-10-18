// Package storetest runs the same optimization storage checks against each backend.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/store/scopespy"
)

// Factory returns an empty backend and registers its cleanup with the test.
type Factory func(*testing.T, func() time.Time) store.Store

// Run checks store semantics with populated records and exact identity assertions.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	lifecycle(t, factory)
	leases(t, factory)
	accounting(t, factory)
	validation(t, factory)
	candidates(t, factory)
	trials(t, factory)
	trialEvidence(t, factory)
	t.Run("ScopeIsolation", func(t *testing.T) {
		s := factory(t, time.Now)
		a, b := Fixture(t, "a"), Fixture(t, "b")
		for _, bundle := range []optimization.ExperimentBundle{a, b} {
			ctx := scoped(bundle.Experiment.Spec.Scope)
			if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.GetExperiment(scoped(a.Experiment.Spec.Scope), b.Experiment.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("foreign ID read: %v", err)
		}
		ancestor := a.Experiment.Spec.Scope
		ancestor.Levels = ancestor.Levels[:2]
		if _, err := s.GetExperiment(scoped(ancestor), a.Experiment.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("ancestor read: %v", err)
		}
		if err := s.Transact(scoped(b.Experiment.Spec.Scope), func(tx store.Tx) error { return tx.CreateExperiment(a) }); err == nil {
			t.Fatal("foreign scope write accepted")
		}
		if _, err := s.GetExperiment(context.Background(), a.Experiment.ID); !errors.Is(err, cortex.ErrNoScope) {
			t.Fatalf("missing scope read: %v", err)
		}
		for _, scope := range []cortex.Scope{
			{Levels: []cortex.Level{{Key: "org", Value: "a"}, {Key: "workspace", Value: "w"}, {Key: "project", Value: "p"}, {Key: "fourth", Value: "x"}}},
			{Levels: []cortex.Level{{Key: "org", Value: "a"}, {Key: "org", Value: "b"}}},
			{Levels: []cortex.Level{{Key: "org", Value: "a/project=b"}}},
		} {
			if _, err := s.GetExperiment(scoped(scope), a.Experiment.ID); err == nil {
				t.Fatal("malformed scope admitted")
			}
		}
	})
	t.Run("TransactionRollback", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := Fixture(t, "rollback")
		ctx := scoped(bundle.Experiment.Spec.Scope)
		aborted := errors.New("abort fixture")
		err := s.Transact(ctx, func(tx store.Tx) error {
			if createErr := tx.CreateExperiment(bundle); createErr != nil {
				return createErr
			}
			return aborted
		})
		if !errors.Is(err, aborted) {
			t.Fatalf("wrong rollback error: %v", err)
		}
		if _, err = s.GetExperiment(ctx, bundle.Experiment.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("rolled-back experiment remains: %v", err)
		}
		if _, err = s.GetVersion(ctx, bundle.Experiment.Spec.Baseline.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("rolled-back version remains: %v", err)
		}
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("DetachedReadsAndWrites", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := Fixture(t, "copy")
		ctx := scoped(bundle.Experiment.Spec.Scope)
		wantID, wantVersion := bundle.Experiment.ID, bundle.Experiment.Spec.Baseline
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
			t.Fatal(err)
		}
		bundle.Experiment.Spec.Tasks[0].Input = "caller changed input"
		got, err := s.GetExperiment(ctx, wantID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != wantID || got.Spec.Tasks[0].Input != "Discovery task" || got.Revision != 1 || got.State != optimization.StateDraft {
			t.Fatalf("invalid stored experiment: %+v", got)
		}
		got.Spec.Tasks[0].Input = "reader changed input"
		again, err := s.GetExperiment(ctx, wantID)
		if err != nil || again.Spec.Tasks[0].Input != "Discovery task" {
			t.Fatalf("read alias leaked: %+v %v", again, err)
		}
		version, err := s.GetVersion(ctx, wantVersion.ID)
		if err != nil || version.Digest != wantVersion.Digest {
			t.Fatalf("prepared version did not round trip: %+v %v", version, err)
		}
		if _, err = version.Prepared.Inspect(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("TransactionHandleExpires", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := Fixture(t, "expired")
		ctx := scoped(bundle.Experiment.Spec.Scope)
		var escaped store.Tx
		if err := s.Transact(ctx, func(tx store.Tx) error { escaped = tx; return nil }); err != nil {
			t.Fatal(err)
		}
		if err := escaped.CreateExperiment(bundle); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("escaped transaction still writes: %v", err)
		}
		if _, err := s.GetExperiment(ctx, bundle.Experiment.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatal("expired handle changed store")
		}
	})
	t.Run("VersionReusePreservesCreationIdentity", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := Fixture(t, "version-reuse")
		ctx := scoped(bundle.Experiment.Spec.Scope)
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
			t.Fatal(err)
		}
		original, err := s.GetVersion(ctx, bundle.Versions[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		bundle.Experiment.ID = id.New(optimization.PrefixExperiment)
		bundle.Versions[0].CreatedBy = "different-actor"
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
			t.Fatal(err)
		}
		again, err := s.GetVersion(ctx, original.ID)
		if err != nil {
			t.Fatal(err)
		}
		if again.CreatedBy != original.CreatedBy {
			t.Fatal("reused version rewrote original creator")
		}
	})
}

func scoped(scope cortex.Scope) context.Context { return cortex.WithScope(context.Background(), scope) }

// Fixture creates a populated draft bundle for shared backend checks.
func Fixture(t *testing.T, project string) optimization.ExperimentBundle {
	t.Helper()
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "test"}, {Key: "workspace", Value: "test"}, {Key: "project", Value: project}}}
	agentID := id.NewAgentID()
	e, err := engine.New(engine.WithStore(scopespy.New()), engine.WithModelRevision("fixture", "1"))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := e.PrepareAgent(scoped(scope), agentID, &engine.RunOverrides{SystemPrompt: "Fixture", Model: "fixture", ToolsRestricted: true})
	if err != nil {
		t.Fatal(err)
	}
	policy := optimization.EditPolicy{MaxBodyBytes: 128, MaxPatchBytes: 256, MaxEdits: 2}
	base, err := optimization.SealVersion(optimization.HarnessVersion{ID: id.New(optimization.PrefixVersion), HarnessID: id.New(optimization.PrefixHarness), SchemaVersion: 1, Kind: optimization.HarnessExecution, Scope: scope, AgentID: agentID, Prepared: prepared, RuntimeVersion: "fixture", Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := optimization.SealVersion(optimization.HarnessVersion{ID: id.New(optimization.PrefixVersion), HarnessID: id.New(optimization.PrefixHarness), SchemaVersion: 1, Kind: optimization.HarnessImprovement, Scope: scope, AgentID: agentID, Controller: &optimization.ControllerSpec{Model: "fixture", ModelRevision: "1", Tools: []string{}}, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	tasks := make([]optimization.TaskCase, 0, 2)
	for i, input := range []string{"Discovery task", "Held-out task"} {
		split := optimization.SplitDiscovery
		if i == 1 {
			split = optimization.SplitValidation
		}
		task, taskErr := optimization.SealTaskCase(optimization.TaskCase{ID: fmt.Sprintf("case-%d", i), Input: input, Split: split, EnvironmentSnapshot: optimization.ContentRef{Ref: "fixture", Revision: "1", Digest: strings.Repeat("a", 64)}})
		if taskErr != nil {
			t.Fatal(taskErr)
		}
		tasks = append(tasks, task)
	}
	spec := optimization.ExperimentSpec{SchemaVersion: 1, Scope: scope, AgentID: agentID, Baseline: optimization.VersionRef{ID: base.ID, Digest: base.Digest}, Controller: optimization.VersionRef{ID: controller.ID, Digest: controller.Digest}, Mode: optimization.ModeSequential, Tasks: tasks, Evaluation: optimization.ContentRef{Ref: "plan", Revision: "1", Digest: strings.Repeat("b", 64)}, Limits: optimization.DefaultLimits(), Budget: optimization.Budget{MaxTokens: 1000, TokenOnly: true}, Objective: optimization.DefaultObjective(), Requester: optimization.IdentityRef{Subject: "fixture"}, LessonCutoff: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	return optimization.ExperimentBundle{Experiment: optimization.Experiment{ID: id.New(optimization.PrefixExperiment), Spec: spec}, Versions: []optimization.HarnessVersion{base, controller}}
}
