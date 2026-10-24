package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store/internal/assurance"
)

type runCallFaultDriver struct {
	assurance.Driver
	transaction assurance.RunCallDriver
	rollback    bool
	loseAck     bool
	afterCommit func()
}

func (d runCallFaultDriver) TransactRunCalls(ctx context.Context, runID id.AgentRunID, callback func(context.Context, assurance.Driver) error) error {
	err := d.transaction.TransactRunCalls(ctx, runID, func(tc context.Context, tx assurance.Driver) error {
		if callbackErr := callback(tc, tx); callbackErr != nil {
			return callbackErr
		}
		if d.rollback {
			return errors.New("injected rollback")
		}
		return nil
	})
	if err == nil && d.afterCommit != nil {
		d.afterCommit()
	}
	if err == nil && d.loseAck {
		return errors.New("injected lost commit acknowledgment")
	}
	return err
}

// RunCallTransactions verifies real rollback, ambiguous commits and legacy rows
// through each backend's transaction driver, without an external provider call.
func RunCallTransactions(t *testing.T, driver assurance.Driver) {
	t.Helper()
	transactional, ok := driver.(assurance.RunCallDriver)
	require.True(t, ok)
	ordinary := assurance.New(driver)
	for _, mode := range []string{"rollback", "lost acknowledgment", "cancelled acknowledgment"} {
		t.Run(mode, func(t *testing.T) {
			fault := runCallFaultDriver{Driver: driver, transaction: transactional, rollback: mode == "rollback", loseAck: mode == "lost acknowledgment"}
			r := storedRunCall()
			ctx := cortex.WithScope(t.Context(), r.Scope)
			attemptCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			if mode == "cancelled acknowledgment" {
				fault.afterCommit = cancel
			}
			faulty := assurance.New(fault)
			fresh, err := faulty.BeginRunCall(attemptCtx, r)
			require.Error(t, err)
			if mode == "cancelled acknowledgment" {
				require.ErrorIs(t, err, context.Canceled)
			}
			require.False(t, fresh, "an unacknowledged transaction cannot dispatch")
			fresh, err = ordinary.BeginRunCall(ctx, r)
			require.NoError(t, err)
			require.Equal(t, mode == "rollback", fresh)
			req := execution.RunCallSealRequest{SchemaVersion: 1, Scope: r.Scope, RunID: r.RunID, Actor: "recovery", Reason: mode}
			sealCtx, cancelSeal := context.WithCancel(ctx)
			defer cancelSeal()
			if mode == "cancelled acknowledgment" {
				fault.afterCommit = cancelSeal
				faulty = assurance.New(fault)
			}
			seal, err := faulty.SealRunCalls(sealCtx, req)
			require.Error(t, err)
			if mode == "cancelled acknowledgment" {
				require.ErrorIs(t, err, context.Canceled)
			}
			require.Empty(t, seal)
			retained, err := ordinary.GetRunCallSeal(ctx, r.RunID)
			require.NoError(t, err)
			if mode == "rollback" {
				require.Nil(t, retained)
			} else {
				require.NotNil(t, retained)
			}
			seal, err = ordinary.SealRunCalls(ctx, req)
			require.NoError(t, err)
			if retained != nil {
				require.Equal(t, *retained, seal)
			}
			fresh, err = ordinary.BeginRunCall(ctx, r)
			require.ErrorIs(t, err, execution.ErrRunCallsSealed)
			require.False(t, fresh)
		})
	}
	for _, mode := range []string{"rollback", "lost acknowledgment", "cancelled acknowledgment"} {
		t.Run("bound effect "+mode, func(t *testing.T) {
			r := storedBoundEffect()
			ctx := cortex.WithScope(t.Context(), r.Scope)
			fresh, err := ordinary.BeginRunCall(ctx, *r.RunCall)
			require.NoError(t, err)
			require.True(t, fresh)
			attemptCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			fault := runCallFaultDriver{Driver: driver, transaction: transactional, rollback: mode == "rollback", loseAck: mode == "lost acknowledgment"}
			if mode == "cancelled acknowledgment" {
				fault.afterCommit = cancel
			}
			fresh, err = assurance.New(fault).BeginBoundEffect(attemptCtx, r)
			require.Error(t, err)
			require.False(t, fresh)
			if mode == "cancelled acknowledgment" {
				require.ErrorIs(t, err, context.Canceled)
			}
			retained, readErr := ordinary.GetEffect(ctx, r.OperationKey)
			if mode == "rollback" {
				require.ErrorIs(t, readErr, execution.ErrEffectNotFound)
			} else {
				require.NoError(t, readErr)
				require.Equal(t, r.RunCall, retained.Request.RunCall)
			}
			fresh, err = ordinary.BeginBoundEffect(ctx, r)
			require.NoError(t, err)
			require.Equal(t, mode == "rollback", fresh)
		})
	}
	t.Run("expired bound call", func(t *testing.T) {
		r := storedBoundEffect()
		ctx := cortex.WithScope(t.Context(), r.Scope)
		_, err := ordinary.BeginRunCall(ctx, *r.RunCall)
		require.NoError(t, err)
		expired := assurance.New(driver).WithClock(&jobClock{at: r.RunCall.Deadline.Add(time.Second)})
		fresh, err := expired.BeginBoundEffect(ctx, r)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.False(t, fresh)
	})
	t.Run("legacy journal without guard", func(t *testing.T) {
		r := storedRunCall()
		ctx := cortex.WithScope(t.Context(), r.Scope)
		value := execution.RunCallRecord{Request: r, StartedAt: time.Now().UTC()}
		body, err := json.Marshal(value)
		require.NoError(t, err)
		columns := assurance.Columns(r.Scope)
		require.NoError(t, driver.Insert(ctx, assurance.Record{Kind: "run_call", ID: r.Key(), RunID: r.RunID.String(), ScopeCanon: r.Scope.Canonical(), ScopeL0: columns[0], ScopeL1: columns[1], ScopeL2: columns[2], ScopeExtra: "{}", Digest: evidence.Digest(body), Body: string(body)}))
		fresh, err := ordinary.BeginRunCall(ctx, r)
		require.NoError(t, err)
		require.False(t, fresh)
		_, err = ordinary.SealRunCalls(ctx, execution.RunCallSealRequest{SchemaVersion: 1, Scope: r.Scope, RunID: r.RunID, Actor: "recovery", Reason: "close legacy journal"})
		require.NoError(t, err)
		rows, err := ordinary.ListRunCalls(ctx, execution.RunCallListRequest{RunID: r.RunID, Limit: 10})
		require.NoError(t, err)
		require.Equal(t, []execution.RunCallRecord{value}, rows)
	})
}

