package cortex_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
)

func TestRootTenant(t *testing.T) {
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "o1"}, {Key: "project", Value: "p1"}}}
	tenant, err := cortex.RootTenant(context.Background(), scope)
	require.NoError(t, err)
	require.Equal(t, "o1", tenant)

	tenant, err = cortex.RootTenant(context.Background(), cortex.Scope{})
	require.ErrorIs(t, err, cortex.ErrNoScope)
	require.Empty(t, tenant)

	var f cortex.TenantFunc = cortex.RootTenant
	tenant, err = f(context.Background(), scope)
	require.NoError(t, err)
	require.Equal(t, "o1", tenant)
}
