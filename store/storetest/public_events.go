package storetest

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

func testPublicEvents(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, req := resultFixture(t, s)
	es := resultCapability[event.Store](t, s)
	input := event.AppendRequest{SchemaVersion: 1, RunID: req.Current.RunID, ExpectedRunRevision: req.ExpectedRunRevision, Events: []event.Input{{ID: id.New(event.PrefixEvent), Type: event.ExecutionFinished}, {ID: id.New(event.PrefixEvent), Type: event.VerificationPending}}}
	saved, err := es.AppendPublicEvents(ctx, input)
	require.NoError(t, err)
	require.Equal(t, uint64(2), saved.LastSequence)
	receipt, err := resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, req)
	require.NoError(t, err)
	replay := event.ReadRequest{After: event.Cursor{SchemaVersion: 1, RunID: req.Current.RunID}, Limit: 10}
	page, err := es.ReadPublicEvents(ctx, replay)
	require.NoError(t, err)
	require.Len(t, page.Events, 3)
	require.Equal(t, []event.Type{event.ExecutionFinished, event.VerificationPending, event.ResultCommitted}, []event.Type{page.Events[0].Type, page.Events[1].Type, page.Events[2].Type})
	require.Equal(t, receipt.Outbox.EventID, page.Events[2].ID)
	require.Equal(t, receipt.RunRevision, page.Events[2].RunRevision)
	require.Equal(t, uint64(3), page.Next.Sequence)
	body, err := json.Marshal(page)
	require.NoError(t, err)
	require.NotContains(t, string(body), "private candidate text")
	require.NotContains(t, string(body), "operator")
	retry, err := es.AppendPublicEvents(ctx, input)
	require.NoError(t, err)
	require.Equal(t, saved, retry)
	_, err = resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, req)
	require.NoError(t, err)
	watermark, err := es.ReadEventWatermark(ctx, req.Current.RunID)
	require.NoError(t, err)
	require.Equal(t, uint64(3), watermark.LastSequence)
	_, err = es.ReadPublicEvents(ctxWithScope("other"), replay)
	require.ErrorIs(t, err, cortex.ErrRunNotFound)
	_, err = es.ReadPublicEvents(context.Background(), replay)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	input.Events[0].Type = event.VerificationPending
	_, err = es.AppendPublicEvents(ctx, input)
	require.ErrorIs(t, err, event.ErrConflict)
	require.NoError(t, es.PrunePublicEvents(ctx, event.Cursor{SchemaVersion: 1, RunID: req.Current.RunID, Sequence: 2}))
	gap, err := es.ReadPublicEvents(ctx, replay)
	require.ErrorIs(t, err, event.ErrCursorGap)
	require.Empty(t, gap.Events)
	require.Equal(t, uint64(3), gap.Watermark.FirstSequence)
	replay.After.Sequence = 2
	page, err = es.ReadPublicEvents(ctx, replay)
	require.NoError(t, err)
	require.Len(t, page.Events, 1)
	require.Equal(t, receipt.Outbox.EventID, page.Events[0].ID)
}

func testPublicEventsConcurrent(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, req := resultFixture(t, s)
	es := resultCapability[event.Store](t, s)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, err := es.AppendPublicEvents(ctx, event.AppendRequest{SchemaVersion: 1, RunID: req.Current.RunID, ExpectedRunRevision: req.ExpectedRunRevision, Events: []event.Input{{ID: id.New(event.PrefixEvent), Type: event.ExecutionFinished}}})
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
			require.ErrorIs(t, err, event.ErrConflict)
		}
	}
	require.Equal(t, 1, successes)
	watermark, err := es.ReadEventWatermark(ctx, req.Current.RunID)
	require.NoError(t, err)
	require.Equal(t, uint64(1), watermark.LastSequence)
}

