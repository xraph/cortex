package contract

import (
	"testing"

	"github.com/stretchr/testify/require"
	dc "github.com/xraph/forge/extensions/dashboard/contract"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/verification"
)

func TestPlanningUnavailableIsDistinctFromEmptyAndDenied(t *testing.T) {
	dispatcher, _, _ := testService(t)
	raw, err := dispatch(t, dispatcher, principal("planning"), "planning.list", planningListInput{})
	require.NoError(t, err)
	page := decode[planningListOutput](t, raw)
	require.False(t, page.Available)
	require.NotEmpty(t, page.Reason)
	require.Empty(t, page.Items)
	_, err = dispatch(t, dispatcher, dc.Principal{}, "planning.list", planningListInput{})
	require.ErrorIs(t, err, dc.ErrUnauthenticated)
	_, err = dispatch(t, dispatcher, principal("planning"), "planning.detail", IDInput{ID: id.New(planning.PrefixPlan).String()})
	require.Error(t, err)
	for _, test := range []struct {
		err  error
		code dc.ErrorCode
	}{
		{coordination.ErrPlanningUnavailable, dc.CodeUnavailable},
		{coordination.ErrPlanningDenied, dc.CodePermissionDenied},
		{evidence.ErrDenied, dc.CodePermissionDenied},
		{verification.ErrResultAccessDenied, dc.CodePermissionDenied},
		{planning.ErrConflict, dc.CodeConflict},
	} {
		var mapped *dc.Error
		require.ErrorAs(t, planningError(test.err), &mapped)
		require.Equal(t, test.code, mapped.Code)
	}
}
