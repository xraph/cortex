package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

func terminalFixture(t *testing.T, s store.Store) (context.Context, event.TerminalUpdate) {
	t.Helper()
	ctx := ctxWithScope("terminal")
	owner := &agent.Config{ID: id.NewAgentID(), Name: id.NewAgentID().String()}
	require.NoError(t, s.Create(ctx, owner))
	r := &run.Run{ID: id.NewAgentRunID(), AgentID: owner.ID, State: run.StateRunning}
	require.NoError(t, s.CreateRun(ctx, r))
	at := time.Now().UTC()
	r.State = run.StateCompleted
	r.CompletedAt = &at
	r.Output = "PRIVATE result"
	return ctx, event.TerminalUpdate{SchemaVersion: 1, EventID: id.New(event.PrefixEvent), Run: *r}
}

func testTerminalEvents(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	terminal := resultCapability[event.TerminalStore](t, s)
	events := resultCapability[event.Store](t, s)
	for _, state := range []run.State{run.StateCompleted, run.StateFailed, run.StateIncomplete, run.StateCancelled} {
		ctx, request := terminalFixture(t, s)
		request.Run.State = state
		before := request.Run.Revision
		receipt, err := terminal.CommitTerminalRun(ctx, request)
		require.NoError(t, err)
		require.Equal(t, before+1, receipt.RunRevision)
		require.Equal(t, before, request.Run.Revision)
		saved, err := s.GetRun(ctx, request.Run.ID)
		require.NoError(t, err)
		require.Equal(t, state, saved.State)
		require.Equal(t, receipt.RunRevision, saved.Revision)
		retry, err := terminal.CommitTerminalRun(ctx, request)
		require.NoError(t, err)
		require.Equal(t, receipt, retry)
		page, err := events.ReadPublicEvents(ctx, event.ReadRequest{After: event.Cursor{SchemaVersion: 1, RunID: saved.ID}, Limit: 10})
		require.NoError(t, err)
		require.Len(t, page.Events, 1)
		require.Equal(t, request.EventID, page.Events[0].ID)
		require.Equal(t, saved.Revision, page.Events[0].RunRevision)
		body, err := json.Marshal(page)
		require.NoError(t, err)
		require.NotContains(t, string(body), "PRIVATE")
		request.Run.Output = "altered"
		_, err = terminal.CommitTerminalRun(ctx, request)
		require.ErrorIs(t, err, event.ErrConflict)
		request.EventID = id.New(event.PrefixEvent)
		request.Run.Revision = saved.Revision
		_, err = terminal.CommitTerminalRun(ctx, request)
		require.ErrorIs(t, err, cortex.ErrRunConflict)
	}
	ctx, request := terminalFixture(t, s)
	_, err := terminal.CommitTerminalRun(context.Background(), request)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	_, err = terminal.CommitTerminalRun(ctxWithScope("other"), request)
	require.Error(t, err)
	request.Run.Revision++
	_, err = terminal.CommitTerminalRun(ctx, request)
	require.ErrorIs(t, err, cortex.ErrRunConflict)
	watermark, err := events.ReadEventWatermark(ctx, request.Run.ID)
	require.NoError(t, err)
	require.Zero(t, watermark.LastSequence)
}

// TerminalEventRollback proves terminal state, receipt and replay share rollback.
func TerminalEventRollback(t *testing.T, s store.Store, driver assurance.Driver) {
	t.Helper()
	transaction, ok := driver.(assurance.RunDriver)
	require.True(t, ok)
	failure := errors.New("failure after terminal state and event writes")
	repository := assurance.New(failingRunDriver{Driver: driver, transaction: transaction, failure: failure})
	ctx, request := terminalFixture(t, s)
	receipt, err := repository.CommitTerminalRun(ctx, request)
	require.ErrorIs(t, err, failure)
	require.Zero(t, receipt)
	saved, err := s.GetRun(ctx, request.Run.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateRunning, saved.State)
	require.Equal(t, request.Run.Revision, saved.Revision)
	watermark, err := resultCapability[event.Store](t, s).ReadEventWatermark(ctx, saved.ID)
	require.NoError(t, err)
	require.Zero(t, watermark.LastSequence)
	receipt, err = resultCapability[event.TerminalStore](t, s).CommitTerminalRun(ctx, request)
	require.NoError(t, err)
	require.Equal(t, saved.Revision+1, receipt.RunRevision)
	// A lost acknowledgement must leave one durable transition and permit an exact retry.
	ctx, request = terminalFixture(t, s)
	lost := assurance.New(privateLostReplyDriver{Driver: driver, transaction: transaction, failure: failure})
	_, err = lost.CommitTerminalRun(ctx, request)
	require.ErrorIs(t, err, failure)
	receipt, err = resultCapability[event.TerminalStore](t, s).CommitTerminalRun(ctx, request)
	require.NoError(t, err)
	watermark, err = resultCapability[event.Store](t, s).ReadEventWatermark(ctx, request.Run.ID)
	require.NoError(t, err)
	require.Equal(t, uint64(1), watermark.LastSequence)
	require.NoError(t, resultCapability[event.Store](t, s).PrunePublicEvents(ctx, event.Cursor{SchemaVersion: 1, RunID: request.Run.ID, Sequence: 1}))
	retry, err := resultCapability[event.TerminalStore](t, s).CommitTerminalRun(ctx, request)
	require.NoError(t, err)
	require.Equal(t, receipt, retry)
	watermark, err = resultCapability[event.Store](t, s).ReadEventWatermark(ctx, request.Run.ID)
	require.NoError(t, err)
	require.Equal(t, uint64(1), watermark.LastSequence)
}

func testTerminalEventsConcurrent(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, request := terminalFixture(t, s)
	storage := resultCapability[event.TerminalStore](t, s)
	results := make(chan error, 4)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			req := request
			req.EventID = id.New(event.PrefixEvent)
			_, err := storage.CommitTerminalRun(ctx, req)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			require.ErrorIs(t, err, cortex.ErrRunConflict)
		}
	}
	require.Equal(t, 1, successes)
	watermark, err := resultCapability[event.Store](t, s).ReadEventWatermark(ctx, request.Run.ID)
	require.NoError(t, err)
	require.Equal(t, uint64(1), watermark.LastSequence)
}
