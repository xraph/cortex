package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/suspension"
)

func admissionFixture(t *testing.T, backend store.Store) (context.Context, suspension.ResumeAdmissionStore, suspension.ResumeAdmissionRequest) {
	t.Helper()
	ctx := ctxWithScope(id.NewMessageID().String())
	current, paused := mustSuspendRun(t, backend, ctx, nil)
	require.NoError(t, backend.DeleteSuspension(ctx, current.ID))
	paused.Reason = suspension.ReasonAgentReply
	paused.Pending[0].Name = "agent_ask"
	paused.Pending[0].ReplyWith = id.NewMessageID().String()
	require.NoError(t, backend.CreateSuspension(ctx, paused))
	capability, ok := backend.(suspension.ResumeAdmissionStore)
	require.True(t, ok, "resume admission capability")
	return ctx, capability, suspension.ResumeAdmissionRequest{Scope: cortex.ScopeFromContext(ctx), Version: suspension.ClaimVersion{RunID: current.ID, SuspensionID: paused.ID, RunRevision: current.Revision}, Input: []byte(`{"tool_results":[{"tool_call_id":"call_conformance","content":"answer"}]}`)}
}

func testResumeAdmission(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx, durable, req := admissionFixture(t, backend)
	admitted, err := durable.AdmitResume(ctx, req)
	require.NoError(t, err)
	require.True(t, admitted.Fresh)
	current, err := backend.GetRun(ctx, req.Version.RunID)
	require.NoError(t, err)
	require.Equal(t, run.StateRunning, current.State)
	require.Equal(t, req.Version.RunRevision+1, current.Revision)
	again, err := durable.AdmitResume(ctx, req)
	require.NoError(t, err)
	require.False(t, again.Fresh)
	require.Equal(t, admitted.RequestDigest, again.RequestDigest)
	changed := req
	changed.Input = []byte(`{"tool_results":[]}`)
	_, err = durable.AdmitResume(ctx, changed)
	require.ErrorIs(t, err, suspension.ErrResumeAdmissionConflict)
	observed, err := durable.GetResumeAdmission(ctx, req.Version.RunID, req.Version.SuspensionID)
	require.NoError(t, err)
	require.Equal(t, req.Input, observed.Request.Input)
	require.False(t, observed.Fresh)
	_, err = durable.GetResumeStart(ctx, req.Version.RunID, req.Version.SuspensionID)
	require.ErrorIs(t, err, suspension.ErrResumeAdmissionNotFound)
	var wg sync.WaitGroup
	starts := make(chan suspension.ResumeStart, 4)
	failures := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			started, startErr := durable.StartResume(ctx, req.Version.RunID, req.Version.SuspensionID, admitted.RequestDigest)
			starts <- started
			failures <- startErr
		})
	}
	wg.Wait()
	close(starts)
	close(failures)
	for startErr := range failures {
		require.NoError(t, startErr)
	}
	winners := 0
	for started := range starts {
		if started.Fresh {
			winners++
			require.NotNil(t, started.Suspension)
			require.Equal(t, current.Revision, started.Suspension.ClaimedRun.Revision)
		} else {
			require.Nil(t, started.Suspension)
		}
	}
	require.Equal(t, 1, winners)
	started, err := durable.GetResumeStart(ctx, req.Version.RunID, req.Version.SuspensionID)
	require.NoError(t, err)
	require.False(t, started.Fresh)
	require.Nil(t, started.Suspension)
	_, err = durable.GetResumeAdmission(context.Background(), req.Version.RunID, req.Version.SuspensionID)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	_, err = durable.GetResumeAdmission(ctxWithScope("other"), req.Version.RunID, req.Version.SuspensionID)
	require.ErrorIs(t, err, suspension.ErrResumeAdmissionNotFound)
	_, err = durable.GetResumeAdmission(ctx, id.NewAgentRunID(), req.Version.SuspensionID)
	require.ErrorIs(t, err, suspension.ErrResumeAdmissionNotFound)
	_, err = durable.GetResumeStart(ctx, id.NewAgentRunID(), req.Version.SuspensionID)
	require.ErrorIs(t, err, suspension.ErrResumeAdmissionNotFound)
	_, err = durable.StartResume(ctxWithScope("other"), req.Version.RunID, req.Version.SuspensionID, admitted.RequestDigest)
	require.ErrorIs(t, err, suspension.ErrResumeAdmissionNotFound)
	foreign, err := durable.ListResumeAdmissions(ctxWithScope("other"), "", 1)
	require.NoError(t, err)
	require.Empty(t, foreign.Admissions)
}

