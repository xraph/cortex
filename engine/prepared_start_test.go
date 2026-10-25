package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/session"
	"github.com/xraph/cortex/store/sqlite"
	"github.com/xraph/cortex/verification"
)

var errPreparedStartStorage = errors.New("private startup storage details")

type preparedStartFaultStore struct {
	*sqlite.Store
	failCreate    bool
	lostCreateAck bool
	failTerminal  bool
	created       id.AgentRunID
}

func (s *preparedStartFaultStore) CreateRun(ctx context.Context, r *run.Run) error {
	s.created = r.ID
	if s.failCreate {
		return errPreparedStartStorage
	}
	if err := s.Store.CreateRun(ctx, r); err != nil {
		return err
	}
	if s.lostCreateAck {
		return errPreparedStartStorage
	}
	return nil
}

func (*preparedStartFaultStore) CreateCaptureIntent(context.Context, verification.CaptureIntent) error {
	return errPreparedStartStorage
}

func (s *preparedStartFaultStore) UpdateRun(ctx context.Context, r *run.Run) error {
	if s.failTerminal {
		return errPreparedStartStorage
	}
	return s.Store.UpdateRun(ctx, r)
}

func TestPreparedStartFailureKeepsRunIdentity(t *testing.T) {
	for _, mode := range []string{"registration", "terminal", "create", "lost_create_ack"} {
		for _, stream := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "/sync", true: "/stream"}[stream], func(t *testing.T) {
				f := newWorkerFixture(t)
				storage := &preparedStartFaultStore{Store: f.storage, failCreate: mode == "create", lostCreateAck: mode == "lost_create_ack", failTerminal: mode == "terminal"}
				client := &requiredClient{}
				e, err := engine.New(engine.WithStore(storage), engine.WithLLM(client), engine.WithModelRevision("smart", "model:1"), engine.WithVerificationWorker(f.config), engine.WithRequiredVerification(requiredConfiguration(t, f)))
				require.NoError(t, err)
				ag := &agent.Config{ID: id.NewAgentID(), Name: "startup"}
				require.NoError(t, f.storage.Create(f.ctx, ag))
				p, err := e.PrepareAgent(f.ctx, ag.ID, &engine.RunOverrides{ToolsRestricted: true, Budget: &execution.Budget{MaxSteps: 3, MaxTokens: 1000, Deadline: time.Now().Add(2 * time.Minute)}})
				require.NoError(t, err)
				var returned *run.Run
				if stream {
					events := make(chan engine.StreamEvent, 4)
					err = e.StreamPrepared(f.ctx, p, "work", engine.PreparedRunOptions{}, events)
					_, open := <-events
					require.False(t, open, "startup error must close stream without publication")
				} else {
					returned, err = e.RunPrepared(f.ctx, p, "work", engine.PreparedRunOptions{})
				}
				require.Error(t, err)
				require.Zero(t, client.calls)
				require.NotContains(t, err.Error(), "private startup")
				var startup interface{ RunIdentity() id.AgentRunID }
				require.ErrorAs(t, err, &startup, "startup error must retain identity for reconciliation")
				require.Equal(t, storage.created, startup.RunIdentity())
				stored, readErr := f.storage.GetRun(f.ctx, storage.created)
				if mode == "create" {
					require.ErrorIs(t, readErr, cortex.ErrRunNotFound)
					require.Nil(t, returned)
					return
				}
				require.NoError(t, readErr)
				if mode == "lost_create_ack" {
					require.Equal(t, run.StateRunning, stored.State)
					require.Nil(t, returned, "ambiguous creation must not imply acknowledged start")
					return
				}
				require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
				if !stream {
					require.NotNil(t, returned, "created run lost after startup failure")
					require.Equal(t, stored.ID, returned.ID)
				}
				if mode == "terminal" {
					require.ErrorIs(t, err, errPreparedStartStorage, "terminal persistence failure was hidden")
					require.Equal(t, run.StateRunning, stored.State)
				} else {
					require.Equal(t, run.StateFailed, stored.State)
				}
			})
		}
	}
}

type preparedHistoryBlocker struct{}

func (preparedHistoryBlocker) ScanInput(_ context.Context, req *safety.ScanRequest) (*safety.ScanResult, error) {
	if req.Content == "blocked history" {
		return &safety.ScanResult{Decision: safety.DecisionBlock, Blocked: true}, nil
	}
	return &safety.ScanResult{Decision: safety.DecisionAllow}, nil
}

func (preparedHistoryBlocker) ScanOutput(context.Context, *safety.ScanRequest) (*safety.ScanResult, error) {
	return &safety.ScanResult{Decision: safety.DecisionAllow}, nil
}

func TestPreparedHistoryFailureReturnsCreatedRun(t *testing.T) {
	ctx := overlayCtx("prepared-history-start")
	s := newSQLiteStoreForTest(t)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "history"}
	require.NoError(t, s.Create(ctx, ag))
	sid := id.NewSessionID()
	require.NoError(t, s.CreateSession(ctx, &session.Session{ID: sid, AgentID: ag.ID}))
	require.NoError(t, s.SaveConversation(ctx, ag.ID, sid, []memory.Message{{Role: "user", Content: "blocked history"}}))
	client := &requiredClient{}
	e, err := engine.New(engine.WithStore(s), engine.WithLLM(client), engine.WithSafety(preparedHistoryBlocker{}))
	require.NoError(t, err)
	p, err := e.PrepareAgent(ctx, ag.ID, nil)
	require.NoError(t, err)
	r, err := e.RunPrepared(ctx, p, "work", engine.PreparedRunOptions{SessionID: sid})
	require.ErrorIs(t, err, safety.ErrBlocked)
	require.NotNil(t, r, "created run lost after history validation")
	require.Zero(t, client.calls)
	stored, err := s.GetRun(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateIncomplete, stored.State)
	require.Equal(t, run.StopSafetyBlock, stored.StopReason)
}