func testPublicEventsRetention(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, req := resultFixture(t, s)
	es := resultCapability[event.Store](t, s)
	input := event.AppendRequest{SchemaVersion: 1, RunID: req.Current.RunID, ExpectedRunRevision: req.ExpectedRunRevision}
	var first event.AppendRequest
	for input.AfterSequence <= event.MaxRetainedEvents {
		input.Events = make([]event.Input, event.MaxBatchSize)
		for i := range input.Events {
			input.Events[i] = event.Input{ID: id.New(event.PrefixEvent), Type: event.ExecutionFinished}
		}
		if input.AfterSequence == 0 {
			first = input
		}
		receipt, err := es.AppendPublicEvents(ctx, input)
		require.NoError(t, err)
		input.AfterSequence = receipt.LastSequence
	}
	watermark, err := es.ReadEventWatermark(ctx, req.Current.RunID)
	require.NoError(t, err)
	require.Equal(t, uint64(event.MaxRetainedEvents), watermark.LastSequence+1-watermark.FirstSequence)
	retry, err := es.AppendPublicEvents(ctx, first)
	require.NoError(t, err)
	require.Equal(t, uint64(event.MaxBatchSize), retry.LastSequence)
	first.Events[0].Type = event.VerificationPending
	_, err = es.AppendPublicEvents(ctx, first)
	require.ErrorIs(t, err, event.ErrConflict)
	require.NoError(t, es.PrunePublicEvents(ctx, event.Cursor{SchemaVersion: 1, RunID: req.Current.RunID, Sequence: watermark.LastSequence}))
	page, err := es.ReadPublicEvents(ctx, event.ReadRequest{After: event.Cursor{SchemaVersion: 1, RunID: req.Current.RunID}, Limit: 10})
	require.ErrorIs(t, err, event.ErrCursorGap)
	require.Empty(t, page.Events)
	require.Equal(t, watermark.LastSequence+1, page.Watermark.FirstSequence)
	input.Events = []event.Input{{ID: id.New(event.PrefixEvent), Type: event.VerificationPending}}
	_, err = es.AppendPublicEvents(ctx, input)
	require.NoError(t, err)
	page, err = es.ReadPublicEvents(ctx, event.ReadRequest{After: page.Next, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Events, 1)
	require.Equal(t, watermark.LastSequence+1, page.Events[0].Sequence)
}

func testPublicEventsDenials(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, req := resultFixture(t, s)
	es := resultCapability[event.Store](t, s)
	input := event.AppendRequest{SchemaVersion: 1, RunID: req.Current.RunID, ExpectedRunRevision: req.ExpectedRunRevision, Events: []event.Input{{ID: id.New(event.PrefixEvent), Type: event.ExecutionFinished}, {ID: id.New(event.PrefixEvent), Type: event.VerificationFailed}}}
	_, err := es.AppendPublicEvents(ctx, input)
	require.ErrorIs(t, err, event.ErrConflict)
	watermark, err := es.ReadEventWatermark(ctx, req.Current.RunID)
	require.NoError(t, err)
	require.Zero(t, watermark.LastSequence)
	input.Events = input.Events[:1]
	for _, denied := range []context.Context{context.Background(), ctxWithScope("other")} {
		_, err = es.AppendPublicEvents(denied, input)
		require.Error(t, err)
		_, err = es.ReadEventWatermark(denied, req.Current.RunID)
		require.Error(t, err)
		require.Error(t, es.PrunePublicEvents(denied, event.Cursor{SchemaVersion: 1, RunID: req.Current.RunID}))
	}
	input.ExpectedRunRevision++
	_, err = es.AppendPublicEvents(ctx, input)
	require.ErrorIs(t, err, cortex.ErrRunConflict)
	input.ExpectedRunRevision--
	receipt, err := es.AppendPublicEvents(ctx, input)
	require.NoError(t, err, "invalid batch must roll back its first ID tombstone")
	require.Equal(t, uint64(1), receipt.LastSequence)
	_, err = es.ReadPublicEvents(ctx, event.ReadRequest{After: event.Cursor{SchemaVersion: 1, RunID: req.Current.RunID, Sequence: 2}, Limit: 10})
	require.ErrorIs(t, err, event.ErrInvalid)
	input.AfterSequence = 1
	input.Events = []event.Input{{ID: id.New(verification.PrefixCommitEvent), Type: event.ResultCommitted, ResultID: req.ResultID}}
	_, err = es.AppendPublicEvents(ctx, input)
	require.Error(t, err, "uncommitted result cannot enter replay")
	watermark, err = es.ReadEventWatermark(ctx, req.Current.RunID)
	require.NoError(t, err)
	require.Equal(t, uint64(1), watermark.LastSequence)
}
