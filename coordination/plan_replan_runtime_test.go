package coordination_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/planning"
)

func TestPlanReplannerExecutionDeadlineBounds(t *testing.T) {
	for _, tc := range []struct {
		name        string
		timeout     time.Duration
		parent      time.Duration
		reservation time.Duration
		want        time.Duration
	}{
		{name: "default", want: 30 * time.Second},
		{name: "configured", timeout: 5 * time.Minute, want: 5 * time.Minute},
		{name: "reservation ceiling", timeout: 5 * time.Minute, reservation: time.Minute, want: time.Minute},
		{name: "caller ceiling", timeout: 5 * time.Minute, parent: 10 * time.Second, want: 10 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := liveReplanAdmission(t)
			if tc.reservation > 0 {
				a.Request.Reservation.Deadline = time.Now().Add(tc.reservation)
				var err error
				a.Request, a.RequestDigest, err = a.Request.Freeze()
				require.NoError(t, err)
				a.Reservation.Request = a.Request.Reservation
				_, a.Reservation.RequestDigest, err = a.Reservation.Request.Freeze()
				require.NoError(t, err)
			}
			ctx := cortex.WithScope(context.Background(), a.Request.Progress.Plan.Scope)
			if tc.parent > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.parent)
				defer cancel()
			}
			calls := 0
			ports := &planReplanPorts{execute: func(bounded context.Context) error {
				calls++
				deadline, ok := bounded.Deadline()
				require.True(t, ok)
				require.InDelta(t, float64(tc.want), float64(time.Until(deadline)), float64(time.Second))
				return nil
			}}
			service, err := coordination.NewPlanReplanner(&planReplanStore{admission: a, fresh: true}, ports, ports, ports, coordination.PlanReplannerConfig{ExecutionTimeout: tc.timeout})
			require.NoError(t, err)
			_, err = service.Execute(ctx, a.Request)
			require.NoError(t, err)
			require.Equal(t, 1, calls)
		})
	}
	ports := &planReplanPorts{}
	for _, configs := range [][]coordination.PlanReplannerConfig{{{ExecutionTimeout: -time.Second}}, {{ExecutionTimeout: 30*time.Minute + time.Nanosecond}}, {{}, {}}} {
		_, err := coordination.NewPlanReplanner(&planReplanStore{}, ports, ports, ports, configs...)
		require.ErrorIs(t, err, planning.ErrInvalid)
	}
}
