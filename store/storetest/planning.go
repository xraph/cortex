package storetest

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

func storedPlan(t *testing.T, scope cortex.Scope) planning.Plan {
	t.Helper()
	contract, err := verification.SealContract(verification.Contract{SchemaVersion: 1, ID: id.New(verification.PrefixContract), Scope: scope, Revision: "1", Objective: "checked report", Criteria: []verification.Criterion{{ID: "check", Kind: verification.KindTest, Required: true, Verifier: "tests", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Hour}}})
	require.NoError(t, err)
	budget := execution.Budget{MaxSteps: 10, MaxTokens: 1000, MaxToolCalls: 10, Deadline: time.Now().UTC().Add(time.Hour)}
	p, err := planning.Seal(planning.Plan{SchemaVersion: 1, ID: id.New(planning.PrefixPlan), Scope: scope, Revision: 1, Objective: contract.Objective, Contract: contract, Budget: budget, Owners: []string{"worker"}, Actor: "host", Reason: "request", Tasks: []planning.Task{{ID: id.New(planning.PrefixTask), Instruction: "produce report", Owner: "worker", Criteria: []string{"check"}, Budget: budget, Output: planning.ArtifactRequirement{Name: "report", MediaType: "text/plain", MaxBytes: 4096}}}}, planning.Limits{})
	require.NoError(t, err)
	return p
}

func testPlanning(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	s, ok := backend.(planning.Store)
	require.True(t, ok, "planning capability missing")
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "planning"}, {Key: "project", Value: "one"}}}
	ctx := cortex.WithScope(context.Background(), scope)
	p := storedPlan(t, scope)
	require.NoError(t, s.CreatePlan(ctx, p))
	require.NoError(t, s.CreatePlan(ctx, p))
	got, err := s.GetPlan(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, p, got)
	changed, err := planning.Seal(p, planning.Limits{})
	require.NoError(t, err)
	changed.Tasks[0].Instruction = "different"
	changed, err = planning.Seal(changed, planning.Limits{})
	require.NoError(t, err)
	require.ErrorIs(t, s.CreatePlan(ctx, changed), planning.ErrConflict)
	proposal := planning.Revision{Plan: changed, Actor: "host", Reason: "refine"}
	next, err := s.CommitRevision(ctx, proposal, 1)
	require.NoError(t, err)
	require.EqualValues(t, 2, next.Revision)
	retry, err := s.CommitRevision(ctx, proposal, 1)
	require.NoError(t, err)
	require.Equal(t, next, retry)
	proposal.Reason = "conflicting retry"
	_, err = s.CommitRevision(ctx, proposal, 1)
	require.ErrorIs(t, err, planning.ErrConflict)
	got, err = s.GetPlan(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, next, got)
	old, err := s.GetPlanRevision(ctx, p.ID, 1)
	require.NoError(t, err)
	require.Equal(t, p, old)
	tasks, err := s.ReadTasks(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, next.Tasks, tasks)
	tasks[0].Instruction = "caller mutation"
	got, err = s.GetPlan(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, next, got)
	listed, err := s.ListPlans(ctx, planning.ListRequest{Limit: 1})
	require.NoError(t, err)
	require.Equal(t, []planning.Plan{next}, listed)
	listed, err = s.ListPlans(ctx, planning.ListRequest{Limit: 1, After: p.ID})
	require.NoError(t, err)
	require.Empty(t, listed)
	_, err = s.ListPlans(ctx, planning.ListRequest{})
	require.Error(t, err)
	_, err = s.CommitRevision(ctx, planning.Revision{Plan: next, Actor: "host", Reason: "skip"}, 9)
	require.ErrorIs(t, err, planning.ErrConflict)
	for _, otherScope := range []cortex.Scope{{Levels: scope.Levels[:1]}, {Levels: []cortex.Level{{Key: "org", Value: "planning"}, {Key: "project", Value: "other"}}}} {
		other := cortex.WithScope(ctx, otherScope)
		_, err = s.GetPlan(other, p.ID)
		require.ErrorIs(t, err, planning.ErrNotFound)
		_, err = s.GetPlanRevision(other, p.ID, 1)
		require.ErrorIs(t, err, planning.ErrNotFound)
		_, err = s.ReadTasks(other, p.ID)
		require.ErrorIs(t, err, planning.ErrNotFound)
		listed, err = s.ListPlans(other, planning.ListRequest{Limit: 1})
		require.NoError(t, err)
		require.Empty(t, listed)
		require.Error(t, s.CreatePlan(other, p))
		_, err = s.CommitRevision(other, proposal, 1)
		require.Error(t, err)
	}
	require.ErrorIs(t, s.CreatePlan(context.Background(), p), cortex.ErrNoScope)
	_, err = s.GetPlan(context.Background(), p.ID)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.GetPlan(cancelled, p.ID)
	require.ErrorIs(t, err, context.Canceled)
	inspector, supported := backend.(store.ScopeInspector)
	require.True(t, supported)
	var issues []store.ScopeIssue
	require.NoError(t, inspector.InspectScopes(context.Background(), func(issue store.ScopeIssue) error { issues = append(issues, issue); return nil }))
	require.Empty(t, issues)
}

func testPlanningConcurrentRevision(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	s, ok := backend.(planning.Store)
	require.True(t, ok)
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "planning-race"}}}
	ctx := cortex.WithScope(context.Background(), scope)
	p := storedPlan(t, scope)
	require.NoError(t, s.CreatePlan(ctx, p))
	const workers = 12
	results := make([]planning.Plan, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = s.CommitRevision(ctx, planning.Revision{Plan: p, Actor: "host", Reason: fmt.Sprintf("proposal %d", i)}, 1)
		}()
	}
	wg.Wait()
	successes := 0
	var winner planning.Plan
	for i, err := range errs {
		if err == nil {
			successes++
			winner = results[i]
		} else {
			require.ErrorIs(t, err, planning.ErrConflict)
		}
	}
	require.Equal(t, 1, successes)
	latest, err := s.GetPlan(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, winner, latest)
	for revision := int64(3); revision <= 12; revision++ {
		latest, err = s.CommitRevision(ctx, planning.Revision{Plan: latest, Actor: "host", Reason: "advance"}, revision-1)
		require.NoError(t, err)
	}
	got, err := s.GetPlan(ctx, p.ID)
	require.NoError(t, err)
	require.EqualValues(t, 12, got.Revision)
	previous := p
	for revision := int64(2); revision <= 12; revision++ {
		saved, readErr := s.GetPlanRevision(ctx, p.ID, revision)
		require.NoError(t, readErr)
		require.Equal(t, previous.Digest, saved.PreviousDigest)
		previous = saved
	}
	replay, replayErr := s.CommitRevision(ctx, planning.Revision{Plan: p, Actor: winner.Actor, Reason: winner.Reason}, 1)
	require.NoError(t, replayErr)
	require.Equal(t, winner, replay)
	got, err = s.GetPlan(ctx, p.ID)
	require.NoError(t, err)
	require.EqualValues(t, 12, got.Revision)
}
