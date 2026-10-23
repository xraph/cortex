package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/verification"
)

func transcriptCaptureFixture(t *testing.T, s store.Store) (context.Context, verification.CaptureIntent, run.CandidateCapture) {
	t.Helper()
	ctx, intent, output := executionInputIntent(t, s)
	intent.SchemaVersion = 2
	ref := intent.Template.Binding.Candidate
	ref.ID = id.New(evidence.PrefixArtifact)
	intent.Transcript = &ref
	intent.MaxTranscriptBytes = 1 << 20
	intent, err := verification.SealCaptureIntent(intent)
	require.NoError(t, err)
	usage := execution.Usage{Steps: 2, ToolCalls: 1, Tokens: 10, ReportedTokens: 10}
	trace := verification.ExecutionTranscript{SchemaVersion: 1, RunID: intent.Template.Binding.RunID, InputsDigest: intent.Template.Binding.Context.ExecutionInputs.SHA256, Usage: usage, Messages: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call", Name: "probe", Arguments: `{"private":true}`}}},
		{Role: "tool", ToolCallID: "call", Content: "private tool result"},
		{Role: "assistant", Content: output},
	}, Tools: []verification.ToolResult{{CallID: "call", Outcome: verification.ToolCompleted, Source: verification.ToolLocal}}}
	data, err := trace.Encode()
	require.NoError(t, err)
	return ctx, intent, run.CandidateCapture{SchemaVersion: 2, IntentID: intent.ID, ExpectedRunRevision: intent.Template.ExpectedRunRevision, Output: output, FinishReason: "stop", Usage: usage, Transcript: data}
}

func testTranscriptCapture(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, mode := range []string{"publish", "deleted before capture", "deleted before claim", "deleted before commit", "changed retry", "reserved identity exists"} {
		t.Run(mode, func(t *testing.T) {
			s := newStore(t)
			ctx, intent, capture := transcriptCaptureFixture(t, s)
			es := resultCapability[evidence.Storage](t, s)
			intents := resultCapability[verification.CaptureIntentStore](t, s)
			if mode == "reserved identity exists" {
				require.NoError(t, es.SaveEvidence(ctx, evidence.Artifact{Ref: *intent.Transcript}))
				require.ErrorIs(t, intents.CreateCaptureIntent(ctx, intent), evidence.ErrConflict)
				return
			}
			require.NoError(t, intents.CreateCaptureIntent(ctx, intent))
			req, err := intent.JobFor(capture.ExpectedRunRevision+1, capture.Output, capture.Transcript)
			require.NoError(t, err)
			if mode == "deleted before capture" {
				require.NoError(t, es.TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), *req.Binding.Context.ExecutionTranscript, "revoked"))
				_, err = resultCapability[run.CandidateStore](t, s).CaptureCandidate(ctx, capture)
				require.ErrorIs(t, err, evidence.ErrDeleted)
				_, err = es.LoadEvidence(ctx, req.Binding.Candidate)
				require.ErrorIs(t, err, evidence.ErrNotFound)
				saved, readErr := s.GetRun(ctx, intent.Template.Binding.RunID)
				require.NoError(t, readErr)
				require.Equal(t, run.StateRunning, saved.State)
				return
			}
			cs := resultCapability[run.CandidateStore](t, s)
			job, err := cs.CaptureCandidate(ctx, capture)
			require.NoError(t, err)
			artifact, err := es.LoadEvidence(ctx, *job.Request.Binding.Context.ExecutionTranscript)
			require.NoError(t, err)
			require.Equal(t, capture.Transcript, artifact.Data)
			retried, err := cs.CaptureCandidate(ctx, capture)
			require.NoError(t, err)
			require.Equal(t, job, retried)
			if mode == "changed retry" {
				trace, decodeErr := verification.DecodeExecutionTranscript(capture.Transcript)
				require.NoError(t, decodeErr)
				trace.Messages[1].Content = "different result"
				capture.Transcript, err = trace.Encode()
				require.NoError(t, err)
				_, err = cs.CaptureCandidate(ctx, capture)
				require.ErrorIs(t, err, verification.ErrConflict)
				return
			}
			if mode == "deleted before claim" {
				require.NoError(t, es.TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), artifact.Ref, "revoked"))
				_, err = resultCapability[verification.JobStore](t, s).ClaimVerificationJob(ctx, verification.JobClaim{JobID: job.Request.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "worker", Duration: time.Minute})
				require.ErrorIs(t, err, evidence.ErrDeleted)
				return
			}
			job = settlePrivateCandidate(ctx, t, s, job, verification.StatusPassed)
			commit := jobCommitRequest(job)
			commit.ResultID = intent.ResultID
			if mode == "deleted before commit" {
				require.NoError(t, es.TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), artifact.Ref, "revoked"))
			}
			_, err = resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, commit)
			if mode == "deleted before commit" {
				require.ErrorIs(t, err, evidence.ErrDeleted)
				return
			}
			require.NoError(t, err)
			saved, err := s.GetRun(ctx, intent.Template.Binding.RunID)
			require.NoError(t, err)
			require.Equal(t, capture.Output, saved.Output)
		})
	}
}

func transcriptCaptureRollback(t *testing.T, s store.Store, driver assurance.Driver) {
	t.Helper()
	ctx, intent, capture := transcriptCaptureFixture(t, s)
	require.NoError(t, resultCapability[verification.CaptureIntentStore](t, s).CreateCaptureIntent(ctx, intent))
	transaction, ok := driver.(assurance.RunDriver)
	require.True(t, ok)
	failure := errors.New("after transcript capture")
	repository := assurance.New(failingRunDriver{Driver: driver, transaction: transaction, failure: failure})
	before, err := s.GetRun(ctx, intent.Template.Binding.RunID)
	require.NoError(t, err)
	_, err = repository.CaptureCandidate(ctx, capture)
	require.ErrorIs(t, err, failure)
	after, err := s.GetRun(ctx, intent.Template.Binding.RunID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	req, err := intent.JobFor(capture.ExpectedRunRevision+1, capture.Output, capture.Transcript)
	require.NoError(t, err)
	for _, ref := range []evidence.Ref{req.Binding.Candidate, *req.Binding.Context.ExecutionTranscript} {
		_, err = resultCapability[evidence.Storage](t, s).LoadEvidence(ctx, ref)
		require.ErrorIs(t, err, evidence.ErrNotFound)
	}
	_, err = resultCapability[run.CandidateStore](t, s).CaptureCandidate(ctx, capture)
	require.NoError(t, err)
}
