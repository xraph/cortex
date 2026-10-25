package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

func testTaskCallSeal(t *testing.T, backend store.Store, driver assurance.Driver) {
	for _, mode := range []string{"accepted", "unaccepted", "stale claim", "changed digest", "foreign run", "running", "scope"} {
		t.Run(mode, func(t *testing.T) {
			ctx, acceptance := taskAcceptanceFixture(t, backend)
			repo := assurance.New(driver)
			claim, err := repo.GetTaskClaim(ctx, acceptance.Claim.PlanID, acceptance.Claim.TaskID)
			require.NoError(t, err)
			if mode != "unaccepted" {
				accepted, acceptErr := repo.AcceptTaskResult(ctx, acceptance)
				require.NoError(t, acceptErr)
				claim = accepted.Claim
			}
			request := coordination.TaskCallSealRequest{SchemaVersion: 1, Claim: claim.Token(), ClaimDigest: claim.Digest, Seal: execution.RunCallSealRequest{SchemaVersion: 1, Scope: claim.Scope, RunID: claim.RunID, Actor: "accounting", Reason: "accepted task"}}
			attempt := ctx
			switch mode {
			case "stale claim":
				request.Claim.Revision--
			case "changed digest":
				request.ClaimDigest = evidence.Digest([]byte("other claim"))
			case "foreign run":
				request.Seal.RunID = id.NewAgentRunID()
			case "running":
				current, readErr := backend.GetRun(ctx, claim.RunID)
				require.NoError(t, readErr)
				current.State = run.StateRunning
				require.NoError(t, backend.UpdateRun(ctx, current))
			case "scope":
				attempt = ctxWithScope("other")
			}
			sealed, err := repo.SealTaskCalls(attempt, request)
			if mode != "accepted" {
				require.Error(t, err)
				require.Empty(t, sealed)
				stored, readErr := repo.GetRunCallSeal(ctx, request.Seal.RunID)
				require.NoError(t, readErr)
				require.Nil(t, stored)
				return
			}
			require.NoError(t, err)
			require.NoError(t, sealed.Validate())
			retry, err := repo.SealTaskCalls(ctx, request)
			require.NoError(t, err)
			require.Equal(t, sealed, retry)
			call := storedRunCall()
			call.Scope, call.RunID = claim.Scope, claim.RunID
			_, err = repo.BeginRunCall(ctx, call)
			require.ErrorIs(t, err, execution.ErrRunCallsSealed)
			changed := request
			changed.Seal.Reason = "different closure"
			_, err = repo.SealTaskCalls(ctx, changed)
			require.ErrorIs(t, err, execution.ErrRunCallConflict)
			_, err = repo.SealTaskCalls(context.Background(), request)
			require.Error(t, err)
		})
	}
	for _, mode := range []string{"rollback", "lost acknowledgment", "cancel after commit"} {
		t.Run(mode, func(t *testing.T) {
			ctx, request := taskCallSealFixture(t, backend, driver)
			plans, ok := driver.(assurance.PlanDriver)
			require.True(t, ok)
			attempt, cancel := context.WithCancel(ctx)
			defer cancel()
			fault := &submissionFenceDriver{Driver: driver, plans: plans, failInsert: mode == "rollback", lostAck: mode == "lost acknowledgment"}
			if mode == "cancel after commit" {
				fault.afterCommit = cancel
			}
			sealed, err := assurance.New(fault).SealTaskCalls(attempt, request)
			require.Error(t, err)
			require.Empty(t, sealed)
			repo := assurance.New(driver)
			stored, err := repo.GetRunCallSeal(ctx, request.Seal.RunID)
			require.NoError(t, err)
			if mode == "rollback" {
				require.Nil(t, stored)
			} else {
				require.NotNil(t, stored)
				retried, retryErr := repo.SealTaskCalls(ctx, request)
				require.NoError(t, retryErr)
				require.Equal(t, *stored, retried)
			}
		})
	}
	t.Run("delayed call waits for seal commit", func(t *testing.T) {
		ctx, request := taskCallSealFixture(t, backend, driver)
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		plans, ok := driver.(assurance.PlanDriver)
		require.True(t, ok)
		entered, release := make(chan struct{}), make(chan struct{})
		var once, enteredOnce sync.Once
		defer once.Do(func() { close(release) })
		fault := &submissionFenceDriver{Driver: driver, plans: plans}
		fault.afterInsert = func(ctx context.Context, row assurance.Record) error {
			if row.Kind == "run_call_seal" {
				enteredOnce.Do(func() { close(entered) })
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}
		finished := make(chan error, 1)
		go func() { _, err := assurance.New(fault).SealTaskCalls(ctx, request); finished <- err }()
		select {
		case <-entered:
		case err := <-finished:
			t.Fatalf("seal ended before commit guard: %v", err)
		case <-ctx.Done():
			t.Fatal("seal did not reach commit guard")
		}
		late := make(chan error, 1)
		call := storedRunCall()
		call.Scope, call.RunID = request.Seal.Scope, request.Seal.RunID
		go func() { _, err := assurance.New(driver).BeginRunCall(ctx, call); late <- err }()
		select {
		case err := <-late:
			t.Fatalf("call escaped seal transaction: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		once.Do(func() { close(release) })
		require.NoError(t, <-finished)
		require.ErrorIs(t, <-late, execution.ErrRunCallsSealed)
	})
}

func taskCallSealFixture(t *testing.T, backend store.Store, driver assurance.Driver) (context.Context, coordination.TaskCallSealRequest) {
	t.Helper()
	ctx, request := taskAcceptanceFixture(t, backend)
	accepted, err := assurance.New(driver).AcceptTaskResult(ctx, request)
	require.NoError(t, err)
	claim := accepted.Claim
	return ctx, coordination.TaskCallSealRequest{SchemaVersion: 1, Claim: claim.Token(), ClaimDigest: claim.Digest, Seal: execution.RunCallSealRequest{SchemaVersion: 1, Scope: claim.Scope, RunID: claim.RunID, Actor: "accounting", Reason: "accepted task"}}
}