// RunCallTransactionsUnavailable verifies that a store without usable transactions
// cannot admit calls or acknowledge closure, including after creating its anchor.
func RunCallTransactionsUnavailable(t *testing.T, s execution.RunCallSealStore) {
	t.Helper()
	r := storedRunCall()
	ctx := cortex.WithScope(t.Context(), r.Scope)
	fresh, err := s.BeginRunCall(ctx, r)
	require.ErrorIs(t, err, execution.ErrRunCallUnavailable)
	require.False(t, fresh)
	_, err = s.GetRunCall(ctx, r.RunID, r.OperationDigest)
	require.ErrorIs(t, err, execution.ErrRunCallNotFound)
	_, err = s.SealRunCalls(ctx, execution.RunCallSealRequest{SchemaVersion: 1, Scope: r.Scope, RunID: r.RunID, Actor: "recovery", Reason: "close discovery"})
	require.ErrorIs(t, err, execution.ErrRunCallUnavailable)
	sealed, err := s.GetRunCallSeal(ctx, r.RunID)
	require.NoError(t, err)
	require.Nil(t, sealed)
	effects, ok := s.(execution.EffectStore)
	require.True(t, ok)
	effect := storedEffect()
	effect.Scope = r.Scope
	fresh, err = effects.BeginEffect(ctx, effect)
	require.ErrorIs(t, err, execution.ErrRunCallUnavailable)
	require.False(t, fresh)
	_, err = effects.GetEffect(ctx, effect.OperationKey)
	require.ErrorIs(t, err, execution.ErrEffectNotFound)
}
