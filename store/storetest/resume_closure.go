package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/suspension"
	"github.com/xraph/cortex/verification"
)

func closureFixture(t *testing.T, backend store.Store, driver assurance.Driver, start bool) (context.Context, *assurance.Repository, suspension.ResumeClosureRequest) {
	t.Helper()
	ctx, _, req := admissionFixture(t, backend)
	current, err := backend.GetRun(ctx, req.Version.RunID)
	require.NoError(t, err)
	current.Budget.MaxTokens, current.Usage.Tokens = 1000, 300
	current.Usage.ReportedTokens, current.Usage.Estimated = 250, true
	current.Provenance.OrchestrationReservationID = "retained-reservation"
	require.NoError(t, backend.UpdateRun(ctx, current))
	req.Version.RunRevision = current.Revision
	now := time.Now().UTC()
	repo := assurance.New(driver).WithClock(rosterClock{at: now})
	admitted, err := repo.AdmitResume(ctx, req)
	require.NoError(t, err)
	digest := evidence.Digest([]byte("missing start"))
	if start {
		started, startErr := repo.StartResume(ctx, req.Version.RunID, req.Version.SuspensionID, admitted.RequestDigest)
		require.NoError(t, startErr)
		digest, err = started.Digest()
		require.NoError(t, err)
	}
	return ctx, repo, suspension.ResumeClosureRequest{SchemaVersion: 1, Scope: req.Scope, Version: req.Version, AdmissionDigest: admitted.RequestDigest, StartDigest: digest, RunRevision: admitted.ClaimedRevision, Actor: "operator", Issuer: "fixture supervisor", IssuerRevision: "1", ProofRef: "all writers stopped", ProofDigest: evidence.Digest([]byte("fixture quiescence")), ObservedAt: now, ExpiresAt: now.Add(time.Minute)}
}

