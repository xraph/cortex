package mongo

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/verification"
)

func TestVerifiedResultStandaloneUnavailable(t *testing.T) {
	s := newConformanceStore(t, false)
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "standalone"}}})
	driver := assuranceDriver{store: s}
	err := driver.TransactRun(ctx, id.NewAgentRunID(), func(txctx context.Context, tx assurance.RunTransaction) error {
		_, readErr := tx.GetRun(txctx, id.NewAgentRunID())
		return readErr
	})
	require.ErrorIs(t, err, verification.ErrCommitUnavailable)
}
