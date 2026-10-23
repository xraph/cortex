package engine_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/scopespy"
)

type rejectedTerminalStore struct{ store.Store }

func (rejectedTerminalStore) CommitTerminalRun(context.Context, event.TerminalUpdate) (event.TerminalReceipt, error) {
	return event.TerminalReceipt{}, errors.New("terminal transaction unavailable")
}

func TestOrdinaryRunRecordsTerminalEvent(t *testing.T) {
	for _, mode := range []string{"sync", "stream", "failure", "cancel", "external", "mock", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			ctx := overlayCtx("terminal")
			storage := openSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "terminal.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
			client := &requiredClient{response: &llm.Response{Content: "answer", FinishReason: "stop"}, chunks: []llm.Chunk{{Content: "answer", FinishReason: "stop"}}}
			if mode == "failure" {
				client.failure = errors.New("provider unavailable")
			}
			opts := []engine.Option{engine.WithStore(storage)}
			if mode != "disabled" {
				opts = append(opts, engine.WithPublicEventRecording())
			}
			if mode != "mock" {
				opts = append(opts, engine.WithLLM(client))
			}
			e, err := engine.New(opts...)
			require.NoError(t, err)
			ag := &agent.Config{ID: id.NewAgentID(), Name: "worker"}
			require.NoError(t, e.CreateAgent(ctx, ag))
			var r *run.Run
			switch mode {
			case "stream":
				events := make(chan engine.StreamEvent, 64)
				require.NoError(t, e.StreamAgent(ctx, "worker", "work", nil, events))
				for streamed := range events {
					require.NotEqual(t, engine.EventError, streamed.Type)
				}
				runs, listErr := storage.ListRuns(ctx, nil)
				require.NoError(t, listErr)
				require.Len(t, runs, 1)
				r = runs[0]
			case "cancel", "external":
				r = &run.Run{ID: id.NewAgentRunID(), AgentID: ag.ID, State: run.StateRunning}
				require.NoError(t, storage.CreateRun(ctx, r))
				if mode == "cancel" {
					require.NoError(t, e.CancelRun(ctx, r.ID))
				} else {
					r.State = run.StateIncomplete
					require.NoError(t, e.UpdateRun(ctx, r))
				}
				r, err = storage.GetRun(ctx, r.ID)
				require.NoError(t, err)
			default:
				r, err = e.RunAgent(ctx, "worker", "work", nil)
				if mode == "failure" {
					require.Error(t, err)
					runs, listErr := storage.ListRuns(ctx, nil)
					require.NoError(t, listErr)
					require.Len(t, runs, 1)
					r = runs[0]
				} else {
					require.NoError(t, err)
				}
			}
			page, err := storage.ReadPublicEvents(ctx, event.ReadRequest{After: event.Cursor{SchemaVersion: 1, RunID: r.ID}, Limit: 10})
			require.NoError(t, err)
			if mode == "disabled" {
				require.Empty(t, page.Events)
				return
			}
			require.Len(t, page.Events, 1)
			require.Equal(t, event.ExecutionFinished, page.Events[0].Type)
			require.Equal(t, r.Revision, page.Events[0].RunRevision)
		})
	}
}

func TestOrdinaryEventRecordingRequiresAtomicStore(t *testing.T) {
	_, err := engine.New(engine.WithStore(scopespy.New()), engine.WithPublicEventRecording())
	require.ErrorIs(t, err, event.ErrUnavailable)
}

func TestOrdinaryEventFailureDoesNotFallBackToRunUpdate(t *testing.T) {
	ctx := overlayCtx("terminal")
	storage := openSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "terminal.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	e, err := engine.New(engine.WithStore(rejectedTerminalStore{Store: storage}), engine.WithPublicEventRecording())
	require.NoError(t, err)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker"}
	require.NoError(t, e.CreateAgent(ctx, ag))
	r := &run.Run{ID: id.NewAgentRunID(), AgentID: ag.ID, State: run.StateRunning}
	require.NoError(t, storage.CreateRun(ctx, r))
	original := r.Revision
	r.State = run.StateCompleted
	require.Error(t, e.UpdateRun(ctx, r))
	require.Equal(t, original, r.Revision)
	saved, err := storage.GetRun(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateRunning, saved.State)
}

func TestOrdinaryRecordingKeepsRequiredVerificationTransaction(t *testing.T) {
	f := newWorkerFixture(t)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "required-recording"}
	require.NoError(t, f.storage.Create(f.ctx, ag))
	e, err := engine.New(engine.WithStore(f.storage), engine.WithLLM(&requiredClient{}), engine.WithModelRevision("smart", "model:1"), engine.WithVerificationWorker(f.config), engine.WithRequiredVerification(requiredConfiguration(t, f)), engine.WithPublicEventRecording())
	require.NoError(t, err)
	r, err := e.RunAgent(f.ctx, ag.Name, "work", &engine.RunOverrides{ToolsRestricted: true, Budget: &execution.Budget{MaxSteps: 3, MaxTokens: 1000, Deadline: time.Now().Add(2 * time.Minute)}})
	require.NoError(t, err)
	page, err := f.storage.ReadPublicEvents(f.ctx, event.ReadRequest{After: event.Cursor{SchemaVersion: 1, RunID: r.ID}, Limit: 10})
	require.NoError(t, err)
	kinds := make([]event.Type, 0, len(page.Events))
	for _, item := range page.Events {
		kinds = append(kinds, item.Type)
	}
	require.Equal(t, []event.Type{event.ExecutionFinished, event.VerificationPending, event.VerificationPassed, event.ResultCommitted}, kinds)
}

type terminalHooks struct{ completed int }

func (*terminalHooks) Name() string { return "terminal-event-test" }
func (h *terminalHooks) OnRunCompleted(context.Context, id.AgentID, id.AgentRunID, string, time.Duration) error {
	h.completed++
	return nil
}

func TestOrdinaryEventFailureDoesNotPublishCompletion(t *testing.T) {
	ctx := overlayCtx("terminal")
	storage := openSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "terminal.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	hooks := &terminalHooks{}
	e, err := engine.New(engine.WithStore(rejectedTerminalStore{Store: storage}), engine.WithPublicEventRecording(), engine.WithExtension(hooks), engine.WithLLM(&requiredClient{response: &llm.Response{Content: "answer", FinishReason: "stop"}}))
	require.NoError(t, err)
	require.NoError(t, e.CreateAgent(ctx, &agent.Config{ID: id.NewAgentID(), Name: "worker"}))
	_, err = e.RunAgent(ctx, "worker", "work", nil)
	require.Error(t, err)
	require.Zero(t, hooks.completed)
	runs, err := storage.ListRuns(ctx, nil)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, run.StateRunning, runs[0].State)
}
