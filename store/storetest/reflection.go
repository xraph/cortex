package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/session"
	"github.com/xraph/cortex/store"
)

func reflectionFixture(t *testing.T, backend store.Store) (context.Context, cognitive.ReflectionRequest) {
	t.Helper()
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "reflection"}}})
	ag := mustCreateAgent(t, backend, ctx, "reflection")
	sessionID := id.NewSessionID()
	require.NoError(t, backend.CreateSession(ctx, &session.Session{ID: sessionID, AgentID: ag, Title: "reflection"}))
	now := time.Now().UTC()
	budget := execution.Budget{MaxSteps: 10, MaxTokens: 1000}
	r := &run.Run{Entity: cortex.NewEntity(), ID: id.NewAgentRunID(), AgentID: ag, SessionID: sessionID, Scope: cortex.ScopeFromContext(ctx), State: run.StateRunning, Budget: budget, StartedAt: &now, Provenance: run.ExecutionProvenance{SchemaVersion: 1, PreparedDigest: evidence.Digest([]byte("prepared")), Purpose: run.PurposeNormal, VerificationRequired: true}}
	require.NoError(t, backend.CreateRun(ctx, r))
	current, err := backend.GetRun(ctx, r.ID)
	require.NoError(t, err)
	return ctx, cognitive.ReflectionRequest{SchemaVersion: 1, ExpectedRunRevision: current.Revision, Snapshot: cognitive.ReflectionSnapshot{Binding: cognitive.ReflectionBinding{Scope: r.Scope, RunID: r.ID, AgentID: ag, PreparedDigest: r.Provenance.PreparedDigest, Budget: budget, VerificationRequired: true}, ObservedAt: now, Step: 3, Usage: execution.Usage{Steps: 3, Tokens: 30}, NoProgressSteps: 3, ProgressDigest: evidence.Digest([]byte("progress"))}, Policy: cognitive.ReflectionPolicy{Revision: "controller/v1", OnStuck: cognitive.ActionReplan}}
}

// ReflectionExecution covers atomic decisions, stale authority inputs and recovery.
func ReflectionExecution(t *testing.T, newStore func(*testing.T) store.Store) {
	t.Run("history", func(t *testing.T) { reflectionHistory(t, newStore(t)) })
	t.Run("retained_evidence_and_assumptions", func(t *testing.T) { reflectionEvidence(t, newStore(t)) })
	t.Run("one_fresh_admission_and_restart", func(t *testing.T) {
		backend := newStore(t)
		ctx, req := reflectionFixture(t, backend)
		s := resultCapability[cognitive.ReflectionStore](t, backend)
		var wg sync.WaitGroup
		results := make(chan cognitive.ReflectionReceipt, 6)
		errs := make(chan error, 6)
		for range 6 {
			wg.Go(func() { r, err := s.AdmitReflection(ctx, req); results <- r; errs <- err })
		}
		wg.Wait()
		close(results)
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		fresh := 0
		for r := range results {
			if r.Fresh {
				fresh++
			}
			require.Equal(t, 1, r.State.Reflections)
		}
		require.Equal(t, 1, fresh)
		restored, err := s.GetReflection(ctx, req.Snapshot.Binding.RunID)
		require.NoError(t, err)
		require.False(t, restored.Fresh)
		req.ExpectedVersion = restored.State.Version
		req.ExpectedControlRevision = restored.State.ControlRevision
		req.Snapshot.ObservedAt = time.Now().UTC()
		next, err := s.AdmitReflection(ctx, req)
		require.NoError(t, err)
		require.Equal(t, cognitive.ReflectionPending, next.Decision.Reason)
		require.Equal(t, 1, next.State.Reflections)
	})
	t.Run("user_intervention_supersedes_pending_proposal", func(t *testing.T) {
		backend := newStore(t)
		ctx, req := reflectionFixture(t, backend)
		s := resultCapability[cognitive.ReflectionStore](t, backend)
		first, err := s.AdmitReflection(ctx, req)
		require.NoError(t, err)
		correction := req
		correction.ExpectedVersion = first.State.Version
		correction.ExpectedControlRevision = first.State.ControlRevision
		correction.Intervention = &cognitive.ContextSource{Reference: "conversation:correction", Digest: evidence.Digest([]byte("correction"))}
		fixed, err := s.AdmitReflection(ctx, correction)
		require.NoError(t, err)
		require.Equal(t, int64(2), fixed.State.ControlRevision)
		require.False(t, fixed.State.Pending)
		require.Equal(t, 1, fixed.State.Reflections)
		stale := req
		stale.ExpectedVersion = fixed.State.Version
		stale.ExpectedControlRevision = first.State.ControlRevision
		_, err = s.AdmitReflection(ctx, stale)
		require.ErrorIs(t, err, cognitive.ErrReflectionConflict)
	})
	t.Run("cannot_raise_budget_or_remove_required_check", func(t *testing.T) {
		for _, mutate := range []func(*cognitive.ReflectionRequest){func(r *cognitive.ReflectionRequest) { r.Snapshot.Binding.Budget.MaxTokens++ }, func(r *cognitive.ReflectionRequest) { r.Snapshot.Binding.VerificationRequired = false }, func(r *cognitive.ReflectionRequest) { r.ExpectedRunRevision++ }} {
			backend := newStore(t)
			ctx, req := reflectionFixture(t, backend)
			s := resultCapability[cognitive.ReflectionStore](t, backend)
			mutate(&req)
			_, err := s.AdmitReflection(ctx, req)
			require.ErrorIs(t, err, cognitive.ErrReflectionConflict)
			retained, err := s.GetReflection(ctx, req.Snapshot.Binding.RunID)
			require.NoError(t, err)
			require.Nil(t, retained)
		}
	})
	t.Run("scope_and_policy_cannot_drift", func(t *testing.T) {
		backend := newStore(t)
		ctx, req := reflectionFixture(t, backend)
		s := resultCapability[cognitive.ReflectionStore](t, backend)
		first, err := s.AdmitReflection(ctx, req)
		require.NoError(t, err)
		other := cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "other"}}})
		_, err = s.AdmitReflection(other, req)
		require.Error(t, err)
		req.ExpectedVersion = first.State.Version
		req.ExpectedControlRevision = first.State.ControlRevision
		req.Policy.MaxReflections = 16
		_, err = s.AdmitReflection(ctx, req)
		require.ErrorIs(t, err, cognitive.ErrReflectionConflict)
	})
}

