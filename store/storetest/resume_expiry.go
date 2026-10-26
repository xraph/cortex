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
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/suspension"
	"github.com/xraph/cortex/verification"
)

func expiryFixture(t *testing.T, backend store.Store, driver assurance.Driver) (ctx context.Context, live, expired *assurance.Repository, admitted suspension.ResumeAdmission) {
	t.Helper()
	ctx, _, req := admissionFixture(t, backend)
	paused, err := backend.GetSuspension(ctx, req.Version.RunID)
	require.NoError(t, err)
	now := time.Now().UTC()
	deadline := now.Add(time.Minute)
	paused.ExpiresAt = &deadline
	require.NoError(t, backend.DeleteSuspension(ctx, req.Version.RunID))
	require.NoError(t, backend.CreateSuspension(ctx, paused))
	live = assurance.New(driver).WithClock(rosterClock{at: now})
	expired = assurance.New(driver).WithClock(rosterClock{at: deadline})
	admitted, err = live.AdmitResume(ctx, req)
	require.NoError(t, err)
	return ctx, live, expired, admitted
}

// ResumeExpiryTransactions exercises expiry against each native transaction driver.
func ResumeExpiryTransactions(t *testing.T, backend store.Store, driver assurance.Driver) {
	t.Helper()
	for _, mode := range []string{"expired", "early", "started", "revision", "replaced_pause", "wrong_scope", "wrong_digest", "cancelled", "rollback", "lost_ack"} {
		t.Run(mode, func(t *testing.T) {
			ctx, live, expired, admitted := expiryFixture(t, backend, driver)
			key := admitted.Request.Version
			attempt := ctx
			digest := admitted.RequestDigest
			writer := expired
			switch mode {
			case "early":
				writer = live
			case "started":
				_, err := live.StartResume(ctx, key.RunID, key.SuspensionID, digest)
				require.NoError(t, err)
			case "revision":
				current, err := backend.GetRun(ctx, key.RunID)
				require.NoError(t, err)
				require.NoError(t, backend.UpdateRun(ctx, current))
			case "replaced_pause":
				paused, err := backend.GetSuspension(ctx, key.RunID)
				require.NoError(t, err)
				require.NoError(t, backend.DeleteSuspension(ctx, key.RunID))
				paused.Cont.SystemPrompt = "changed"
				require.NoError(t, backend.CreateSuspension(ctx, paused))
			case "wrong_scope":
				attempt = ctxWithScope("other")
			case "wrong_digest":
				digest = "changed"
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				attempt = cancelled
			case "rollback", "lost_ack":
				tx, ok := driver.(assurance.RunDriver)
				require.True(t, ok)
				fault := errors.New("expiry commit interrupted")
				var wrapped assurance.Driver = failingRunDriver{Driver: driver, transaction: tx, failure: fault}
				if mode == "lost_ack" {
					wrapped = privateLostReplyDriver{Driver: driver, transaction: tx, failure: fault}
				}
				writer = assurance.New(wrapped).WithClock(rosterClock{at: admitted.AdmittedAt.Add(time.Minute)})
			}
			receipt, err := writer.ExpireResumeAdmission(attempt, key.RunID, key.SuspensionID, digest)
			if mode != "expired" {
				require.Error(t, err)
				require.Zero(t, receipt)
				if mode == "started" {
					require.ErrorIs(t, err, suspension.ErrResumeAlreadyStarted)
				}
			} else {
				require.NoError(t, err)
				require.True(t, receipt.Fresh)
			}
			page, eventErr := expired.ReadPublicEvents(ctx, event.ReadRequest{After: event.Cursor{SchemaVersion: 1, RunID: key.RunID}, Limit: 10})
			require.NoError(t, eventErr)
			current, readErr := backend.GetRun(ctx, key.RunID)
			require.NoError(t, readErr)
			if mode == "expired" || mode == "lost_ack" {
				require.Len(t, page.Events, 1)
				require.Equal(t, event.ExecutionFinished, page.Events[0].Type)
				require.Equal(t, run.StateIncomplete, current.State)
				require.Equal(t, run.StopDeadline, current.StopReason)
				require.Empty(t, current.Output)
				require.Equal(t, verification.StatusUnchecked, current.VerificationStatus)
				require.Equal(t, admitted.ClaimedRevision+1, current.Revision)
				retry, retryErr := expired.ExpireResumeAdmission(ctx, key.RunID, key.SuspensionID, admitted.RequestDigest)
				require.NoError(t, retryErr)
				require.False(t, retry.Fresh)
				require.Equal(t, current.Revision, retry.RunRevision)
				_, startErr := live.StartResume(ctx, key.RunID, key.SuspensionID, admitted.RequestDigest)
				require.ErrorIs(t, startErr, cortex.ErrSuspensionExpired)
			} else {
				require.Empty(t, page.Events)
				require.Equal(t, run.StateRunning, current.State)
			}
		})
	}
	t.Run("start_races_expiry", func(t *testing.T) {
		ctx, live, expired, admitted := expiryFixture(t, backend, driver)
		key := admitted.Request.Version
		var wg sync.WaitGroup
		var start suspension.ResumeStart
		var expiry suspension.ResumeExpiry
		var startErr, expiryErr error
		wg.Go(func() { start, startErr = live.StartResume(ctx, key.RunID, key.SuspensionID, admitted.RequestDigest) })
		wg.Go(func() {
			expiry, expiryErr = expired.ExpireResumeAdmission(ctx, key.RunID, key.SuspensionID, admitted.RequestDigest)
		})
		wg.Wait()
		require.NotEqual(t, start.Fresh, expiry.Fresh)
		if start.Fresh {
			require.NoError(t, startErr)
			require.ErrorIs(t, expiryErr, suspension.ErrResumeAlreadyStarted)
		} else {
			require.NoError(t, expiryErr)
			require.ErrorIs(t, startErr, cortex.ErrSuspensionExpired)
		}
	})
}
