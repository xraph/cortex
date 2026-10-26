package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/suspension"
)

func testSuspensionVersionClaim(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx := ctxWithScope("versioned-resume")
	current, paused := mustSuspendRun(t, backend, ctx, nil)
	versioned, ok := backend.(suspension.VersionedStore)
	require.True(t, ok)
	req := suspension.ClaimVersion{RunID: current.ID, SuspensionID: paused.ID, RunRevision: current.Revision}
	// A later pause with identical call IDs cannot receive this answer.
	require.NoError(t, backend.DeleteSuspension(ctx, current.ID))
	replacement := *paused
	replacement.ID = id.NewSuspensionID()
	require.NoError(t, backend.CreateSuspension(ctx, &replacement))
	_, err := versioned.ClaimSuspensionVersion(ctx, req)
	require.ErrorIs(t, err, cortex.ErrNotSuspended)
	retained, err := backend.GetRun(ctx, current.ID)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, retained.State)
	req.SuspensionID = replacement.ID
	req.RunRevision++
	_, err = versioned.ClaimSuspensionVersion(ctx, req)
	require.ErrorIs(t, err, cortex.ErrRunConflict)
	req.RunRevision--
	_, err = versioned.ClaimSuspensionVersion(context.Background(), req)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	var wg sync.WaitGroup
	results := make(chan *suspension.Suspension, 4)
	failures := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			result, claimErr := versioned.ClaimSuspensionVersion(ctx, req)
			results <- result
			failures <- claimErr
		})
	}
	wg.Wait()
	close(results)
	close(failures)
	winners := 0
	for err = range failures {
		if err == nil {
			winners++
		} else {
			require.ErrorIs(t, err, cortex.ErrNotSuspended)
		}
	}
	require.Equal(t, 1, winners)
	for result := range results {
		if result == nil {
			continue
		}
		require.Equal(t, replacement.ID, result.ID)
		require.NotNil(t, result.ClaimedRun)
		require.Equal(t, current.Revision+1, result.ClaimedRun.Revision)
		require.Equal(t, run.StateRunning, result.ClaimedRun.State)
	}
}

func testSuspensionQuestionBinding(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx := ctxWithScope("question-binding")
	current, paused := mustSuspendRun(t, backend, ctx, nil)
	require.NoError(t, backend.DeleteSuspension(ctx, current.ID))
	paused.Reason = suspension.ReasonAgentReply
	paused.Pending[0].Name = "agent_ask"
	paused.Pending[0].ReplyWith = id.NewMessageID().String()
	require.NoError(t, backend.CreateSuspension(ctx, paused))
	loaded, err := backend.GetSuspension(ctx, current.ID)
	require.NoError(t, err)
	require.Equal(t, paused.Pending, loaded.Pending)
	versioned, ok := backend.(suspension.VersionedStore)
	require.True(t, ok)
	claimed, err := versioned.ClaimSuspensionVersion(ctx, suspension.ClaimVersion{RunID: current.ID, SuspensionID: paused.ID, RunRevision: current.Revision})
	require.NoError(t, err)
	require.Equal(t, paused.Pending, claimed.Pending)
}

func testSuspensionVersionExpiry(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx := ctxWithScope("versioned-expiry")
	at := time.Now().Add(-time.Minute)
	current, paused := mustSuspendRun(t, backend, ctx, &at)
	versioned, ok := backend.(suspension.VersionedStore)
	require.True(t, ok)
	_, err := versioned.ClaimSuspensionVersion(ctx, suspension.ClaimVersion{RunID: current.ID, SuspensionID: paused.ID, RunRevision: current.Revision})
	require.ErrorIs(t, err, cortex.ErrSuspensionExpired)
	retained, err := backend.GetRun(ctx, current.ID)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, retained.State)
	require.Equal(t, current.Revision, retained.Revision)
}

// SuspensionVersionTransactions verifies rollback and ambiguous claim commits.
func SuspensionVersionTransactions(t *testing.T, backend store.Store, driver assurance.Driver) {
	t.Helper()
	transaction, ok := driver.(assurance.RunDriver)
	require.True(t, ok)
	for _, lostAck := range []bool{false, true} {
		ctx := ctxWithScope(id.NewMessageID().String())
		current, paused := mustSuspendRun(t, backend, ctx, nil)
		req := suspension.ClaimVersion{RunID: current.ID, SuspensionID: paused.ID, RunRevision: current.Revision}
		failure := errors.New("injected claim commit failure")
		var faulty assurance.Driver = failingRunDriver{Driver: driver, transaction: transaction, failure: failure}
		if lostAck {
			faulty = privateLostReplyDriver{Driver: driver, transaction: transaction, failure: failure}
		}
		result, err := assurance.New(faulty).ClaimSuspensionVersion(ctx, req)
		require.ErrorIs(t, err, failure)
		require.Nil(t, result, "ambiguous acknowledgment cannot grant execution")
		retained, err := backend.GetRun(ctx, current.ID)
		require.NoError(t, err)
		if lostAck {
			require.Equal(t, run.StateRunning, retained.State)
			require.Equal(t, current.Revision+1, retained.Revision)
			result, err = assurance.New(driver).ClaimSuspensionVersion(ctx, req)
			require.ErrorIs(t, err, cortex.ErrNotSuspended)
			require.Nil(t, result)
			// A later pause must remain untouched by the original retry.
			retained.State = run.StatePaused
			require.NoError(t, backend.UpdateRun(ctx, retained))
			require.NoError(t, backend.DeleteSuspension(ctx, current.ID))
			replacement := newSuspension(current.ID)
			require.NoError(t, backend.CreateSuspension(ctx, replacement))
			result, err = assurance.New(driver).ClaimSuspensionVersion(ctx, req)
			require.ErrorIs(t, err, cortex.ErrRunConflict)
			require.Nil(t, result)
			later, readErr := backend.GetRun(ctx, current.ID)
			require.NoError(t, readErr)
			require.Equal(t, run.StatePaused, later.State)
		} else {
			require.Equal(t, run.StatePaused, retained.State)
			require.Equal(t, current.Revision, retained.Revision)
			result, err = assurance.New(driver).ClaimSuspensionVersion(ctx, req)
			require.NoError(t, err)
			require.Equal(t, paused.ID, result.ID)
		}
	}
}

func testSuspensionVersionScope(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx := ctxWithScope("versioned", "workspace", "private")
	other := ctxWithScope("versioned", "workspace", "other")
	current, paused := mustSuspendRun(t, backend, ctx, nil)
	versioned, ok := backend.(suspension.VersionedStore)
	require.True(t, ok)
	req := suspension.ClaimVersion{RunID: current.ID, SuspensionID: paused.ID, RunRevision: current.Revision}
	for _, other := range []context.Context{other, ctxWithScope("versioned", "workspace")} {
		result, err := versioned.ClaimSuspensionVersion(other, req)
		require.ErrorIs(t, err, cortex.ErrRunNotFound)
		require.Nil(t, result)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	result, err := versioned.ClaimSuspensionVersion(cancelled, req)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, result)
	result, err = versioned.ClaimSuspensionVersion(ctx, req)
	require.NoError(t, err)
	require.Equal(t, paused.ID, result.ID)
}
