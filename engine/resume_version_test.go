package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/scopespy"
	"github.com/xraph/cortex/suspension"
)

type versionClaimProbe struct {
	store.Store
	request suspension.ClaimVersion
	replace bool
}

func (*versionClaimProbe) ClaimSuspension(context.Context, id.AgentRunID) (*suspension.Suspension, error) {
	return nil, errors.New("unversioned claim used")
}
func (s *versionClaimProbe) ClaimSuspensionVersion(ctx context.Context, req suspension.ClaimVersion) (*suspension.Suspension, error) {
	s.request = req
	paused, err := s.GetSuspension(ctx, req.RunID)
	if err != nil {
		return nil, err
	}
	if s.replace {
		if err = s.DeleteSuspension(ctx, req.RunID); err != nil {
			return nil, err
		}
		paused.ID = id.NewSuspensionID()
		if err = s.CreateSuspension(ctx, paused); err != nil {
			return nil, err
		}
	}
	if paused.ID != req.SuspensionID {
		return nil, cortex.ErrNotSuspended
	}
	return s.Store.ClaimSuspension(ctx, req.RunID)
}

func TestResumePinsValidatedSuspensionVersion(t *testing.T) {
	for _, replace := range []bool{false, true} {
		spy := scopespy.New()
		wrapped := &versionClaimProbe{Store: spy, replace: replace}
		engine := mustResumeEngine(t, wrapped)
		current := suspendedFixture(t, spy, engine)
		expected := spy.Suspensions()[0].ID
		ctx := resumeCtx(scopeA())
		snapshot, err := spy.GetRun(ctx, current.ID)
		require.NoError(t, err)
		revision := snapshot.Revision
		result, err := engine.Resume(ctx, current.ID, oneResult(t, spy, "answer"))
		if replace {
			require.ErrorIs(t, err, cortex.ErrNotSuspended)
			retained, readErr := spy.GetRun(ctx, current.ID)
			require.NoError(t, readErr)
			require.Equal(t, run.StatePaused, retained.State)
		} else {
			require.NoError(t, err)
			require.Equal(t, run.StateCompleted, result.State)
		}
		require.Equal(t, suspension.ClaimVersion{RunID: current.ID, SuspensionID: expected, RunRevision: revision}, wrapped.request)
	}
}
