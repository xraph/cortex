package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/suspension"
)

type resumeAdmissionFaultStore struct {
	store.Store
	a2a.AskResolutionStore
	suspension.VersionedStore
	suspension.ResumeAdmissionStore
	boundary string
	fault    error
}

func TestAgentReplyRecoveryRequiresLiveAuthority(t *testing.T) {
	for _, boundary := range []string{"before_start", "seal_lost"} {
		for _, revoke := range []string{"no_authority", "before_read", "before_start", "after_start"} {
			t.Run(boundary+"/"+revoke, func(t *testing.T) {
				st, ctx, current, pauseID := admittedReplyFixture(t, boundary)
				client := &countedResumeClient{scriptedLLM: &scriptedLLM{answered: true}}
				fresh, err := New(WithStore(st), WithLLM(client), WithA2A(a2a.Options{HopCeiling: 4, Workers: 1}))
				require.NoError(t, err)
				durable, ok := st.(suspension.ResumeAdmissionStore)
				require.True(t, ok)
				denied := errors.New("recovery grant withdrawn")
				checked := false
				var authorize func(context.Context, id.AgentRunID) error
				if revoke != "no_authority" {
					authorize = func(_ context.Context, key id.AgentRunID) error {
						require.Equal(t, current.ID, key)
						_, admissionErr := durable.GetResumeAdmission(ctx, key, pauseID)
						require.True(t, admissionErr == nil || errors.Is(admissionErr, suspension.ErrResumeAdmissionNotFound))
						_, startErr := durable.GetResumeStart(ctx, key, pauseID)
						require.True(t, startErr == nil || errors.Is(startErr, suspension.ErrResumeAdmissionNotFound))
						withdrawn := revoke == "before_read" ||
							(revoke == "before_start" && checked && admissionErr == nil) ||
							(revoke == "after_start" && startErr == nil)
						checked = true
						if withdrawn {
							return denied
						}
						return nil
					}
				}
				_, err = fresh.RecoverAgentReply(ctx, current.ID, pauseID, authorize)
				if revoke == "no_authority" {
					require.ErrorIs(t, err, suspension.ErrResumeAdmissionUnavailable)
				} else {
					require.ErrorIs(t, err, denied)
				}
				require.Zero(t, client.calls.Load())
				_, startErr := durable.GetResumeStart(ctx, current.ID, pauseID)
				if revoke == "after_start" {
					require.NoError(t, startErr)
				} else {
					require.ErrorIs(t, startErr, suspension.ErrResumeAdmissionNotFound)
				}
			})
		}
	}
}

func TestAgentReplyRecoveryBindsScopeAndRun(t *testing.T) {
	st, ctx, current, pauseID := admittedReplyFixture(t, "before_start")
	client := &countedResumeClient{scriptedLLM: &scriptedLLM{answered: true}}
	fresh, err := New(WithStore(st), WithLLM(client), WithA2A(a2a.Options{}))
	require.NoError(t, err)
	authorize := func(context.Context, id.AgentRunID) error { return nil }
	foreign := cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "other"}}})
	for _, attempt := range []context.Context{context.Background(), foreign} {
		_, err = fresh.RecoverAgentReply(attempt, current.ID, pauseID, authorize)
		require.Error(t, err)
	}
	_, err = fresh.RecoverAgentReply(ctx, id.NewAgentRunID(), pauseID, authorize)
	require.Error(t, err)
	_, err = fresh.RecoverAgentReply(ctx, current.ID, id.NewSuspensionID(), authorize)
	require.ErrorIs(t, err, suspension.ErrResumeAdmissionConflict)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = fresh.RecoverAgentReply(cancelled, current.ID, pauseID, authorize)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, client.calls.Load())
	durable, ok := st.(suspension.ResumeAdmissionStore)
	require.True(t, ok)
	_, err = durable.GetResumeStart(ctx, current.ID, pauseID)
	require.ErrorIs(t, err, suspension.ErrResumeAdmissionNotFound)
}

func TestAgentReplyRecoveryRevocationStopsToolDispatch(t *testing.T) {
	var permitted atomic.Bool
	permitted.Store(true)
	var effects atomic.Int32
	tool := WithTool(llm.Tool{Name: "effect", Description: "record an effect", Parameters: map[string]any{"type": "object"}}, func(context.Context, cortex.Invocation) (string, error) { effects.Add(1); return "done", nil })
	st, ctx, current, pauseID := admittedReplyFixture(t, "before_start", tool)
	client := taskCallClient{complete: func(context.Context, *llm.Request) (*llm.Response, error) {
		permitted.Store(false)
		return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "effect-1", Name: "effect", Arguments: `{}`}}}, nil
	}}
	fresh, err := New(WithStore(st), WithLLM(client), WithA2A(a2a.Options{HopCeiling: 4, Workers: 1}), tool)
	require.NoError(t, err)
	denied := errors.New("recovery permission revoked")
	_, err = fresh.RecoverAgentReply(ctx, current.ID, pauseID, func(context.Context, id.AgentRunID) error {
		if !permitted.Load() {
			return denied
		}
		return nil
	})
	require.ErrorIs(t, err, denied)
	require.Zero(t, effects.Load())
}

func (s *resumeAdmissionFaultStore) AdmitResume(ctx context.Context, req suspension.ResumeAdmissionRequest) (suspension.ResumeAdmission, error) {
	result, err := s.ResumeAdmissionStore.AdmitResume(ctx, req)
	if err == nil && s.boundary == "admit_lost" {
		return suspension.ResumeAdmission{}, s.fault
	}
	return result, err
}

