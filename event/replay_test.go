package event_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
)

func TestReplayCursorRoundTripAndStrictParsing(t *testing.T) {
	runID := id.NewAgentRunID()
	for _, seq := range []uint64{0, 1, 9007199254740993, math.MaxInt64} {
		cursor := event.Cursor{SchemaVersion: 1, RunID: runID, Sequence: seq}
		token, err := event.EncodeCursor(cursor)
		require.NoError(t, err)
		got, err := event.DecodeCursor(runID, token)
		require.NoError(t, err)
		require.Equal(t, cursor, got)
	}
	prefix := "1." + runID.String() + "."
	for _, token := range []string{"1", prefix + "01", prefix + "+1", prefix + "-1", prefix + "1.0", prefix + "9223372036854775808", "2." + runID.String() + ".1", "1." + id.NewAgentRunID().String() + ".1", strings.Repeat("a", 256)} {
		_, err := event.DecodeCursor(runID, token)
		require.ErrorIs(t, err, event.ErrInvalid, token)
	}
	cursor, err := event.DecodeCursor(runID, "")
	require.NoError(t, err)
	require.Zero(t, cursor.Sequence)
	_, err = event.DecodeCursor(id.AgentRunID{}, "")
	require.ErrorIs(t, err, event.ErrInvalid)
}

func TestReplayPagePreservesGapAndRejectsMalformedStorage(t *testing.T) {
	ctx, req, page := replayFixture()
	reader := replayStorage(func(context.Context, event.ReadRequest) (event.Page, error) { return page, nil })
	got, err := event.ReadReplayPage(ctx, reader, req.After.RunID, "", 0)
	require.NoError(t, err)
	require.False(t, got.Gap)
	require.Equal(t, page, got.Page)
	cursor, err := event.DecodeCursor(req.After.RunID, got.NextCursor)
	require.NoError(t, err)
	require.Equal(t, page.Next, cursor)
	page.Events = nil
	page.Watermark.FirstSequence = 2
	reader = replayStorage(func(context.Context, event.ReadRequest) (event.Page, error) { return page, event.ErrCursorGap })
	got, err = event.ReadReplayPage(ctx, reader, req.After.RunID, "", 10)
	require.NoError(t, err)
	require.True(t, got.Gap)
	require.Empty(t, got.Page.Events)
	page.Watermark.RunID = id.NewAgentRunID()
	got, err = event.ReadReplayPage(ctx, reader, req.After.RunID, "", 10)
	require.ErrorIs(t, err, event.ErrUnavailable)
	require.Zero(t, got)
}

func TestReplayInvalidInputNeverReads(t *testing.T) {
	ctx, req, _ := replayFixture()
	reader := replayStorage(func(context.Context, event.ReadRequest) (event.Page, error) {
		t.Fatal("storage read for invalid input")
		return event.Page{}, nil
	})
	for _, limit := range []int{-1, event.MaxPageSize + 1} {
		_, err := event.ReadReplayPage(ctx, reader, req.After.RunID, "", limit)
		require.ErrorIs(t, err, event.ErrInvalid)
	}
	_, err := event.ReadReplayPage(ctx, reader, req.After.RunID, "bad", 1)
	require.ErrorIs(t, err, event.ErrInvalid)
}

func TestReplayFailuresDoNotExposePrivateCauses(t *testing.T) {
	for _, cause := range []error{event.ErrInvalid, event.ErrAccessDenied, event.ErrUnavailable, errors.New("SECRET candidate and credentials")} {
		failure := event.DescribeReplayFailure(fmt.Errorf("SECRET provider: %w", cause))
		require.NotContains(t, failure.Message, "SECRET")
	}
	ctx, req, page := replayFixture()
	denied := errors.New("SECRET grant revoked")
	reader, err := event.NewReader(replayStorage(func(context.Context, event.ReadRequest) (event.Page, error) { return page, nil }), replayAuthorizer(func(context.Context, id.AgentRunID) error { return denied }))
	require.NoError(t, err)
	_, err = reader.Read(ctx, req)
	require.ErrorIs(t, err, denied)
	require.ErrorIs(t, err, event.ErrAccessDenied)
	require.Equal(t, "replay_denied", event.DescribeReplayFailure(err).Code)
}

func TestReplayFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		cause error
		code  string
	}{
		{fmt.Errorf("%w: %w", event.ErrUnavailable, event.ErrInvalid), "replay_unavailable"},
		{fmt.Errorf("%w: %w", event.ErrAccessDenied, event.ErrInvalid), "replay_denied"},
		{context.Canceled, "replay_interrupted"},
	} {
		require.Equal(t, tc.code, event.DescribeReplayFailure(tc.cause).Code)
	}
	ctx, req, page := replayFixture()
	for _, cause := range []error{event.ErrUnavailable, context.Canceled, context.DeadlineExceeded} {
		reader, err := event.NewReader(replayStorage(func(context.Context, event.ReadRequest) (event.Page, error) {
			t.Fatal("unauthorized storage read")
			return page, nil
		}), replayAuthorizer(func(context.Context, id.AgentRunID) error { return cause }))
		require.NoError(t, err)
		_, err = reader.Read(ctx, req)
		require.ErrorIs(t, err, cause)
		require.NotErrorIs(t, err, event.ErrAccessDenied)
	}
}
