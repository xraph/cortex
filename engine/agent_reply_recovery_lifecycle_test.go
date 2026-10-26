package engine

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/suspension"
)

type replyRecoverySource func(context.Context, coordination.AgentReplyListRequest) (coordination.AgentReplyPage, error)

type backgroundReplyClient struct {
	*scriptedLLM
	calls atomic.Int32
}

func (c *backgroundReplyClient) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	// Starting the bus also delivers the original queued question to its worker.
	// Count only the interrupted planner whose resume is under test.
	if strings.Contains(req.System, "you are planner") {
		c.calls.Add(1)
	}
	return c.scriptedLLM.Complete(ctx, req)
}

func (f replyRecoverySource) ListAgentReplyRuns(ctx context.Context, req coordination.AgentReplyListRequest) (coordination.AgentReplyPage, error) {
	return f(ctx, req)
}

func TestAgentReplyBackgroundRecovery(t *testing.T) {
	for _, boundary := range []string{"seal_lost", "admit_lost", "before_start", "start_lost", "running_without_pause"} {
		t.Run(boundary, func(t *testing.T) {
			fixtureBoundary := boundary
			if boundary == "running_without_pause" {
				fixtureBoundary = "start_lost"
			}
			st, ctx, current, pauseID := admittedReplyFixture(t, fixtureBoundary)
			if boundary == "running_without_pause" {
				require.NoError(t, st.DeleteSuspension(ctx, current.ID))
			}
			client := &backgroundReplyClient{scriptedLLM: &scriptedLLM{answered: true}}
			var checks, failures atomic.Int32
			discovery, available := st.(suspension.ReplyDiscoveryStore)
			require.True(t, available)
			source, sourceErr := coordination.NewScopedAgentReplySource(discovery, []cortex.Scope{cortex.ScopeFromContext(ctx)}, func(context.Context, cortex.Scope) error { return nil })
			require.NoError(t, sourceErr)
			type hostIdentity struct{}
			config := AgentReplyRecoveryConfig{
				Service: coordination.AgentReplyRecoveryConfig{
					Discovery: source, Interval: 5 * time.Millisecond,
					ReportError: func(err error) {
						if errors.Is(err, suspension.ErrResumeAlreadyStarted) {
							failures.Add(1)
						}
					},
				},
				Authorize: func(tc context.Context, key id.AgentRunID) error {
					require.Equal(t, "host", tc.Value(hostIdentity{}))
					require.Equal(t, current.ID, key)
					require.Equal(t, cortex.ScopeFromContext(ctx), cortex.ScopeFromContext(tc))
					checks.Add(1)
					return nil
				},
			}
			fresh, err := New(WithAgentReplyRecovery(config), WithStore(st), WithLLM(client), WithA2A(a2a.Options{Workers: 1}))
			require.NoError(t, err)
			startup, cancel := context.WithCancel(context.WithValue(ctx, hostIdentity{}, "host"))
			require.NoError(t, fresh.Start(startup))
			cancel()
			t.Cleanup(func() { require.NoError(t, fresh.Stop(context.Background())) })
			require.Eventually(t, func() bool {
				if boundary == "start_lost" || boundary == "running_without_pause" {
					return failures.Load() > 1
				}
				stored, readErr := st.GetRun(ctx, current.ID)
				return readErr == nil && stored.State == run.StateCompleted
			}, 10*time.Second, 5*time.Millisecond)
			require.NoError(t, fresh.Stop(context.Background()))
			want := int32(1)
			if boundary == "start_lost" || boundary == "running_without_pause" {
				want = 0
			}
			require.Equal(t, want, client.calls.Load())
			status, configured := fresh.AgentReplyRecoveryStatus()
			require.True(t, configured)
			require.False(t, status.Running)
			require.Zero(t, status.Active)
			durable := st.(suspension.ResumeAdmissionStore)
			_, err = durable.GetResumeStart(ctx, current.ID, pauseID)
			require.NoError(t, err)
		})
	}
}

