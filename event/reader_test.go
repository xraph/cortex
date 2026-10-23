package event_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
)

type replayStorage func(context.Context, event.ReadRequest) (event.Page, error)

func (s replayStorage) ReadPublicEvents(ctx context.Context, r event.ReadRequest) (event.Page, error) {
	return s(ctx, r)
}

type replayAuthorizer func(context.Context, id.AgentRunID) error

func (a replayAuthorizer) AuthorizePublicReplay(ctx context.Context, runID id.AgentRunID) error {
	return a(ctx, runID)
}

func replayFixture() (context.Context, event.ReadRequest, event.Page) {
	runID := id.NewAgentRunID()
	req := event.ReadRequest{After: event.Cursor{SchemaVersion: 1, RunID: runID}, Limit: 10}
	page := event.Page{Watermark: event.Watermark{SchemaVersion: 1, RunID: runID, FirstSequence: 1, LastSequence: 1, RunRevision: 2}, Next: event.Cursor{SchemaVersion: 1, RunID: runID, Sequence: 1}, Events: []event.Event{{SchemaVersion: 1, Input: event.Input{ID: id.New(event.PrefixEvent), Type: event.ExecutionFinished}, RunID: runID, Sequence: 1, RunRevision: 2, ObservedAt: time.Now().UTC()}}}
	return cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "replay"}}}), req, page
}

func TestPublicReplayRejectsMalformedStoragePages(t *testing.T) {
	for name, mutate := range map[string]func(*event.Page){
		"other run":       func(p *event.Page) { p.Events[0].RunID = id.NewAgentRunID() },
		"other schema":    func(p *event.Page) { p.Watermark.SchemaVersion = 2 },
		"missing event":   func(p *event.Page) { p.Events = nil },
		"bad cursor":      func(p *event.Page) { p.Next.Sequence++ },
		"private type":    func(p *event.Page) { p.Events[0].Type = "raw candidate" },
		"future revision": func(p *event.Page) { p.Events[0].RunRevision++ },
		"bad retention":   func(p *event.Page) { p.Watermark.FirstSequence = 0 },
		"oversized page":  func(p *event.Page) { p.Events = append(p.Events, p.Events[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			ctx, req, page := replayFixture()
			mutate(&page)
			reader, err := event.NewReader(replayStorage(func(context.Context, event.ReadRequest) (event.Page, error) { return page, nil }), replayAuthorizer(func(context.Context, id.AgentRunID) error { return nil }))
			require.NoError(t, err)
			got, err := reader.Read(ctx, req)
			require.ErrorIs(t, err, event.ErrInvalid)
			require.Zero(t, got)
		})
	}
}

func TestPublicReplayDetachesBeforeReauthorization(t *testing.T) {
	ctx, req, page := replayFixture()
	calls := 0
	reader, err := event.NewReader(replayStorage(func(context.Context, event.ReadRequest) (event.Page, error) { return page, nil }), replayAuthorizer(func(context.Context, id.AgentRunID) error {
		calls++
		if calls == 2 {
			page.Events[0].Type = "raw candidate"
		}
		return nil
	}))
	require.NoError(t, err)
	got, err := reader.Read(ctx, req)
	require.NoError(t, err)
	require.Equal(t, event.ExecutionFinished, got.Events[0].Type)
}

func TestPublicReplayGapStillRequiresAuthority(t *testing.T) {
	ctx, req, page := replayFixture()
	page.Events = nil
	page.Watermark.FirstSequence = 2
	calls := 0
	denied := errors.New("subscriber revoked")
	reader, err := event.NewReader(replayStorage(func(context.Context, event.ReadRequest) (event.Page, error) { return page, event.ErrCursorGap }), replayAuthorizer(func(context.Context, id.AgentRunID) error {
		calls++
		if calls == 2 {
			return denied
		}
		return nil
	}))
	require.NoError(t, err)
	got, err := reader.Read(ctx, req)
	require.ErrorIs(t, err, denied)
	require.Zero(t, got)
}

func TestPublicReplayCancellationDiscardsPage(t *testing.T) {
	base, req, page := replayFixture()
	ctx, cancel := context.WithCancel(base)
	defer cancel()
	reader, err := event.NewReader(replayStorage(func(context.Context, event.ReadRequest) (event.Page, error) { cancel(); return page, nil }), replayAuthorizer(func(context.Context, id.AgentRunID) error { return nil }))
	require.NoError(t, err)
	got, err := reader.Read(ctx, req)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, got)
}
