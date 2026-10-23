package verification_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

func TestEvaluationRegistrySelectsOnlyHostRegisteredManifest(t *testing.T) {
	manifest, req, now := evaluationManifestFixture(t)
	sealed, err := verification.SealEvaluationManifest(manifest)
	require.NoError(t, err)
	registry, err := verification.NewEvaluationRegistry([]verification.EvaluationManifest{sealed})
	require.NoError(t, err)
	ctx := cortex.WithScope(t.Context(), req.Binding.Scope)
	selected, err := registry.Resolve(ctx, req, now)
	require.NoError(t, err)
	require.Equal(t, sealed.Digest, selected.Digest)
	sealed.RequiredChecks[0] = "caller mutation"
	selected.Binding.Context.Sources[0].Revision = "returned mutation"
	again, err := registry.Resolve(ctx, req, now)
	require.NoError(t, err)
	require.Equal(t, "required-behavior", again.RequiredChecks[0])
	require.Equal(t, "1", again.Binding.Context.Sources[0].Revision)
	req.Binding.RunID = id.NewAgentRunID()
	_, err = registry.Resolve(ctx, req, now)
	require.ErrorIs(t, err, verification.ErrNotFound)
}

func TestEvaluationRegistryScopeDeadlineAndCancellation(t *testing.T) {
	manifest, req, now := evaluationManifestFixture(t)
	sealed, err := verification.SealEvaluationManifest(manifest)
	require.NoError(t, err)
	registry, err := verification.NewEvaluationRegistry([]verification.EvaluationManifest{sealed})
	require.NoError(t, err)
	_, err = registry.Resolve(t.Context(), req, now)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	wrong := cortex.WithScope(t.Context(), cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}})
	_, err = registry.Resolve(wrong, req, now)
	require.ErrorIs(t, err, verification.ErrInvalid)
	ctx := cortex.WithScope(t.Context(), req.Binding.Scope)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = registry.Resolve(cancelled, req, now)
	require.ErrorIs(t, err, context.Canceled)
	req.Deadline = now
	_, err = registry.Resolve(ctx, req, now)
	require.ErrorIs(t, err, verification.ErrInvalid)
}

func TestEvaluationRegistryRejectsChangedAndAmbiguousRegistrations(t *testing.T) {
	manifest, _, _ := evaluationManifestFixture(t)
	sealed, err := verification.SealEvaluationManifest(manifest)
	require.NoError(t, err)
	_, err = verification.NewEvaluationRegistry(nil)
	require.ErrorIs(t, err, verification.ErrInvalid)
	_, err = verification.NewEvaluationRegistry(make([]verification.EvaluationManifest, 257))
	require.ErrorIs(t, err, verification.ErrInvalid)
	_, err = verification.NewEvaluationRegistry([]verification.EvaluationManifest{sealed, sealed})
	require.ErrorIs(t, err, verification.ErrInvalid)
	sealed.RequiredChecks[0] = "unsealed replacement"
	_, err = verification.NewEvaluationRegistry([]verification.EvaluationManifest{sealed})
	require.ErrorIs(t, err, verification.ErrInvalid)
	var missing *verification.EvaluationRegistry
	_, req, now := evaluationManifestFixture(t)
	_, err = missing.Resolve(cortex.WithScope(t.Context(), req.Binding.Scope), req, now)
	require.ErrorIs(t, err, verification.ErrInvalid)
}

func TestEvaluationRegistryBoundsTotalRegisteredMaterial(t *testing.T) {
	manifests := make([]verification.EvaluationManifest, 65)
	for i := range manifests {
		m, _, _ := evaluationManifestFixture(t)
		m.RequiredChecks = make([]string, 256)
		for j := range m.RequiredChecks {
			m.RequiredChecks[j] = fmt.Sprintf("%03d-%s", j, strings.Repeat("x", 1020))
		}
		sealed, err := verification.SealEvaluationManifest(m)
		require.NoError(t, err)
		manifests[i] = sealed
	}
	_, err := verification.NewEvaluationRegistry(manifests)
	require.ErrorIs(t, err, verification.ErrInvalid)
}

func TestEvaluationRegistryConcurrentReadsAreDetached(t *testing.T) {
	manifest, req, now := evaluationManifestFixture(t)
	sealed, err := verification.SealEvaluationManifest(manifest)
	require.NoError(t, err)
	registry, err := verification.NewEvaluationRegistry([]verification.EvaluationManifest{sealed})
	require.NoError(t, err)
	ctx := cortex.WithScope(t.Context(), req.Binding.Scope)
	var group sync.WaitGroup
	failures := make(chan error, 16)
	for range 16 {
		group.Go(func() {
			selected, resolveErr := registry.Resolve(ctx, req, now)
			if resolveErr != nil {
				failures <- resolveErr
				return
			}
			selected.RequiredChecks[0] = "private local mutation"
			selected.StateProbes[0].Revision = "private local revision"
		})
	}
	group.Wait()
	close(failures)
	for failure := range failures {
		require.NoError(t, failure)
	}
	selected, err := registry.Resolve(ctx, req, now)
	require.NoError(t, err)
	require.Equal(t, "required-behavior", selected.RequiredChecks[0])
	require.Equal(t, "1", selected.StateProbes[0].Revision)
}
