package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
)

type publicEventAuthorizer func(context.Context, id.AgentRunID) error

func (a publicEventAuthorizer) AuthorizePublicReplay(ctx context.Context, runID id.AgentRunID) error {
	return a(ctx, runID)
}

func TestCommittedResultReplayAfterRestart(t *testing.T) {
	f := newWorkerFixture(t)
	result, err := f.engine(t, f.storage).VerifyRun(f.ctx, f.request)
	require.NoError(t, err)
	reopened := openSQLiteStoreForTest(t, f.path)
	calls := 0
	reader, err := engine.New(engine.WithStore(reopened), engine.WithPublicEventReplay(publicEventAuthorizer(func(_ context.Context, runID id.AgentRunID) error {
		require.Equal(t, f.request.Job.Binding.RunID, runID)
		calls++
		return nil
	})))
	require.NoError(t, err)
	req := event.ReadRequest{After: event.Cursor{SchemaVersion: 1, RunID: f.request.Job.Binding.RunID}, Limit: 10}
	page, err := reader.ReadPublicEvents(f.ctx, req)
	require.NoError(t, err)
	require.Len(t, page.Events, 1)
	require.Equal(t, result.Receipt.Outbox.EventID, page.Events[0].ID)
	require.Equal(t, result.Receipt.Request.ResultID, page.Events[0].ResultID)
	require.Equal(t, 2, calls)
	retry, err := reader.ReadPublicEvents(f.ctx, req)
	require.NoError(t, err)
	require.Equal(t, page, retry)
	require.Equal(t, 1, f.adapter.evaluations)
	req.After = page.Next
	next, err := reader.ReadPublicEvents(f.ctx, req)
	require.NoError(t, err)
	require.Empty(t, next.Events)
}

func TestPublicReplayReauthorizesSubscriber(t *testing.T) {
	for _, denyAt := range []int{1, 2} {
		f := newWorkerFixture(t)
		_, err := f.engine(t, f.storage).VerifyRun(f.ctx, f.request)
		require.NoError(t, err)
		calls := 0
		denied := errors.New("revoked")
		reader, err := engine.New(engine.WithStore(f.storage), engine.WithPublicEventReplay(publicEventAuthorizer(func(context.Context, id.AgentRunID) error {
			calls++
			if calls == denyAt {
				return denied
			}
			return nil
		})))
		require.NoError(t, err)
		page, err := reader.ReadPublicEvents(f.ctx, event.ReadRequest{After: event.Cursor{SchemaVersion: 1, RunID: f.request.Job.Binding.RunID}, Limit: 10})
		require.ErrorIs(t, err, denied)
		require.Zero(t, page)
	}
}

func TestPublicReplayRequiresExplicitHostAuthority(t *testing.T) {
	f := newWorkerFixture(t)
	reader, err := engine.New(engine.WithStore(f.storage))
	require.NoError(t, err)
	_, err = reader.ReadPublicEvents(f.ctx, event.ReadRequest{})
	require.ErrorIs(t, err, event.ErrUnavailable)
	var auth publicEventAuthorizer
	_, err = engine.New(engine.WithStore(f.storage), engine.WithPublicEventReplay(auth))
	require.Error(t, err)
}
