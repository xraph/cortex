package mongo

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWaitMongoPrimaryRetriesElectionAndTransientErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		attempts := 0
		err := waitMongoPrimary(ctx, func(context.Context) (bool, error) {
			attempts++
			switch attempts {
			case 1:
				return false, errors.New("election in progress")
			case 2:
				return false, nil
			default:
				return true, nil
			}
		})
		require.NoError(t, err)
		require.Equal(t, 3, attempts)
	})
}
func TestWaitMongoPrimaryHonorsCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		err := waitMongoPrimary(ctx, func(context.Context) (bool, error) { return false, nil })
		require.ErrorIs(t, err, context.DeadlineExceeded)
		calls := 0
		err = waitMongoPrimary(ctx, func(context.Context) (bool, error) { calls++; return true, nil })
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Zero(t, calls)
	})
}