func TestAgentReplyBackgroundRecoveryDenied(t *testing.T) {
	st, ctx, current, pauseID := admittedReplyFixture(t, "before_start")
	client := &countedResumeClient{scriptedLLM: &scriptedLLM{answered: true}}
	var failures atomic.Int32
	denied := errors.New("host permission revoked")
	config := AgentReplyRecoveryConfig{Service: coordination.AgentReplyRecoveryConfig{
		Discovery: replyRecoverySource(func(context.Context, coordination.AgentReplyListRequest) (coordination.AgentReplyPage, error) {
			return coordination.AgentReplyPage{Runs: []coordination.ScheduledAgentReply{{Scope: cortex.ScopeFromContext(ctx), ID: current.ID}}}, nil
		}), Interval: time.Millisecond, ReportError: func(err error) {
			if errors.Is(err, denied) {
				failures.Add(1)
			}
		},
	}, Authorize: func(context.Context, id.AgentRunID) error { return denied }}
	fresh, err := New(WithStore(st), WithLLM(client), WithAgentReplyRecovery(config))
	require.NoError(t, err)
	require.NoError(t, fresh.Start(ctx))
	t.Cleanup(func() { require.NoError(t, fresh.Stop(context.Background())) })
	require.Eventually(t, func() bool { return failures.Load() > 1 }, 5*time.Second, time.Millisecond)
	require.NoError(t, fresh.Stop(context.Background()))
	require.Zero(t, client.calls.Load())
	durable := st.(suspension.ResumeAdmissionStore)
	_, err = durable.GetResumeStart(ctx, current.ID, pauseID)
	require.ErrorIs(t, err, suspension.ErrResumeAdmissionNotFound)
}

func TestAgentReplyBackgroundRecoveryCompetingEngines(t *testing.T) {
	st, ctx, current, _ := admittedReplyFixture(t, "before_start")
	discovery := st.(suspension.ReplyDiscoveryStore)
	source, err := coordination.NewScopedAgentReplySource(discovery, []cortex.Scope{cortex.ScopeFromContext(ctx)}, func(context.Context, cortex.Scope) error { return nil })
	require.NoError(t, err)
	client := &backgroundReplyClient{scriptedLLM: &scriptedLLM{answered: true}}
	config := AgentReplyRecoveryConfig{Service: coordination.AgentReplyRecoveryConfig{Discovery: source, Interval: time.Millisecond}, Authorize: func(context.Context, id.AgentRunID) error { return nil }}
	for range 2 {
		fresh, createErr := New(WithStore(st), WithLLM(client), WithA2A(a2a.Options{Workers: 1}), WithAgentReplyRecovery(config))
		require.NoError(t, createErr)
		require.NoError(t, fresh.Start(ctx))
		t.Cleanup(func() { require.NoError(t, fresh.Stop(context.Background())) })
	}
	require.Eventually(t, func() bool {
		stored, readErr := st.GetRun(ctx, current.ID)
		return readErr == nil && stored.State == run.StateCompleted
	}, 10*time.Second, 5*time.Millisecond)
	require.EqualValues(t, 1, client.calls.Load())
}

func TestAgentReplyBackgroundRecoveryRequiresDependencies(t *testing.T) {
	st, _, _, _ := admittedReplyFixture(t, "before_start")
	source := replyRecoverySource(func(context.Context, coordination.AgentReplyListRequest) (coordination.AgentReplyPage, error) {
		return coordination.AgentReplyPage{}, nil
	})
	config := AgentReplyRecoveryConfig{Service: coordination.AgentReplyRecoveryConfig{Discovery: source}}
	_, err := New(WithStore(st), WithLLM(&scriptedLLM{}), WithAgentReplyRecovery(config))
	require.ErrorIs(t, err, coordination.ErrInvalidScheduler)
	config.Authorize = func(context.Context, id.AgentRunID) error { return nil }
	_, err = New(WithAgentReplyRecovery(config), WithLLM(&scriptedLLM{}))
	require.ErrorIs(t, err, cortex.ErrNoStore)
	_, err = New(WithAgentReplyRecovery(config), WithStore(st))
	require.ErrorIs(t, err, cortex.ErrNoLLM)
	_, err = New(WithAgentReplyRecovery(config), WithStore(struct{ store.Store }{st}), WithLLM(&scriptedLLM{}))
	require.ErrorIs(t, err, suspension.ErrResumeAdmissionUnavailable)
	_, err = New(WithStore(st), WithLLM(&scriptedLLM{}), WithAgentReplyRecovery(config), WithAgentReplyRecovery(config))
	require.ErrorIs(t, err, coordination.ErrInvalidScheduler)
}
