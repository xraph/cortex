package sqlite

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/learning"
	"github.com/xraph/cortex/store/internal/assurance"

	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/storetest"
)

func TestQualifiedLearning(t *testing.T) {
	storetest.QualifiedLearning(t, func(t *testing.T) store.Store { return newTestStore(t) })
}

func TestLessonRetentionRemovesHistoricalBodies(t *testing.T) {
	s := newTestStore(t)
	ctx, request := storetest.AcceptedLessonFixture(t, s)
	publication, err := s.AcceptLesson(ctx, request)
	require.NoError(t, err)
	_, err = s.ClaimLessonPublication(ctx, request.ID, 1, "worker")
	require.NoError(t, err)
	_, err = s.RevokeLesson(ctx, learning.RevokeRequest{ID: request.ID, ExpectedRevision: 1, Actor: "operator", Reason: "withdrawn"})
	require.NoError(t, err)
	_, err = s.DeleteLesson(ctx, learning.DeletionRequest{ID: request.ID, ExpectedRevision: 2, Actor: "retention", Reason: "expired"})
	require.NoError(t, err)
	driver := assuranceDriver{store: s}
	for _, kind := range assurance.LessonPayloadKinds() {
		rows, readErr := driver.Select(ctx, assurance.Query{Kind: kind, Limit: 100})
		require.NoError(t, readErr)
		require.NotEmpty(t, rows)
		for _, row := range rows {
			require.True(t, row.Deleted)
			require.Equal(t, "{}", row.Body)
			require.False(t, strings.Contains(row.Body, publication.Payload.Statement))
		}
	}
}
