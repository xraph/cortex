//go:build integration

package inttest_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/xraph/fabriq"
	"github.com/xraph/fabriq/core/command"
	"github.com/xraph/fabriq/core/fabriqerr"
	"github.com/xraph/fabriq/core/query"
	"github.com/xraph/fabriq/core/tenant"

	"github.com/xraph/cortex"
	fabriqbrain "github.com/xraph/cortex/integrations/fabriq"
	"github.com/xraph/cortex/learning"
	"github.com/xraph/cortex/store/storetest"
)

// qualifiedLegacyFabric intentionally exposes the previous query facade without
// the optional retention method, as an older installed transport would.
type qualifiedLegacyFabric struct{ query.Fabric }

type qualifiedErasureAuthority struct {
	tenant, scope, entity, aggregate, canonical string
	denied                                      bool
	calls                                       int
}

func (a *qualifiedErasureAuthority) WithErasureAuthorization(ctx context.Context, r command.ErasureResource, next func(context.Context, command.ErasureGrant) error) error {
	a.calls++
	if a.denied || r.TenantID != a.tenant || r.Request.ScopeID != a.scope || r.Request.Entity != a.entity || r.Request.AggregateID != a.aggregate || cortex.ScopeFromContext(ctx).Canonical() != a.canonical {
		return learning.ErrDenied
	}
	return next(ctx, command.ErasureGrant{Actor: "installed-retention-worker", AuthorityRevision: "retention-grant:1"})
}

type qualifiedErasureTransport struct {
	query.Fabric
	eraser                command.AggregateEraser
	loseDelete, loseErase bool
	calls                 []command.ErasureRequest
	receipt               command.ErasureReceipt
}

func (f *qualifiedErasureTransport) Exec(ctx context.Context, c command.Command) (command.Result, error) {
	r, err := f.Fabric.Exec(ctx, c)
	if err == nil && c.Op == command.OpDelete && f.loseDelete {
		f.loseDelete = false
		return command.Result{}, errors.New("lost delete acknowledgement")
	}
	return r, err
}
func (f *qualifiedErasureTransport) EraseAggregate(ctx context.Context, r command.ErasureRequest) (command.ErasureReceipt, error) {
	f.calls = append(f.calls, r)
	receipt, err := f.eraser.EraseAggregate(ctx, r)
	if err == nil {
		f.receipt = receipt
		if f.loseErase {
			f.loseErase = false
			return command.ErasureReceipt{}, errors.New("lost erasure acknowledgement")
		}
	}
	return receipt, err
}