// ResumeClosureTransactions checks incomplete closure against native drivers.
func ResumeClosureTransactions(t *testing.T, backend store.Store, driver assurance.Driver) {
	t.Helper()
	for _, mode := range []string{"close", "pause_deleted", "missing_start", "new_admission", "revision", "terminal", "wrong_scope", "wrong_digest", "wrong_start", "expired", "future", "cancelled", "rollback", "lost_ack"} {
		t.Run(mode, func(t *testing.T) {
			ctx, repo, req := closureFixture(t, backend, driver, mode != "missing_start")
			attempt := ctx
			writer := repo
			switch mode {
			case "pause_deleted":
				require.NoError(t, backend.DeleteSuspension(ctx, req.Version.RunID))
			case "new_admission":
				current, err := backend.GetRun(ctx, req.Version.RunID)
				require.NoError(t, err)
				paused, err := backend.GetSuspension(ctx, req.Version.RunID)
				require.NoError(t, err)
				current.State = run.StatePaused
				require.NoError(t, backend.UpdateRun(ctx, current))
				require.NoError(t, backend.DeleteSuspension(ctx, current.ID))
				paused.ID = id.NewSuspensionID()
				require.NoError(t, backend.CreateSuspension(ctx, paused))
				admitted, err := repo.AdmitResume(ctx, suspension.ResumeAdmissionRequest{Scope: req.Scope, Version: suspension.ClaimVersion{RunID: current.ID, SuspensionID: paused.ID, RunRevision: current.Revision}, Input: []byte(`{"tool_results":[]}`)})
				require.NoError(t, err)
				req.RunRevision = admitted.ClaimedRevision
			case "revision", "terminal":
				current, err := backend.GetRun(ctx, req.Version.RunID)
				require.NoError(t, err)
				if mode == "terminal" {
					current.State = run.StateCompleted
					current.Output = "existing"
				}
				require.NoError(t, backend.UpdateRun(ctx, current))
				if mode == "terminal" {
					req.RunRevision = current.Revision
				}
			case "wrong_scope":
				attempt = ctxWithScope("other")
			case "wrong_digest":
				req.AdmissionDigest = evidence.Digest([]byte("other"))
			case "wrong_start":
				req.StartDigest = evidence.Digest([]byte("other"))
			case "expired":
				writer = assurance.New(driver).WithClock(rosterClock{at: req.ExpiresAt})
			case "future":
				writer = assurance.New(driver).WithClock(rosterClock{at: req.ObservedAt.Add(-time.Second)})
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				attempt = cancelled
			case "rollback", "lost_ack":
				tx, ok := driver.(assurance.RunDriver)
				require.True(t, ok)
				fault := errors.New("closure commit interrupted")
				var wrapped assurance.Driver = failingRunDriver{Driver: driver, transaction: tx, failure: fault}
				if mode == "lost_ack" {
					wrapped = privateLostReplyDriver{Driver: driver, transaction: tx, failure: fault}
				}
				writer = assurance.New(wrapped).WithClock(rosterClock{at: req.ObservedAt})
			}
			closer, ok := any(writer).(suspension.ResumeClosureStore)
			require.True(t, ok, "native resume closure capability")
			before, err := backend.GetRun(ctx, req.Version.RunID)
			require.NoError(t, err)
			receipt, err := closer.CloseResume(attempt, req)
			committed := mode == "close" || mode == "pause_deleted" || mode == "lost_ack"
			if committed && mode != "lost_ack" {
				require.NoError(t, err)
				require.True(t, receipt.Fresh)
				require.NoError(t, receipt.Validate())
			} else {
				require.Error(t, err)
				require.Zero(t, receipt)
			}
			after, readErr := backend.GetRun(ctx, req.Version.RunID)
			require.NoError(t, readErr)
			page, eventErr := repo.ReadPublicEvents(ctx, event.ReadRequest{After: event.Cursor{SchemaVersion: 1, RunID: req.Version.RunID}, Limit: 10})
			require.NoError(t, eventErr)
			if !committed {
				require.Equal(t, before, after)
				require.Empty(t, page.Events)
				return
			}
			require.Equal(t, run.StateIncomplete, after.State)
			require.Equal(t, run.StopExecutionError, after.StopReason)
			require.Equal(t, verification.StatusUnchecked, after.VerificationStatus)
			require.Empty(t, after.Output)
			require.Empty(t, after.ResultRef)
			require.Equal(t, before.Usage, after.Usage)
			require.Equal(t, before.Budget, after.Budget)
			require.Equal(t, before.Provenance, after.Provenance)
			require.Equal(t, req.RunRevision+1, after.Revision)
			require.Len(t, page.Events, 1)
			require.Equal(t, event.ExecutionFinished, page.Events[0].Type)
			late := assurance.New(driver).WithClock(rosterClock{at: req.ExpiresAt.Add(time.Hour)})
			retry, retryErr := late.CloseResume(ctx, req)
			require.NoError(t, retryErr)
			require.False(t, retry.Fresh)
			require.NoError(t, retry.Validate())
			require.Equal(t, after.Revision, retry.RunRevision)
			changed := req
			changed.ProofRef = "different proof"
			_, retryErr = closer.CloseResume(ctx, changed)
			require.ErrorIs(t, retryErr, suspension.ErrResumeAdmissionConflict)
			_, retryErr = closer.GetResumeClosure(ctxWithScope("other"), req.Version.RunID, req.Version.SuspensionID)
			require.Error(t, retryErr)
			// A late original worker cannot overwrite closure with its old revision.
			before.State = run.StateCompleted
			before.Output = "stale worker"
			require.ErrorIs(t, backend.UpdateRun(ctx, before), cortex.ErrRunConflict)
			start, startErr := repo.StartResume(ctx, req.Version.RunID, req.Version.SuspensionID, req.AdmissionDigest)
			require.NoError(t, startErr)
			require.False(t, start.Fresh)
			require.Nil(t, start.Suspension)
		})
	}
	t.Run("competing_closures", func(t *testing.T) {
		ctx, repo, req := closureFixture(t, backend, driver, true)
		closer, ok := any(repo).(suspension.ResumeClosureStore)
		require.True(t, ok)
		var wg sync.WaitGroup
		var receipts [2]suspension.ResumeClosure
		var errs [2]error
		for i := range 2 {
			wg.Go(func() {
				offered := req
				if i == 1 {
					offered.ProofRef = "other proof"
				}
				receipts[i], errs[i] = closer.CloseResume(ctx, offered)
			})
		}
		wg.Wait()
		require.NotEqual(t, receipts[0].Fresh, receipts[1].Fresh)
		for i := range 2 {
			if receipts[i].Fresh {
				require.NoError(t, errs[i])
			} else {
				require.ErrorIs(t, errs[i], suspension.ErrResumeAdmissionConflict)
			}
		}
	})
}
