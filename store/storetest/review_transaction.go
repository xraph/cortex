package storetest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

// ReviewTransactions checks commit uncertainty and evidence freshness using the
// same backend transaction faults exercised by assignment submission tests.
func ReviewTransactions(t *testing.T, backend store.Store, driver assurance.Driver) {
	plans, ok := driver.(assurance.PlanDriver)
	require.True(t, ok)
	reviews := resultCapability[coordination.ReviewStore](t, backend)
	t.Run("rollback", func(t *testing.T) {
		ctx, r, _, _ := storedReviewFixture(t, backend)
		faulty := assurance.New(&submissionFenceDriver{Driver: driver, plans: plans, failInsert: true})
		result, err := faulty.RecordAssignmentReview(ctx, r)
		require.ErrorIs(t, err, errAssignmentInjected)
		require.Zero(t, result)
		_, err = reviews.GetAssignmentReview(ctx, r.ID)
		require.ErrorIs(t, err, coordination.ErrReviewNotFound)
	})
	t.Run("lost_acknowledgement", func(t *testing.T) {
		ctx, r, _, _ := storedReviewFixture(t, backend)
		faulty := assurance.New(&submissionFenceDriver{Driver: driver, plans: plans, lostAck: true})
		result, err := faulty.RecordAssignmentReview(ctx, r)
		require.ErrorIs(t, err, errAssignmentInjected)
		require.Zero(t, result)
		retry, err := assurance.New(driver).RecordAssignmentReview(ctx, r)
		require.NoError(t, err)
		require.Equal(t, r, retry)
		list, err := reviews.ListAssignmentReviews(ctx, coordination.ReviewListRequest{AssignmentID: r.SubjectAssignmentID, Limit: 10})
		require.NoError(t, err)
		require.Len(t, list, 1)
	})
	t.Run("expires_during_read", func(t *testing.T) {
		ctx, r, _, _ := storedReviewFixture(t, backend)
		clock := &jobClock{at: time.Now().UTC()}
		faulty := assurance.New(&submissionFenceDriver{Driver: driver, plans: plans, afterRead: func() { clock.advance(2 * time.Hour) }}).WithClock(clock)
		result, err := faulty.RecordAssignmentReview(ctx, r)
		require.Error(t, err)
		require.Zero(t, result)
		_, err = reviews.GetAssignmentReview(ctx, r.ID)
		require.ErrorIs(t, err, coordination.ErrReviewNotFound)
	})
}
