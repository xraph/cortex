package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

type continuationCompletionFault struct{ *assurance.Repository }

func (s continuationCompletionFault) CompleteEffectContinuation(ctx context.Context, r execution.EffectContinuationRequest, receipt execution.EffectReceipt) (execution.EffectContinuationResult, error) {
	result, err := s.Repository.CompleteEffectContinuation(ctx, r, receipt)
	if err != nil {
		return result, err
	}
	return execution.EffectContinuationResult{}, errAssignmentInjected
}
func testContinuationCompletionFaults(t *testing.T, backend store.Store, driver assurance.Driver) {
	t.Run("lost terminal acknowledgment", func(t *testing.T) {
		f, r := effectContinuationFixture(t, backend, driver, execution.EffectNonIdempotent, false)
		adapter := &continuationAdapter{}
		executor := continuationExecutor(t, continuationCompletionFault{f.repo}, r, adapter)
		_, err := executor.ContinueEffect(f.ctx, r, []byte("payload"), continuationAuthority{})
		require.ErrorIs(t, err, execution.ErrEffectUncertain)
		require.Equal(t, 1, adapter.calls)
		recovery := continuationExecutor(t, f.repo, r, adapter)
		result, err := recovery.ContinueEffect(f.ctx, r, []byte("payload"), continuationAuthority{})
		require.NoError(t, err)
		require.Equal(t, 1, adapter.calls)
		require.Zero(t, adapter.probes)
		_, err = f.repo.LoadEvidence(f.ctx, result.Evidence)
		require.NoError(t, err)
	})
	t.Run("artifact failure rolls back terminal accounting", func(t *testing.T) {
		f, r := effectContinuationFixture(t, backend, driver, execution.EffectNonIdempotent, false)
		admission, err := f.repo.AdmitEffectContinuation(f.ctx, r)
		require.NoError(t, err)
		plans, ok := driver.(assurance.PlanDriver)
		require.True(t, ok)
		fault := &submissionFenceDriver{Driver: driver, plans: plans, afterInsert: func(_ context.Context, row assurance.Record) error {
			if row.Kind == assurance.KindEvidence {
				return errAssignmentInjected
			}
			return nil
		}}
		_, err = assurance.New(fault).WithClock(f.clock).CompleteEffectContinuation(f.ctx, r, continuationReceipt(r.Operation, false))
		require.ErrorIs(t, err, errAssignmentInjected)
		result, err := f.repo.GetEffectContinuationResult(f.ctx, r)
		require.NoError(t, err)
		require.Nil(t, result)
		reservation, err := f.repo.GetReservation(f.ctx, admission.Reservation.Request.ID)
		require.NoError(t, err)
		require.Nil(t, reservation.Settlement)
		resultValue, err := f.repo.CompleteEffectContinuation(f.ctx, r, continuationReceipt(r.Operation, false))
		require.NoError(t, err)
		require.NoError(t, resultValue.Validate(admission))
	})
	t.Run("backoff and predecessor are persisted", func(t *testing.T) {
		f, r := effectContinuationFixture(t, backend, driver, execution.EffectNonIdempotent, false)
		_, err := f.repo.AdmitEffectContinuation(f.ctx, r)
		require.NoError(t, err)
		receipt := continuationReceipt(r.Operation, true)
		_, err = f.repo.CompleteEffectContinuation(f.ctx, r, receipt)
		require.NoError(t, err)
		next := r
		next.Sequence++
		next.Operation.AttemptID = id.New(execution.PrefixEffectAttempt)
		next.Reservation.ID = id.New(execution.PrefixReservation)
		next.Reservation.OwnerRef = next.Operation.AttemptID.String()
		next.ResultID = id.New(evidence.PrefixArtifact)
		next.PredecessorDigest, err = execution.EffectPredecessorDigest(r.Operation, &receipt)
		require.NoError(t, err)
		_, err = f.repo.AdmitEffectContinuation(f.ctx, next)
		require.ErrorIs(t, err, execution.ErrEffectConflict, "backoff survives restart")
		f.clock.advance(time.Second)
		next.Proof.ObservedAt = f.clock.Now()
		altered := next
		altered.PredecessorDigest = evidence.Digest(nil)
		_, err = f.repo.AdmitEffectContinuation(f.ctx, altered)
		require.ErrorIs(t, err, execution.ErrEffectConflict)
		got, err := assurance.New(driver).WithClock(f.clock).AdmitEffectContinuation(f.ctx, next)
		require.NoError(t, err)
		require.True(t, got.Fresh)
	})
}
