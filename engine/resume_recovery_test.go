package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/sqlite"
	"github.com/xraph/cortex/suspension"
)

type resumeRecoveryClock struct{ at time.Time }

func (c resumeRecoveryClock) Now() time.Time { return c.at }

type expiredReplyStore struct {
	*sqlite.Store
	admissions suspension.ResumeAdmissionStore
	recovery   suspension.ResumeRecoveryStore
	afterStart func()
}

func (s *expiredReplyStore) StartResume(ctx context.Context, key id.AgentRunID, pause id.SuspensionID, digest string) (suspension.ResumeStart, error) {
	result, err := s.admissions.StartResume(ctx, key, pause, digest)
	if s.afterStart != nil {
		s.afterStart()
	}
	return result, err
}
func (s *expiredReplyStore) ExpireResumeAdmission(ctx context.Context, key id.AgentRunID, pause id.SuspensionID, digest string) (suspension.ResumeExpiry, error) {
	return s.recovery.ExpireResumeAdmission(ctx, key, pause, digest)
}

func TestAgentReplyRecoveryExpiresOnlyUnstartedAdmission(t *testing.T) {
	for _, mode := range []string{"expired", "started", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			boundary := "before_start"
			if mode == "started" {
				boundary = "start_lost"
			}
			st, ctx, current, pauseID := admittedReplyFixture(t, boundary)
			native, ok := st.(*sqlite.Store)
			require.True(t, ok)
			paused, err := st.GetSuspension(ctx, current.ID)
			require.NoError(t, err)
			require.NotNil(t, paused.ExpiresAt)
			expired := native.WithClock(resumeRecoveryClock{at: *paused.ExpiresAt})
			wrapped := &expiredReplyStore{Store: native, admissions: expired, recovery: expired}
			allowed := true
			if mode == "revoked" {
				wrapped.afterStart = func() { allowed = false }
			}
			client := &countedResumeClient{scriptedLLM: &scriptedLLM{answered: true}}
			fresh, err := New(WithStore(wrapped), WithLLM(client))
			require.NoError(t, err)
			denied := errors.New("recovery permission revoked before expiry")
			authorize := func(context.Context, id.AgentRunID) error {
				if !allowed {
					return denied
				}
				return nil
			}
			result, err := fresh.RecoverAgentReply(ctx, current.ID, pauseID, authorize)
			switch mode {
			case "expired":
				require.NoError(t, err)
				require.Equal(t, run.StateIncomplete, result.State)
				require.Equal(t, run.StopDeadline, result.StopReason)
				again, retryErr := fresh.RecoverAgentReply(ctx, current.ID, pauseID, authorize)
				require.NoError(t, retryErr)
				require.Equal(t, result.Revision, again.Revision)
			case "started":
				require.ErrorIs(t, err, suspension.ErrResumeAlreadyStarted)
			default:
				require.ErrorIs(t, err, denied)
			}
			require.Zero(t, client.calls.Load())
			stored, readErr := st.GetRun(ctx, current.ID)
			require.NoError(t, readErr)
			if mode != "expired" {
				require.Equal(t, run.StateRunning, stored.State)
			}
		})
	}
}

type missingReplyPauseStore struct {
	*sqlite.Store
	revoke         func()
	admissionReads int
	present        bool
}

func (s *missingReplyPauseStore) GetSuspension(ctx context.Context, key id.AgentRunID) (*suspension.Suspension, error) {
	s.revoke()
	if s.present {
		return s.Store.GetSuspension(ctx, key)
	}
	return nil, cortex.ErrNotSuspended
}

func (s *missingReplyPauseStore) GetResumeAdmission(ctx context.Context, key id.AgentRunID, pause id.SuspensionID) (suspension.ResumeAdmission, error) {
	s.admissionReads++
	return s.Store.GetResumeAdmission(ctx, key, pause)
}

func (s *missingReplyPauseStore) LatestResumeAdmission(ctx context.Context, key id.AgentRunID) (suspension.ResumeAdmission, error) {
	s.admissionReads++
	return s.Store.LatestResumeAdmission(ctx, key)
}

func TestAgentReplyRecoveryRechecksAuthorityBeforeRetainedAdmissionRead(t *testing.T) {
	for _, present := range []bool{false, true} {
		st, ctx, current, _ := admittedReplyFixture(t, "start_lost")
		native, ok := st.(*sqlite.Store)
		require.True(t, ok)
		allowed := true
		wrapped := &missingReplyPauseStore{Store: native, present: present, revoke: func() { allowed = false }}
		client := &countedResumeClient{scriptedLLM: &scriptedLLM{answered: true}}
		fresh, err := New(WithStore(wrapped), WithLLM(client))
		require.NoError(t, err)
		denied := errors.New("admission read permission withdrawn")
		recovery := agentReplyRecovery{engine: fresh, authorize: func(context.Context, id.AgentRunID) error {
			if !allowed {
				return denied
			}
			return nil
		}}
		require.ErrorIs(t, recovery.Recover(ctx, current.ID), denied)
		require.Zero(t, wrapped.admissionReads)
		require.Zero(t, client.calls.Load())
	}
}

type stagingReplyPauseStore struct {
	*sqlite.Store
	beforePause func(context.Context, *run.Run)
}

func (s *stagingReplyPauseStore) UpdateRun(ctx context.Context, current *run.Run) error {
	if current.State == run.StatePaused {
		s.beforePause(ctx, current)
	}
	return s.Store.UpdateRun(ctx, current)
}

func TestAgentReplyRecoveryWaitsForInitialPauseStateWrite(t *testing.T) {
	e, st, ctx := a2aEngine(t, []llm.ToolCall{{ID: "question", Name: toolAgentAsk, Arguments: `{"to":"worker","content":"check"}`}})
	native, ok := st.(*sqlite.Store)
	require.True(t, ok)
	client := &countedResumeClient{scriptedLLM: &scriptedLLM{answered: true}}
	fresh, err := New(WithStore(native), WithLLM(client), WithA2A(a2a.Options{}))
	require.NoError(t, err)
	recovery := agentReplyRecovery{engine: fresh, authorize: func(context.Context, id.AgentRunID) error { return nil }}
	observed := false
	wrapped := &stagingReplyPauseStore{Store: native, beforePause: func(callCtx context.Context, current *run.Run) {
		observed = true
		stored, readErr := native.GetRun(callCtx, current.ID)
		require.NoError(t, readErr)
		require.Equal(t, run.StateRunning, stored.State)
		paused, readErr := native.GetSuspension(callCtx, current.ID)
		require.NoError(t, readErr, "the suspension is durable before its run becomes paused")
		require.NoError(t, recovery.Recover(callCtx, current.ID), "discovery must leave an initial pause write in progress alone")
		_, readErr = native.GetHeldAskRelease(callCtx, paused.Pending[0].ReplyWith)
		require.ErrorIs(t, readErr, a2a.ErrHeldAskNotFound)
		_, readErr = native.GetResumeAdmission(callCtx, current.ID, paused.ID)
		require.ErrorIs(t, readErr, suspension.ErrResumeAdmissionNotFound)
	}}
	e.store = wrapped
	require.NoError(t, e.buildA2A())
	current, err := e.RunAgent(ctx, "planner", "ask", nil)
	require.NoError(t, err)
	require.True(t, observed)
	require.Equal(t, run.StatePaused, current.State)
	require.Zero(t, client.calls.Load(), "an uncommitted pause must not dispatch a resumed call")
}
