package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

func executionInputIntent(t *testing.T, s store.Store) (context.Context, verification.CaptureIntent, string) {
	t.Helper()
	ctx, intent, output := captureIntentStoreFixture(t, s)
	r, err := s.GetRun(ctx, intent.Template.Binding.RunID)
	require.NoError(t, err)
	r.Input, r.SessionID = "scanned task", id.NewSessionID()
	r.Provenance.ModelRevision = "model:1"
	r.Provenance.Tools = []run.Revision{{Name: "probe", Revision: "1"}}
	inputs := verification.ExecutionInputs{SchemaVersion: 1, RunID: r.ID, AgentID: r.AgentID, SessionID: r.SessionID, PreparedDigest: r.Provenance.PreparedDigest, Input: r.Input, History: []llm.Message{{Role: "user", Content: "private history"}}, Model: "model", ModelRevision: r.Provenance.ModelRevision, Tools: []verification.ToolRevision{{Name: "probe", Revision: "1"}}}
	data, err := inputs.Encode()
	require.NoError(t, err)
	ref := intent.Template.Binding.Candidate
	ref.ID, ref.MediaType = id.New(evidence.PrefixArtifact), "application/json"
	artifact, err := evidence.Seal(evidence.Artifact{Ref: ref, Data: data})
	require.NoError(t, err)
	require.NoError(t, resultCapability[evidence.Storage](t, s).SaveEvidence(ctx, artifact))
	r.Provenance.ExecutionInputsDigest = artifact.Ref.SHA256
	require.NoError(t, s.UpdateRun(ctx, r))
	intent.Template.ExpectedRunRevision = r.Revision
	intent.Template.Binding.Context.ExecutionInputs = &artifact.Ref
	intent.Template.Binding.Evidence = append(intent.Template.Binding.Evidence, artifact.Ref)
	intent, err = verification.SealCaptureIntent(intent)
	require.NoError(t, err)
	return ctx, intent, output
}

func testExecutionInputs(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, stage := range []string{"registration", "capture"} {
		t.Run(stage, func(t *testing.T) {
			for name, mutate := range map[string]func(*run.Run){
				"task":           func(r *run.Run) { r.Input = "different" },
				"session":        func(r *run.Run) { r.SessionID = id.NewSessionID() },
				"agent":          func(r *run.Run) { r.AgentID = id.NewAgentID() },
				"model":          func(r *run.Run) { r.Provenance.ModelRevision = "model:2" },
				"tool":           func(r *run.Run) { r.Provenance.Tools[0].Revision = "2" },
				"digest":         func(r *run.Run) { r.Provenance.ExecutionInputsDigest = evidence.Digest(nil) },
				"removed digest": func(r *run.Run) { r.Provenance.ExecutionInputsDigest = "" },
			} {
				t.Run(name, func(t *testing.T) {
					s := newStore(t)
					ctx, intent, output := executionInputIntent(t, s)
					storage := resultCapability[verification.CaptureIntentStore](t, s)
					if stage == "capture" {
						require.NoError(t, storage.CreateCaptureIntent(ctx, intent))
					}
					r, err := s.GetRun(ctx, intent.Template.Binding.RunID)
					require.NoError(t, err)
					mutate(r)
					if name == "agent" {
						require.NoError(t, s.Create(ctx, &agent.Config{ID: r.AgentID, Name: r.AgentID.String()}))
					}
					require.NoError(t, s.UpdateRun(ctx, r))
					if stage == "registration" {
						intent.Template.ExpectedRunRevision = r.Revision
						intent, err = verification.SealCaptureIntent(intent)
						require.NoError(t, err)
						err = storage.CreateCaptureIntent(ctx, intent)
					} else {
						_, err = resultCapability[run.CandidateStore](t, s).CaptureCandidate(ctx, run.CandidateCapture{SchemaVersion: 1, IntentID: intent.ID, ExpectedRunRevision: r.Revision, Output: output})
					}
					require.ErrorIs(t, err, cortex.ErrRunConflict)
					saved, err := s.GetRun(ctx, r.ID)
					require.NoError(t, err)
					require.Empty(t, saved.Output)
					require.Empty(t, saved.ResultRef)
				})
			}
		})
	}
	t.Run("publish", func(t *testing.T) {
		s := newStore(t)
		ctx, intent, output := executionInputIntent(t, s)
		require.NoError(t, resultCapability[verification.CaptureIntentStore](t, s).CreateCaptureIntent(ctx, intent))
		job, err := resultCapability[run.CandidateStore](t, s).CaptureCandidate(ctx, run.CandidateCapture{SchemaVersion: 1, IntentID: intent.ID, ExpectedRunRevision: intent.Template.ExpectedRunRevision, Output: output})
		require.NoError(t, err)
		job = settlePrivateCandidate(ctx, t, s, job, verification.StatusPassed)
		req := jobCommitRequest(job)
		req.ResultID = intent.ResultID
		_, err = resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, req)
		require.NoError(t, err)
	})

	t.Run("missing reference", func(t *testing.T) {
		s := newStore(t)
		ctx, intent, _ := executionInputIntent(t, s)
		intent.Template.Binding.Context.ExecutionInputs = nil
		intent, err := verification.SealCaptureIntent(intent)
		require.NoError(t, err)
		require.ErrorIs(t, resultCapability[verification.CaptureIntentStore](t, s).CreateCaptureIntent(ctx, intent), cortex.ErrRunConflict)
	})
	t.Run("deleted before commit", func(t *testing.T) {
		s := newStore(t)
		ctx, intent, output := executionInputIntent(t, s)
		require.NoError(t, resultCapability[verification.CaptureIntentStore](t, s).CreateCaptureIntent(ctx, intent))
		job, err := resultCapability[run.CandidateStore](t, s).CaptureCandidate(ctx, run.CandidateCapture{SchemaVersion: 1, IntentID: intent.ID, ExpectedRunRevision: intent.Template.ExpectedRunRevision, Output: output})
		require.NoError(t, err)
		job = settlePrivateCandidate(ctx, t, s, job, verification.StatusPassed)
		require.NoError(t, resultCapability[evidence.Storage](t, s).TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), *intent.Template.Binding.Context.ExecutionInputs, "revoked"))
		req := jobCommitRequest(job)
		req.ResultID = intent.ResultID
		_, err = resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, req)
		require.ErrorIs(t, err, evidence.ErrDeleted)
		saved, err := s.GetRun(ctx, intent.Template.Binding.RunID)
		require.NoError(t, err)
		require.Empty(t, saved.Output)
		require.Empty(t, saved.ResultRef)
	})
	t.Run("deleted before claim", func(t *testing.T) {
		s := newStore(t)
		ctx, intent, output := executionInputIntent(t, s)
		require.NoError(t, resultCapability[verification.CaptureIntentStore](t, s).CreateCaptureIntent(ctx, intent))
		job, err := resultCapability[run.CandidateStore](t, s).CaptureCandidate(ctx, run.CandidateCapture{SchemaVersion: 1, IntentID: intent.ID, ExpectedRunRevision: intent.Template.ExpectedRunRevision, Output: output})
		require.NoError(t, err)
		require.NoError(t, resultCapability[evidence.Storage](t, s).TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), *intent.Template.Binding.Context.ExecutionInputs, "revoked"))
		_, err = resultCapability[verification.JobStore](t, s).ClaimVerificationJob(ctx, verification.JobClaim{JobID: job.Request.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "worker", Duration: time.Minute})
		require.ErrorIs(t, err, evidence.ErrDeleted)
	})
}
