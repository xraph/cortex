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
	require.Len(t, page.Events, 5)
	requireLifecycle(t, page, event.ExecutionFinished, event.VerificationPending, event.VerificationPassed, event.ArtifactCreated, event.ResultCommitted)
	require.Equal(t, receipt.Outbox.EventID, page.Events[4].ID)
	require.Equal(t, receipt.RunRevision, page.Events[4].RunRevision)
	require.Equal(t, uint64(5), page.Next.Sequence)
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
	require.Equal(t, uint64(5), watermark.LastSequence)
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
	require.Len(t, page.Events, 3)
	require.Equal(t, receipt.Outbox.EventID, page.Events[2].ID)
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

func testArtifactEventBinding(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, request := resultFixture(t, s)
	es := resultCapability[event.Store](t, s)
	input := event.AppendRequest{SchemaVersion: 1, RunID: request.Current.RunID, ExpectedRunRevision: request.ExpectedRunRevision, Events: []event.Input{{ID: id.New(event.PrefixEvent), Type: event.ArtifactCreated, ResultID: request.ResultID}}}
	_, err := es.AppendPublicEvents(ctx, input)
	require.Error(t, err, "private candidate must not be announced before commit")
	require.Empty(t, readLifecycle(ctx, t, s, request.Current.RunID).Events)
	receipt, err := resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, request)
	require.NoError(t, err)
	before := readLifecycle(ctx, t, s, request.Current.RunID)
	requireLifecycle(t, before, event.VerificationPassed, event.ArtifactCreated, event.ResultCommitted)
	body, err := json.Marshal(before)
	require.NoError(t, err)
	require.NotContains(t, string(body), request.Current.Candidate.ID.String())
	require.NotContains(t, string(body), request.Current.Candidate.SHA256)
	require.NotContains(t, string(body), "private candidate text")
	// A committed result from a sibling run is still not this run's artifact.
	_, sibling := resultFixture(t, s)
	input.RunID, input.ExpectedRunRevision = sibling.Current.RunID, sibling.ExpectedRunRevision
	_, err = es.AppendPublicEvents(ctx, input)
	require.ErrorIs(t, err, event.ErrConflict)
	require.Empty(t, readLifecycle(ctx, t, s, sibling.Current.RunID).Events)
	// A later run revision cannot mint another announcement for an old receipt.
	current, err := s.GetRun(ctx, request.Current.RunID)
	require.NoError(t, err)
	require.NoError(t, s.UpdateRun(ctx, current))
	input.RunID, input.ExpectedRunRevision, input.AfterSequence = current.ID, current.Revision, before.Next.Sequence
	_, err = es.AppendPublicEvents(ctx, input)
	require.ErrorIs(t, err, event.ErrConflict)
	after := readLifecycle(ctx, t, s, current.ID)
	require.Equal(t, before.Events, after.Events)
	require.Equal(t, receipt.RunRevision+1, after.Watermark.RunRevision)
	// Pruning never turns a retried commit into a new artifact announcement.
	require.NoError(t, es.PrunePublicEvents(ctx, before.Next))
	_, err = resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, request)
	require.NoError(t, err)
	page, err := es.ReadPublicEvents(ctx, event.ReadRequest{After: before.Next, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, page.Events)
	require.Equal(t, before.Next.Sequence, page.Watermark.LastSequence)
}
