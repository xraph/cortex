package event_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
)

func TestFollowReplayStopsOnConsumerFailure(t *testing.T) {
	ctx, req, page := replayFixture()
	calls := 0
	storage := replayStorage(func(context.Context, event.ReadRequest) (event.Page, error) { calls++; return page, nil })
	disconnected := errors.New("disconnected")
	err := event.FollowReplay(ctx, storage, event.FollowRequest{RunID: req.After.RunID}, func(context.Context, event.ReplayPage) error { return disconnected })
	require.ErrorIs(t, err, disconnected)
	require.Equal(t, 1, calls)
	require.NoError(t, ctx.Err())
}

func TestFollowReplayReauthorizesIdleSubscription(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, req, page := replayFixture()
		calls, sends := 0, 0
		denied := event.ErrAccessDenied
		storage := replayStorage(func(_ context.Context, r event.ReadRequest) (event.Page, error) {
			calls++
			if calls == 2 {
				require.Equal(t, page.Next, r.After)
				return event.Page{}, denied
			}
			return page, nil
		})
		started := time.Now()
		err := event.FollowReplay(ctx, storage, event.FollowRequest{RunID: req.After.RunID}, func(context.Context, event.ReplayPage) error { sends++; return nil })
		require.ErrorIs(t, err, denied)
		require.Equal(t, 1, sends)
		require.Equal(t, time.Second, time.Since(started))
	})
}

func TestFollowReplayGapStopsWithoutAnotherRead(t *testing.T) {
	ctx, req, page := replayFixture()
	page.Events = nil
	page.Watermark.FirstSequence = 2
	calls, sends := 0, 0
	err := event.FollowReplay(ctx, replayStorage(func(context.Context, event.ReadRequest) (event.Page, error) { calls++; return page, event.ErrCursorGap }), event.FollowRequest{RunID: req.After.RunID}, func(_ context.Context, p event.ReplayPage) error { require.True(t, p.Gap); sends++; return nil })
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Equal(t, 1, sends)
}

func TestFollowReplayDeadlineAndCancelledRead(t *testing.T) {
	for _, slowRead := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			ctx, req, page := replayFixture()
			calls, sends := 0, 0
			started := time.Now()
			storage := replayStorage(func(ctx context.Context, r event.ReadRequest) (event.Page, error) {
				calls++
				if slowRead {
					<-ctx.Done()
					return event.Page{}, ctx.Err()
				}
				page.Events = nil
				page.Watermark.LastSequence = 0
				page.Next = r.After
				return page, nil
			})
			err := event.FollowReplay(ctx, storage, event.FollowRequest{RunID: req.After.RunID, Lifetime: 3 * time.Second, ReadTimeout: 2 * time.Second}, func(context.Context, event.ReplayPage) error { sends++; return nil })
			require.ErrorIs(t, err, context.DeadlineExceeded)
			if slowRead {
				require.Equal(t, 2*time.Second, time.Since(started))
				require.Zero(t, sends)
				require.Equal(t, 1, calls)
			} else {
				require.Equal(t, 3*time.Second, time.Since(started))
				require.GreaterOrEqual(t, sends, 2)
				require.LessOrEqual(t, sends, 3)
			}
		})
	}
}

func TestFollowReplayRejectsInvalidBoundsBeforeReading(t *testing.T) {
	ctx, req, _ := replayFixture()
	for _, in := range []event.FollowRequest{
		{RunID: req.After.RunID, PollInterval: time.Nanosecond},
		{RunID: req.After.RunID, PollInterval: time.Minute},
		{RunID: req.After.RunID, Lifetime: time.Hour},
		{RunID: req.After.RunID, ReadTimeout: -1},
		{RunID: req.After.RunID, After: "foreign"},
		{RunID: req.After.RunID, Limit: 129},
	} {
		err := event.FollowReplay(ctx, replayStorage(func(context.Context, event.ReadRequest) (event.Page, error) {
			t.Fatal("invalid request read")
			return event.Page{}, nil
		}), in, func(context.Context, event.ReplayPage) error { t.Fatal("invalid request sent"); return nil })
		require.ErrorIs(t, err, event.ErrInvalid)
	}
}

func TestFollowReplayDrainsBacklogWithoutPollingDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base, req, page := replayFixture()
		ctx, cancel := context.WithCancel(base)
		defer cancel()
		page.Watermark.LastSequence = 2
		reads, sends := 0, 0
		started := time.Now()
		err := event.FollowReplay(ctx, replayStorage(func(_ context.Context, r event.ReadRequest) (event.Page, error) {
			reads++
			page.Next.Sequence = r.After.Sequence + 1
			page.Events[0].ID = id.New(event.PrefixEvent)
			page.Events[0].Sequence = page.Next.Sequence
			return page, nil
		}), event.FollowRequest{RunID: req.After.RunID, Limit: 1}, func(_ context.Context, p event.ReplayPage) error {
			sends++
			require.Equal(t, uint64(sends), p.Page.Next.Sequence)
			if sends == 2 {
				cancel()
			}
			return nil
		})
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, 2, reads)
		require.Equal(t, 2, sends)
		require.Zero(t, time.Since(started))
	})
}

func TestFollowReplayDoesNotReadAheadOfSlowConsumer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, req, page := replayFixture()
		reads := 0
		err := event.FollowReplay(ctx, replayStorage(func(context.Context, event.ReadRequest) (event.Page, error) { reads++; return page, nil }), event.FollowRequest{RunID: req.After.RunID, Lifetime: time.Second}, func(ctx context.Context, _ event.ReplayPage) error { <-ctx.Done(); return ctx.Err() })
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, 1, reads)
	})
}