func reflectionEvidence(t *testing.T, backend store.Store) {
	t.Helper()
	ctx, req := reflectionFixture(t, backend)
	s := resultCapability[cognitive.ReflectionStore](t, backend)
	artifacts := resultCapability[evidence.Storage](t, backend)
	now := req.Snapshot.ObservedAt
	artifact, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: req.Snapshot.Binding.Scope, MediaType: "application/json", Version: "v1", Producer: "trusted-checker", Sensitivity: evidence.SensitivityRestricted, CreatedAt: now.Add(-time.Second), RetainUntil: now.Add(time.Hour)}, Data: []byte(`{"resource_exists":false}`)})
	require.NoError(t, err)
	req.Snapshot.Assumptions = []cognitive.Assumption{{Key: "resource", Summary: "Resource exists", Origin: cognitive.AssumptionHost, State: cognitive.AssumptionDisputed, Evidence: []evidence.Ref{artifact.Ref}}}
	_, err = s.AdmitReflection(ctx, req)
	require.ErrorIs(t, err, evidence.ErrNotFound)
	require.NoError(t, artifacts.SaveEvidence(ctx, artifact))
	first, err := s.AdmitReflection(ctx, req)
	require.NoError(t, err)
	require.Equal(t, cognitive.ReflectionConflict, first.Decision.Reason)
	require.NoError(t, artifacts.TombstoneEvidence(evidence.WithDeletionActor(ctx, "host"), artifact.Ref, "source revoked"))
	next := req
	next.ExpectedVersion = first.State.Version
	next.ExpectedControlRevision = first.State.ControlRevision
	next.Snapshot.ObservedAt = time.Now().UTC()
	_, err = s.AdmitReflection(ctx, next)
	require.ErrorIs(t, err, evidence.ErrDeleted)
	retained, err := s.GetReflection(ctx, req.Snapshot.Binding.RunID)
	require.NoError(t, err)
	require.Equal(t, first.State.Version, retained.State.Version)
	// Historical reads retain the observation without restoring execution authority.
	require.False(t, retained.Fresh)
}