func (s *resumeAdmissionFaultStore) StartResume(ctx context.Context, runID id.AgentRunID, key id.SuspensionID, digest string) (suspension.ResumeStart, error) {
	if s.boundary == "before_start" {
		return suspension.ResumeStart{}, s.fault
	}
	result, err := s.ResumeAdmissionStore.StartResume(ctx, runID, key, digest)
	if err == nil && s.boundary == "start_lost" {
		return suspension.ResumeStart{}, s.fault
	}
	return result, err
}

type countedResumeClient struct {
	*scriptedLLM
	calls atomic.Int32
}

func (s *resumeAdmissionFaultStore) SealAskResolution(ctx context.Context, ask *a2a.PendingAsk, reply a2a.AskReply) (a2a.AskResolution, error) {
	result, err := s.AskResolutionStore.SealAskResolution(ctx, ask, reply)
	if err == nil && s.boundary == "seal_lost" {
		return a2a.AskResolution{}, s.fault
	}
	return result, err
}

func (c *countedResumeClient) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	c.calls.Add(1)
	return c.scriptedLLM.Complete(ctx, req)
}

func TestAgentReplyResumeAdmissionRecovery(t *testing.T) {
	for _, boundary := range []string{"seal_lost", "admit_lost", "before_start", "start_lost"} {
		t.Run(boundary, func(t *testing.T) {
			st, ctx, current, pauseID := admittedReplyFixture(t, boundary)
			client := &countedResumeClient{scriptedLLM: &scriptedLLM{answered: true}}
			fresh, err := New(WithStore(st), WithLLM(client), WithA2A(a2a.Options{HopCeiling: 4, Workers: 1}))
			require.NoError(t, err)
			authorize := func(context.Context, id.AgentRunID) error { return nil }
			recovered, err := fresh.RecoverAgentReply(ctx, current.ID, pauseID, authorize)
			if boundary == "start_lost" {
				require.ErrorIs(t, err, suspension.ErrResumeAlreadyStarted)
				require.Nil(t, recovered)
				require.Equal(t, int32(0), client.calls.Load())
			} else {
				require.NoError(t, err)
				require.Equal(t, run.StateCompleted, recovered.State)
				require.Equal(t, int32(1), client.calls.Load())
			}
			_, err = fresh.RecoverAgentReply(ctx, current.ID, pauseID, authorize)
			require.ErrorIs(t, err, suspension.ErrResumeAlreadyStarted)
			wantCalls := int32(1)
			if boundary == "start_lost" {
				wantCalls = 0
			}
			require.Equal(t, wantCalls, client.calls.Load())
		})
	}
}

func admittedReplyFixture(t *testing.T, boundary string, options ...Option) (store.Store, context.Context, *run.Run, id.SuspensionID) {
	t.Helper()
	return admittedReplyFixtureAs(t, boundary, nil, options...)
}

// admittedReplyFixtureAs is admittedReplyFixture with the asking run
// started under principal.
func admittedReplyFixtureAs(t *testing.T, boundary string, principal any, options ...Option) (store.Store, context.Context, *run.Run, id.SuspensionID) {
	t.Helper()
	e, st, ctx := a2aEngine(t, []llm.ToolCall{{ID: "call-1", Name: toolAgentAsk, Arguments: `{"to":"worker","content":"status?"}`}})
	for _, option := range options {
		require.NoError(t, option(e))
	}
	current, err := e.RunAgent(cortex.WithPrincipal(ctx, principal), "planner", "ask", nil)
	require.NoError(t, err)
	paused, err := st.GetSuspension(ctx, current.ID)
	require.NoError(t, err)
	messages, err := st.ListMessages(ctx, &a2a.MessageListFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, messages, 1)
	durable, ok := st.(suspension.ResumeAdmissionStore)
	require.True(t, ok)
	resolutions, ok := st.(a2a.AskResolutionStore)
	require.True(t, ok)
	versioned, ok := st.(suspension.VersionedStore)
	require.True(t, ok)
	fault := errors.New("injected resume acknowledgment failure")
	e.store = &resumeAdmissionFaultStore{Store: st, AskResolutionStore: resolutions, VersionedStore: versioned, ResumeAdmissionStore: durable, boundary: boundary, fault: fault}
	if boundary == "seal_lost" {
		e.a2a, err = a2a.NewBus(a2a.BusConfig{Store: e.store, Runner: agentRunnerAdapter{eng: e}, Resumer: a2aResumer{eng: e}, Options: a2a.Options{HopCeiling: 4, Workers: 1}})
		require.NoError(t, err)
	}
	question := messages[0]
	_, err = e.A2A().Send(ctx, a2a.SendParams{Sender: a2a.Address{Agent: "worker"}, Receivers: []a2a.Address{{Agent: "planner"}}, Performative: a2a.Inform, Content: "answer", ConversationID: question.ConversationID, InReplyTo: question.ReplyWith})
	require.ErrorIs(t, err, fault)
	stored, err := st.GetRun(ctx, current.ID)
	require.NoError(t, err)
	admitted, err := durable.GetResumeAdmission(ctx, current.ID, paused.ID)
	if boundary == "seal_lost" {
		require.Equal(t, run.StatePaused, stored.State)
		require.ErrorIs(t, err, suspension.ErrResumeAdmissionNotFound)
	} else {
		require.Equal(t, run.StateRunning, stored.State)
		require.NoError(t, err)
		require.Contains(t, string(admitted.Request.Input), "answer")
	}
	return st, ctx, current, paused.ID
}