func testResumeAdmissionBoundaries(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	for _, mode := range []string{"revision", "replacement", "changed_continuation", "cancelled", "wrong_digest"} {
		t.Run(mode, func(t *testing.T) {
			ctx, durable, req := admissionFixture(t, backend)
			admitted, err := durable.AdmitResume(ctx, req)
			require.NoError(t, err)
			if mode == "revision" {
				current, readErr := backend.GetRun(ctx, req.Version.RunID)
				require.NoError(t, readErr)
				require.NoError(t, backend.UpdateRun(ctx, current))
			}
			if mode == "replacement" || mode == "changed_continuation" {
				paused, readErr := backend.GetSuspension(ctx, req.Version.RunID)
				require.NoError(t, readErr)
				require.NoError(t, backend.DeleteSuspension(ctx, req.Version.RunID))
				if mode == "replacement" {
					paused.ID = id.NewSuspensionID()
				} else {
					paused.Cont.SystemPrompt = "changed"
				}
				require.NoError(t, backend.CreateSuspension(ctx, paused))
			}
			attempt := ctx
			if mode == "cancelled" {
				var cancel context.CancelFunc
				attempt, cancel = context.WithCancel(ctx)
				cancel()
			}
			digest := admitted.RequestDigest
			if mode == "wrong_digest" {
				digest = "other"
			}
			started, startErr := durable.StartResume(attempt, req.Version.RunID, req.Version.SuspensionID, digest)
			require.Error(t, startErr)
			require.Zero(t, started)
			_, readErr := durable.GetResumeStart(ctx, req.Version.RunID, req.Version.SuspensionID)
			require.ErrorIs(t, readErr, suspension.ErrResumeAdmissionNotFound)
		})
	}
	ctx, durable, req := admissionFixture(t, backend)
	for _, input := range [][]byte{nil, []byte("not json"), {0xff}} {
		invalid := req
		invalid.Input = input
		_, err := durable.AdmitResume(ctx, invalid)
		require.ErrorIs(t, err, suspension.ErrResumeAdmissionConflict)
	}
	current, err := backend.GetRun(ctx, req.Version.RunID)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, current.State)
	_, err = durable.AdmitResume(context.Background(), req)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	_, err = durable.AdmitResume(ctxWithScope("wrong"), req)
	require.ErrorIs(t, err, cortex.ErrInvalidScope)
	for range 3 {
		_, paused := mustSuspendRun(t, backend, ctx, nil)
		require.NoError(t, backend.DeleteSuspension(ctx, paused.RunID))
		paused.Reason = suspension.ReasonAgentReply
		require.NoError(t, backend.CreateSuspension(ctx, paused))
		stored, readErr := backend.GetRun(ctx, paused.RunID)
		require.NoError(t, readErr)
		_, err = durable.AdmitResume(ctx, suspension.ResumeAdmissionRequest{Scope: req.Scope, Version: suspension.ClaimVersion{RunID: paused.RunID, SuspensionID: paused.ID, RunRevision: stored.Revision}, Input: req.Input})
		require.NoError(t, err)
	}
	seen := map[string]bool{}
	after := ""
	for {
		page, pageErr := durable.ListResumeAdmissions(ctx, after, 1)
		require.NoError(t, pageErr)
		require.Len(t, page.Admissions, 1)
		key := page.Admissions[0].Request.Version.SuspensionID.String()
		require.False(t, seen[key])
		seen[key] = true
		if page.Next == "" {
			break
		}
		after = page.Next
	}
	require.Len(t, seen, 3)
	_, err = durable.ListResumeAdmissions(ctx, "invalid", 1)
	require.ErrorIs(t, err, suspension.ErrResumeAdmissionConflict)
	_, err = durable.ListResumeAdmissions(ctx, "", 257)
	require.ErrorIs(t, err, suspension.ErrResumeAdmissionConflict)
}

// ResumeAdmissionTransactions verifies rollback and ambiguous acknowledgments at
// both durable boundaries using each backend's native transaction implementation.
func ResumeAdmissionTransactions(t *testing.T, backend store.Store, driver assurance.Driver) {
	t.Helper()
	transaction, ok := driver.(assurance.RunDriver)
	require.True(t, ok)
	for _, boundary := range []string{"admit", "start"} {
		for _, lostAck := range []bool{false, true} {
			ctx, durable, req := admissionFixture(t, backend)
			fault := errors.New("lost or rolled back resume commit")
			var wrapped assurance.Driver = failingRunDriver{Driver: driver, transaction: transaction, failure: fault}
			if lostAck {
				wrapped = privateLostReplyDriver{Driver: driver, transaction: transaction, failure: fault}
			}
			faulty, supported := any(assurance.New(wrapped)).(suspension.ResumeAdmissionStore)
			require.True(t, supported)
			var admitted suspension.ResumeAdmission
			var err error
			if boundary == "admit" {
				admitted, err = faulty.AdmitResume(ctx, req)
				require.ErrorIs(t, err, fault)
				require.Zero(t, admitted)
				admitted, err = durable.AdmitResume(ctx, req)
				require.NoError(t, err)
				require.Equal(t, !lostAck, admitted.Fresh)
			} else {
				admitted, err = durable.AdmitResume(ctx, req)
				require.NoError(t, err)
				started, startErr := faulty.StartResume(ctx, req.Version.RunID, req.Version.SuspensionID, admitted.RequestDigest)
				require.ErrorIs(t, startErr, fault)
				require.Zero(t, started)
			}
			started, startErr := durable.StartResume(ctx, req.Version.RunID, req.Version.SuspensionID, admitted.RequestDigest)
			require.NoError(t, startErr)
			require.Equal(t, boundary != "start" || !lostAck, started.Fresh)
			if !started.Fresh {
				require.Nil(t, started.Suspension)
			}
		}
	}
	ctx, _, req := admissionFixture(t, backend)
	paused, err := backend.GetSuspension(ctx, req.Version.RunID)
	require.NoError(t, err)
	now := time.Now().UTC()
	deadline := now.Add(time.Minute)
	paused.ExpiresAt = &deadline
	require.NoError(t, backend.DeleteSuspension(ctx, req.Version.RunID))
	require.NoError(t, backend.CreateSuspension(ctx, paused))
	durable := assurance.New(driver).WithClock(rosterClock{at: now})
	admitted, err := durable.AdmitResume(ctx, req)
	require.NoError(t, err)
	expired := assurance.New(driver).WithClock(rosterClock{at: deadline.Add(time.Second)})
	started, err := expired.StartResume(ctx, req.Version.RunID, req.Version.SuspensionID, admitted.RequestDigest)
	require.ErrorIs(t, err, cortex.ErrSuspensionExpired)
	require.Zero(t, started)
	_, err = durable.GetResumeStart(ctx, req.Version.RunID, req.Version.SuspensionID)
	require.ErrorIs(t, err, suspension.ErrResumeAdmissionNotFound)
}
