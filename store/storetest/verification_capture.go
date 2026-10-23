package storetest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/verification"
)

type captureJobStore interface {
	verification.JobStore
	verification.CaptureStore
}

func captureJobFixture(t *testing.T, s store.Store) (context.Context, verification.JobRequest) {
	t.Helper()
	ctx, req, _ := verificationJobFixture(t, s)
	req.Binding.Candidate.ID = id.New(evidence.PrefixArtifact)
	return ctx, req
}

func testVerificationCapture(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, req := captureJobFixture(t, s)
	cs := resultCapability[captureJobStore](t, s)
	es := resultCapability[evidence.Storage](t, s)
	before, err := s.GetRun(ctx, req.Binding.RunID)
	require.NoError(t, err)
	_, err = es.LoadEvidence(ctx, req.Binding.Candidate)
	require.ErrorIs(t, err, evidence.ErrNotFound)
	job, err := cs.CaptureVerificationJob(ctx, req)
	require.NoError(t, err)
	require.NoError(t, job.Validate())
	require.Equal(t, verification.JobQueued, job.State)
	artifact, err := es.LoadEvidence(ctx, req.Binding.Candidate)
	require.NoError(t, err)
	require.Equal(t, []byte(before.Output), artifact.Data)
	require.Equal(t, req.Binding.Candidate, artifact.Ref)
	saved, err := s.GetRun(ctx, req.Binding.RunID)
	require.NoError(t, err)
	require.Equal(t, before.Revision+1, saved.Revision)
	require.Equal(t, before.Output, saved.Output)
	require.Equal(t, verification.StatusPending, saved.VerificationStatus)
	retry, err := cs.CaptureVerificationJob(ctx, req)
	require.NoError(t, err)
	require.Equal(t, job, retry)
	retry, err = cs.EnqueueVerificationJob(ctx, req)
	require.NoError(t, err)
	require.Equal(t, job, retry)
	changed := req
	changed.Binding.Candidate.ID = id.New(evidence.PrefixArtifact)
	_, err = cs.CaptureVerificationJob(ctx, changed)
	require.ErrorIs(t, err, verification.ErrJobConflict)
	_, err = es.LoadEvidence(ctx, changed.Binding.Candidate)
	require.ErrorIs(t, err, evidence.ErrNotFound)
	require.NoError(t, es.TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), req.Binding.Candidate, "revoked"))
	retry, err = cs.CaptureVerificationJob(ctx, req)
	require.NoError(t, err)
	require.Equal(t, job, retry)
	_, err = es.LoadEvidence(ctx, req.Binding.Candidate)
	require.ErrorIs(t, err, evidence.ErrDeleted, "historical retry must not restore removed bytes")
}