func TestQualifiedLessonRetentionInstalledFabriq(t *testing.T) {
	const entity = "retained_lessons"
	permission := &qualifiedErasureAuthority{tenant: "acme", entity: entity}
	fabric := openLearningFabricOptions(t, []fabriq.Option{fabriq.WithErasureAuthorizer(permission)}, fabriqbrain.LessonSpec(entity))
	local := openQualifiedStore(t, filepath.Join(t.TempDir(), "retention.db"))
	ctx, request := storetest.AcceptedLessonFixture(t, local)
	// LessonSpec uses the exact Cortex scope_key within its Fabriq tenant.
	// It has no Fabriq scope_id column, so its native scope remains empty.
	authority := &qualifiedAuthority{}
	guard, err := learning.NewGuard(learning.GuardConfig{Repository: local, Results: local, ContentAuthority: authority, Authority: authority, Fence: authority, Clock: authority})
	require.NoError(t, err)
	publication, err := guard.Accept(ctx, request)
	require.NoError(t, err)
	permission.aggregate = publication.AggregateID
	permission.canonical = publication.Payload.Scope.Canonical()
	toolkit, err := fabriqbrain.NewLessonToolkit(fabric, fabric.Registry(), hashEmbedder{}, entity)
	require.NoError(t, err)
	transport := &qualifiedErasureTransport{Fabric: fabric, eraser: fabric, loseDelete: true, loseErase: true}
	mutateReceipt := false
	newAdapter := func() *fabriqbrain.LessonAdapter {
		a, createErr := fabriqbrain.NewLessonStore(toolkit, transport, fabriqbrain.LessonConfig{Entity: entity, Repository: local, Guard: guard, Tenant: func(context.Context, cortex.Scope) (string, error) {
			if mutateReceipt && transport.receipt.LocalPayloadsErased {
				transport.receipt.LocalTargets[0] = "unverified_target"
				transport.receipt.Downstream[0].State = "erased"
			}
			return "acme", nil
		}})
		require.NoError(t, createErr)
		return a
	}
	adapter := newAdapter()
	_, err = adapter.Deliver(ctx, publication)
	require.NoError(t, err)
	mapped, err := tenant.WithTenant(ctx, "acme")
	require.NoError(t, err)
	var resurrection map[string]any
	require.NoError(t, fabric.Relational().Get(mapped, entity, publication.AggregateID, &resurrection))
	for _, key := range []string{"id", "tenant_id", "version"} {
		delete(resurrection, key)
	}
	vector := make([]float32, 768)
	vector[0] = 1
	require.NoError(t, fabric.Vector().Upsert(mapped, entity, publication.AggregateID, vector, map[string]any{"content": "private vector copy"}))
	deletion := learning.DeletionRequest{ID: publication.LessonID, ExpectedRevision: 1, Actor: "retention-worker", Reason: "retention_expired"}
	permission.denied = true
	deniedResult, deniedErr := adapter.Delete(ctx, deletion)
	require.ErrorIs(t, deniedErr, learning.ErrDenied)
	require.False(t, deniedResult.ActiveCopiesRemoved)
	var retainedVectors []struct {
		Count int64 `grove:"count"`
	}
	require.NoError(t, fabric.Relational().Query(mapped, &retainedVectors, `SELECT count(*) AS count FROM fabriq_embeddings WHERE tenant_id=$1 AND entity=$2 AND id=$3`, "acme", entity, publication.AggregateID))
	require.Len(t, retainedVectors, 1)
	require.Equal(t, int64(1), retainedVectors[0].Count)
	permission.denied = false
	result, err := adapter.Delete(ctx, deletion)
	require.ErrorIs(t, err, learning.ErrUnavailable)
	require.False(t, result.ActiveCopiesRemoved)
	require.False(t, result.LocalPayloadsErased)
	require.Nil(t, result.LocalErasure)
	require.False(t, result.HistoryErased)
	original := transport.receipt
	require.True(t, original.LocalPayloadsErased)
	require.Empty(t, original.Request.ScopeID)
	require.Equal(t, int64(0), original.Request.ExpectedVersion)
	require.Equal(t, "cortex_retention", original.Request.ReasonCode)
	permission.denied = true
	result, err = newAdapter().Delete(ctx, deletion)
	require.ErrorIs(t, err, learning.ErrDenied)
	require.False(t, result.LocalPayloadsErased)
	require.Nil(t, result.LocalErasure)
	permission.denied = false
	result, err = newAdapter().Delete(ctx, deletion)
	require.NoError(t, err)
	require.True(t, result.ActiveCopiesRemoved)
	require.True(t, result.LocalPayloadsErased)
	require.False(t, result.HistoryErased)
	require.Equal(t, original, *result.LocalErasure)
	require.Equal(t, []string{"aggregate_row", "command_outbox", "embedding", "spatial"}, result.LocalErasure.LocalTargets)
	require.Len(t, result.LocalErasure.Downstream, 6)
	for _, target := range result.LocalErasure.Downstream {
		require.Equal(t, "unconfirmed", target.State)
	}
	for _, call := range transport.calls {
		require.Equal(t, original.Request, call)
	}
	_, err = newAdapter().SweepDeleted(ctx, "")
	require.NoError(t, err)
	var row map[string]any
	require.ErrorIs(t, fabric.Relational().Get(mapped, entity, publication.AggregateID, &row), fabriqerr.ErrNotFound)
	var counts []struct {
		Count int64 `grove:"count"`
	}
	require.NoError(t, fabric.Relational().Query(mapped, &counts, `SELECT count(*) AS count FROM fabriq_outbox WHERE tenant_id=$1 AND aggregate=$2 AND agg_id=$3 AND (payload <> '{}'::jsonb OR traceparent <> '' OR erased_at IS NULL)`, "acme", entity, publication.AggregateID))
	require.Len(t, counts, 1)
	require.Zero(t, counts[0].Count)
	require.NoError(t, fabric.Relational().Query(mapped, &counts, `SELECT count(*) AS count FROM fabriq_embeddings WHERE tenant_id=$1 AND entity=$2 AND id=$3`, "acme", entity, publication.AggregateID))
	require.Zero(t, counts[0].Count)
	_, err = fabric.Exec(mapped, command.Command{Entity: entity, Op: command.OpCreate, AggID: publication.AggregateID, Payload: resurrection})
	require.ErrorIs(t, err, fabriqerr.ErrAggregateErased)
	require.ErrorIs(t, fabric.Vector().Upsert(mapped, entity, publication.AggregateID, vector, nil), fabriqerr.ErrAggregateErased)

	// Current authority for another scope cannot reuse the retained receipt.
	foreign, err := tenant.WithScope(ctx, "another-project")
	require.NoError(t, err)
	permission.scope = "another-project"
	result, err = newAdapter().Delete(foreign, deletion)
	require.ErrorIs(t, err, learning.ErrConflict)
	require.False(t, result.ActiveCopiesRemoved)
	require.False(t, result.LocalPayloadsErased)
	require.Nil(t, result.LocalErasure)
	permission.scope = ""

	// A post-erasure mapping callback cannot alter the validated receipt through
	// slices retained by the installed facade.
	transport.receipt = command.ErasureReceipt{}
	mutateReceipt = true
	result, err = newAdapter().Delete(ctx, deletion)
	require.NoError(t, err)
	require.True(t, result.LocalPayloadsErased)
	require.Equal(t, "unverified_target", transport.receipt.LocalTargets[0])
	require.Equal(t, "erased", transport.receipt.Downstream[0].State)
	require.Equal(t, original, *result.LocalErasure)
	mutateReceipt = false
}
