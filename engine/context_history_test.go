package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/sqlite"
	"github.com/xraph/cortex/verification"
)

type unavailableHistoryStore struct {
	*sqlite.Store
	created      id.AgentRunID
	failTerminal bool
}

func (*unavailableHistoryStore) LoadConversation(context.Context, id.AgentID, id.SessionID, int) ([]memory.Message, error) {
	// A partial read must not become usable context when its read failed.
	return []memory.Message{{Role: "user", Content: "partial private history"}}, errors.New("private database connection details")
}

func (s *unavailableHistoryStore) CreateRun(ctx context.Context, r *run.Run) error {
	s.created = r.ID
	return s.Store.CreateRun(ctx, r)
}

func (s *unavailableHistoryStore) UpdateRun(ctx context.Context, r *run.Run) error {
	if s.failTerminal {
		return errPreparedStartStorage
	}
	return s.Store.UpdateRun(ctx, r)
}

func TestUnavailableConversationStopsBeforeProvider(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			name := map[bool]string{false: "ordinary", true: "prepared"}[prepared] + map[bool]string{false: "/sync", true: "/stream"}[stream]
			t.Run(name, func(t *testing.T) {
				ctx := overlayCtx("unavailable-history")
				s := &unavailableHistoryStore{Store: newSQLiteStoreForTest(t)}
				client := &requiredClient{}
				e, err := engine.New(engine.WithStore(s), engine.WithLLM(client))
				require.NoError(t, err)
				ag := &agent.Config{ID: id.NewAgentID(), Name: "history"}
				require.NoError(t, s.Create(ctx, ag))
				var p *engine.PreparedAgent
				if prepared {
					p, err = e.PrepareAgent(ctx, ag.ID, nil)
					require.NoError(t, err)
				}
				var returned *run.Run
				if stream {
					events := make(chan engine.StreamEvent, 32)
					if prepared {
						err = e.StreamPrepared(ctx, p, "work", engine.PreparedRunOptions{}, events)
					} else {
						err = e.StreamAgent(ctx, ag.Name, "work", nil, events)
					}
					// Join a stream even on the broken implementation before inspecting the client.
					var published []engine.StreamEvent
					for event := range events {
						published = append(published, event)
					}
					require.Error(t, err)
					require.Empty(t, published)
				} else if prepared {
					returned, err = e.RunPrepared(ctx, p, "work", engine.PreparedRunOptions{})
				} else {
					returned, err = e.RunAgent(ctx, ag.Name, "work", nil)
				}
				require.ErrorIs(t, err, cortex.ErrConversationUnavailable)
				require.NotContains(t, err.Error(), "private")
				require.Zero(t, client.calls)
				var startup interface{ RunIdentity() id.AgentRunID }
				require.ErrorAs(t, err, &startup)
				require.Equal(t, s.created, startup.RunIdentity())
				stored, readErr := s.GetRun(ctx, s.created)
				require.NoError(t, readErr)
				require.Equal(t, run.StateIncomplete, stored.State)
				require.Equal(t, run.StopExecutionError, stored.StopReason)
				require.Equal(t, verification.StatusUnchecked, stored.VerificationStatus)
				require.Empty(t, stored.Output)
				require.Empty(t, stored.ResultRef)
				require.Equal(t, "cortex: conversation history unavailable", stored.Error)
				if !stream {
					require.NotNil(t, returned)
					require.Equal(t, stored.ID, returned.ID)
				}
				history, readErr := s.Store.LoadConversation(ctx, ag.ID, stored.SessionID, 100)
				require.NoError(t, readErr)
				require.Empty(t, history)
			})
		}
	}
}

func TestUnavailableConversationRetainsTerminalFailure(t *testing.T) {
	ctx := overlayCtx("history-terminal-failure")
	s := &unavailableHistoryStore{Store: newSQLiteStoreForTest(t), failTerminal: true}
	client := &requiredClient{}
	e, err := engine.New(engine.WithStore(s), engine.WithLLM(client))
	require.NoError(t, err)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "history"}
	require.NoError(t, s.Create(ctx, ag))
	r, err := e.RunAgent(ctx, ag.Name, "work", nil)
	require.ErrorIs(t, err, cortex.ErrConversationUnavailable)
	require.ErrorIs(t, err, errPreparedStartStorage)
	require.NotContains(t, err.Error(), "private")
	require.NotNil(t, r)
	require.Zero(t, client.calls)
	stored, readErr := s.GetRun(ctx, r.ID)
	require.NoError(t, readErr)
	require.Equal(t, run.StateRunning, stored.State, "failed terminal write cannot imply durable termination")
	require.Empty(t, stored.Output)
}

func TestEmptyConversationStillExecutes(t *testing.T) {
	ctx := overlayCtx("empty-history")
	s := newSQLiteStoreForTest(t)
	client := &requiredClient{}
	e, err := engine.New(engine.WithStore(s), engine.WithLLM(client))
	require.NoError(t, err)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "history"}
	require.NoError(t, s.Create(ctx, ag))
	r, err := e.RunAgent(ctx, ag.Name, "work", nil)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, r.State)
	require.Equal(t, 1, client.calls)
}

func TestUnavailableConversationPreservesRequiredPreparationFailure(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "stream"}[stream], func(t *testing.T) {
			f := newWorkerFixture(t)
			s := &unavailableHistoryStore{Store: f.storage}
			client := &requiredClient{}
			_, ag, overrides := requiredSetup(t, f, client)
			e, err := engine.New(engine.WithStore(s), engine.WithLLM(client), engine.WithModelRevision("smart", "model:1"), engine.WithVerificationWorker(f.config), engine.WithRequiredVerification(requiredConfiguration(t, f)))
			require.NoError(t, err)
			if stream {
				events := make(chan engine.StreamEvent, 32)
				err = e.StreamAgent(f.ctx, ag.Name, "work", overrides, events)
				for event := range events {
					t.Errorf("unexpected event: %s", event.Type)
				}
			} else {
				var r *run.Run
				r, err = e.RunAgent(f.ctx, ag.Name, "work", overrides)
				require.Nil(t, r)
			}
			require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
			require.NotContains(t, err.Error(), "private")
			require.True(t, s.created.IsNil(), "required history fails before run creation")
			require.Zero(t, client.calls)
			require.Zero(t, f.adapter.evaluations)
		})
	}
}