func testVerificationCaptureDenied(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, mode := range []string{"wrong scope", "missing scope", "cancelled", "changed output", "revision", "incomplete", "missing contract", "missing evidence", "retention", "expired", "oversized", "tombstone", "conflicting evidence", "preexisting"} {
		t.Run(mode, func(t *testing.T) {
			s := newStore(t)
			ctx, req := captureJobFixture(t, s)
			cs := resultCapability[captureJobStore](t, s)
			es := resultCapability[evidence.Storage](t, s)
			callCtx := ctx
			current, err := s.GetRun(ctx, req.Binding.RunID)
			require.NoError(t, err)
			var existing *evidence.Artifact
			switch mode {
			case "wrong scope":
				callCtx = ctxWithScope("other")
			case "missing scope":
				callCtx = context.Background()
			case "cancelled":
				var cancel context.CancelFunc
				callCtx, cancel = context.WithCancel(ctx)
				cancel()
			case "changed output":
				current.Output = "substituted output"
				require.NoError(t, s.UpdateRun(ctx, current))
				req.ExpectedRunRevision = current.Revision
			case "revision":
				req.ExpectedRunRevision++
			case "incomplete":
				current.State = run.StateIncomplete
				require.NoError(t, s.UpdateRun(ctx, current))
				req.ExpectedRunRevision = current.Revision
			case "missing contract":
				req.ContractID = id.New(verification.PrefixContract)
				current.Provenance.AcceptanceRef = req.ContractID.String()
				require.NoError(t, s.UpdateRun(ctx, current))
				req.ExpectedRunRevision = current.Revision
			case "missing evidence":
				ref := req.Binding.Candidate
				ref.ID = id.New(evidence.PrefixArtifact)
				req.Binding.Evidence = []evidence.Ref{ref}
			case "retention":
				req.Binding.Candidate.RetainUntil = req.Deadline.Add(-time.Second)
			case "expired":
				req.Deadline = time.Now().Add(-time.Second)
			case "oversized":
				current.Output = strings.Repeat("x", assurance.MaxEvidenceBytes+1)
				require.NoError(t, s.UpdateRun(ctx, current))
				req.ExpectedRunRevision = current.Revision
				req.Binding.Candidate.Bytes = int64(len(current.Output))
				req.Binding.Candidate.SHA256 = evidence.Digest([]byte(current.Output))
			case "tombstone":
				require.NoError(t, es.TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), req.Binding.Candidate, "deleted before capture"))
			case "conflicting evidence", "preexisting":
				artifact := evidence.Artifact{Ref: req.Binding.Candidate, Data: []byte(current.Output)}
				if mode == "conflicting evidence" {
					artifact.Ref.Producer = "another producer"
				}
				require.NoError(t, es.SaveEvidence(ctx, artifact))
				existing = &artifact
			}
			before, err := s.GetRun(ctx, req.Binding.RunID)
			require.NoError(t, err)
			job, err := cs.CaptureVerificationJob(callCtx, req)
			if mode == "preexisting" {
				require.NoError(t, err)
				require.Equal(t, verification.JobQueued, job.State)
				return
			}
			require.Error(t, err)
			require.Zero(t, job)
			_, err = cs.GetVerificationJob(ctx, req.ID)
			require.ErrorIs(t, err, verification.ErrJobNotFound)
			after, err := s.GetRun(ctx, req.Binding.RunID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			switch mode {
			case "tombstone":
				_, err = es.LoadEvidence(ctx, req.Binding.Candidate)
				require.ErrorIs(t, err, evidence.ErrDeleted)
			case "conflicting evidence":
				got, loadErr := es.LoadEvidence(ctx, existing.Ref)
				require.NoError(t, loadErr)
				require.Equal(t, *existing, got)
			default:
				_, err = es.LoadEvidence(ctx, req.Binding.Candidate)
				require.ErrorIs(t, err, evidence.ErrNotFound)
			}
		})
	}
}

func testVerificationCaptureConcurrent(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, same := range []bool{false, true} {
		t.Run(map[bool]string{false: "different jobs", true: "same job"}[same], func(t *testing.T) {
			s := newStore(t)
			ctx, req := captureJobFixture(t, s)
			cs := resultCapability[captureJobStore](t, s)
			requests := []verification.JobRequest{req, req}
			if !same {
				requests[1].ID = id.New(verification.PrefixJob)
				requests[1].Binding.Candidate.ID = id.New(evidence.PrefixArtifact)
			}
			type attempt struct {
				job     verification.Job
				err     error
				request verification.JobRequest
			}
			start := make(chan struct{})
			results := make(chan attempt, 2)
			for _, request := range requests {
				go func() {
					<-start
					job, err := cs.CaptureVerificationJob(ctx, request)
					results <- attempt{job: job, err: err, request: request}
				}()
			}
			close(start)
			successes := 0
			for range 2 {
				result := <-results
				if result.err == nil {
					successes++
					require.NoError(t, result.job.Validate())
				} else {
					require.ErrorIs(t, result.err, cortex.ErrRunConflict)
					_, err := resultCapability[evidence.Storage](t, s).LoadEvidence(ctx, result.request.Binding.Candidate)
					require.ErrorIs(t, err, evidence.ErrNotFound)
				}
			}
			expected := 1
			if same {
				expected = 2
			}
			require.Equal(t, expected, successes)
			saved, err := s.GetRun(ctx, req.Binding.RunID)
			require.NoError(t, err)
			require.Equal(t, req.ExpectedRunRevision+1, saved.Revision)
		})
	}
}

type captureFailDriver struct {
	assurance.Driver
	transaction assurance.RunDriver
	kind        string
	failure     error
}

func (d captureFailDriver) TransactRun(ctx context.Context, runID id.AgentRunID, callback func(context.Context, assurance.RunTransaction) error) error {
	return d.transaction.TransactRun(ctx, runID, func(txctx context.Context, tx assurance.RunTransaction) error {
		return callback(txctx, captureFailTransaction{RunTransaction: tx, kind: d.kind, failure: d.failure})
	})
}

