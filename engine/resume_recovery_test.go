package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
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
}

func (s *missingReplyPauseStore) GetSuspension(context.Context, id.AgentRunID) (*suspension.Suspension, error) {
	s.revoke()
	return nil, cortex.ErrNotSuspended
}
func (s *missingReplyPauseStore) LatestResumeAdmission(ctx context.Context, key id.AgentRunID) (suspension.ResumeAdmission, error) {
	s.admissionReads++
	return s.Store.LatestResumeAdmission(ctx, key)
}

func TestAgentReplyRecoveryRechecksAuthorityBeforeRetainedAdmissionRead(t *testing.T) {
	st, ctx, current, _ := admittedReplyFixture(t, "start_lost")
	native, ok := st.(*sqlite.Store)
	require.True(t, ok)
	allowed := true
	wrapped := &missingReplyPauseStore{Store: native, revoke: func() { allowed = false }}
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