type captureFailTransaction struct {
	assurance.RunTransaction
	kind    string
	failure error
}

func (tx captureFailTransaction) Insert(ctx context.Context, row assurance.Record) error {
	if err := tx.RunTransaction.Insert(ctx, row); err != nil {
		return err
	}
	if row.Kind == tx.kind {
		return tx.failure
	}
	return nil
}
func (tx captureFailTransaction) SetRunVerification(ctx context.Context, runID id.AgentRunID, revision int64, status verification.Status, result id.ID, at time.Time) error {
	if err := tx.RunTransaction.SetRunVerification(ctx, runID, revision, status, result, at); err != nil {
		return err
	}
	if tx.kind == "run" {
		return tx.failure
	}
	return nil
}

func (tx captureFailTransaction) SetRunCandidate(ctx context.Context, r *run.Run, at time.Time) error {
	if err := tx.RunTransaction.SetRunCandidate(ctx, r, at); err != nil {
		return err
	}
	if tx.kind == "candidate_run" {
		return tx.failure
	}
	return nil
}

func (tx captureFailTransaction) ReplaceJob(ctx context.Context, row assurance.Record, body string) error {
	if err := tx.RunTransaction.ReplaceJob(ctx, row, body); err != nil {
		return err
	}
	if tx.kind == "commit_job" {
		return tx.failure
	}
	return nil
}

// VerificationCaptureRollback checks every capture write inside a real transaction.
func VerificationCaptureRollback(t *testing.T, s store.Store, driver assurance.Driver) {
	t.Helper()
	transaction, ok := driver.(assurance.RunDriver)
	require.True(t, ok)
	ctx, req := captureJobFixture(t, s)
	unavailable := assurance.New(struct{ assurance.Driver }{driver})
	_, err := unavailable.CaptureVerificationJob(ctx, req)
	require.ErrorIs(t, err, verification.ErrCommitUnavailable)
	_, err = resultCapability[evidence.Storage](t, s).LoadEvidence(ctx, req.Binding.Candidate)
	require.ErrorIs(t, err, evidence.ErrNotFound)
	for _, kind := range []string{assurance.KindEvidence, assurance.KindJob, "verification_job_binding", "run"} {
		t.Run(kind, func(t *testing.T) {
			ctx, req := captureJobFixture(t, s)
			before, err := s.GetRun(ctx, req.Binding.RunID)
			require.NoError(t, err)
			failure := errors.New("interrupted capture transaction")
			failed := assurance.New(captureFailDriver{Driver: driver, transaction: transaction, kind: kind, failure: failure})
			job, err := failed.CaptureVerificationJob(ctx, req)
			require.ErrorIs(t, err, failure)
			require.Zero(t, job)
			cs := resultCapability[captureJobStore](t, s)
			_, err = cs.GetVerificationJob(ctx, req.ID)
			require.ErrorIs(t, err, verification.ErrJobNotFound)
			_, err = resultCapability[evidence.Storage](t, s).LoadEvidence(ctx, req.Binding.Candidate)
			require.ErrorIs(t, err, evidence.ErrNotFound)
			after, err := s.GetRun(ctx, req.Binding.RunID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			_, err = cs.CaptureVerificationJob(ctx, req)
			require.NoError(t, err)
		})
	}
}

// VerificationCaptureRestart recovers the same capture after reopening storage.
func VerificationCaptureRestart(t *testing.T, s store.Store, reopen func() store.Store) {
	t.Helper()
	ctx, req := captureJobFixture(t, s)
	captured, err := resultCapability[captureJobStore](t, s).CaptureVerificationJob(ctx, req)
	require.NoError(t, err)
	restored := reopen()
	recovered, err := resultCapability[captureJobStore](t, restored).CaptureVerificationJob(ctx, req)
	require.NoError(t, err)
	require.Equal(t, captured, recovered)
	artifact, err := resultCapability[evidence.Storage](t, restored).LoadEvidence(ctx, req.Binding.Candidate)
	require.NoError(t, err)
	saved, err := restored.GetRun(ctx, req.Binding.RunID)
	require.NoError(t, err)
	require.Equal(t, saved.Output, string(artifact.Data))
}
