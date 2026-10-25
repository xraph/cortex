package engine_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
)

func TestPreparedOrchestrationSourceCheckDetectsChanges(t *testing.T) {
	for _, change := range []string{"none", "derived", "config", "participant", "aggregator", "overlay", "scope", "cancelled", "nil"} {
		t.Run(change, func(t *testing.T) {
			f := newPreparedTeamFixture(t, orchestration.StrategyParallel)
			ctx, prepared := f.ctx, f.p
			var want error
			switch change {
			case "derived":
				var err error
				prepared, err = prepared.Derive(map[string]engine.PreparedEdits{"judge": {Temperature: new(0.25)}})
				require.NoError(t, err)
			case "config":
				f.cfg.Settings.Aggregator = "a"
				require.NoError(t, f.s.UpdateOrchestration(ctx, f.cfg))
				want = cortex.ErrPreparationConflict
			case "participant", "aggregator", "overlay":
				name := "b"
				if change == "aggregator" {
					name = "judge"
				}
				ag, err := f.s.GetByName(ctx, name)
				require.NoError(t, err)
				if change == "overlay" {
					require.NoError(t, f.s.CreateOverlay(ctx, &prompt.Overlay{ID: id.NewOverlayID(), AgentID: ag.ID}))
				} else {
					ag.SystemPrompt = "changed"
					require.NoError(t, f.s.Update(ctx, ag))
				}
				want = cortex.ErrPreparationConflict
			case "scope":
				ctx, want = overlayCtx("different"), cortex.ErrInvalidScope
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx, want = cancelled, context.Canceled
			case "nil":
				prepared, want = nil, cortex.ErrInvalidPrepared
			}
			checked, err := f.e.CheckPreparedOrchestrationSources(ctx, prepared)
			if want != nil {
				require.ErrorIs(t, err, want)
				require.Nil(t, checked)
				return
			}
			require.NoError(t, err)
			require.Equal(t, prepared.Digest(), checked.PreparedDigest)
			require.Equal(t, 7, checked.SourceCount, "one configuration and each participant's agent and absent overlay")
			require.Equal(t, f.policy.Scope.Canonical(), checked.Snapshot.Scope)
			require.NotEmpty(t, checked.Snapshot.ID)
			require.Zero(t, f.client.calls)
		})
	}
}

type changingTeamCheckSources struct {
	store.OrchestrationPreparationSources
	change func()
}

func (s changingTeamCheckSources) GetOrchestration(ctx context.Context, key id.OrchestrationConfigID) (*orchestration.Config, error) {
	config, err := s.OrchestrationPreparationSources.GetOrchestration(ctx, key)
	if err == nil {
		s.change()
	}
	return config, err
}

func TestPreparedOrchestrationSourceCheckUsesOneSnapshot(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategyParallel)
	changed, captures := false, 0
	wrapper := controlledSnapshotStore{Store: f.s, capture: func(ctx context.Context, collect func(store.PreparationSources) error) (*run.SourceSnapshot, error) {
		captures++
		return f.s.CapturePreparation(ctx, func(sources store.PreparationSources) error {
			team, ok := sources.(store.OrchestrationPreparationSources)
			require.True(t, ok)
			return collect(changingTeamCheckSources{OrchestrationPreparationSources: team, change: func() {
				if changed {
					return
				}
				changed = true
				ag, err := f.s.GetByName(ctx, "judge")
				require.NoError(t, err)
				ag.SystemPrompt = "committed after the configuration read"
				require.NoError(t, f.s.Update(ctx, ag))
			}})
		})
	}}
	checker, err := engine.New(engine.WithStore(wrapper), engine.WithPublicationGate(snapshotPublicationGate{check: func() { t.Fatal("compatibility check called scanner") }}))
	require.NoError(t, err)
	checked, err := checker.CheckPreparedOrchestrationSources(f.ctx, f.p)
	require.NoError(t, err)
	require.NotNil(t, checked)
	require.True(t, changed)
	require.Equal(t, 1, captures)
	checked, err = checker.CheckPreparedOrchestrationSources(f.ctx, f.p)
	require.ErrorIs(t, err, cortex.ErrPreparationConflict)
	require.Nil(t, checked)
	require.Equal(t, 2, captures)
}

func TestPreparedOrchestrationSourceCheckRequiresAtomicTeamView(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	for _, mode := range []string{"no_store", "no_snapshot", "no_team_view", "wrong_receipt_scope", "cancelled_after_capture"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			var selected store.Store = controlledSnapshotStore{Store: f.s, capture: func(c context.Context, collect func(store.PreparationSources) error) (*run.SourceSnapshot, error) {
				receipt, err := f.s.CapturePreparation(c, func(sources store.PreparationSources) error {
					if mode == "no_team_view" {
						return collect(struct{ store.PreparationSources }{sources})
					}
					return collect(sources)
				})
				if err == nil && mode == "wrong_receipt_scope" {
					receipt.Scope = cortex.ScopeFromContext(overlayCtx("other")).Canonical()
				}
				if mode == "cancelled_after_capture" {
					cancel()
				}
				return receipt, err
			}}
			want := cortex.ErrPreparationUnavailable
			switch mode {
			case "no_store":
				selected, want = nil, cortex.ErrNoStore
			case "no_snapshot":
				selected = struct{ store.Store }{f.s}
			case "cancelled_after_capture":
				want = context.Canceled
			}
			checker, err := engine.New(engine.WithStore(selected))
			require.NoError(t, err)
			checked, err := checker.CheckPreparedOrchestrationSources(ctx, f.p)
			require.ErrorIs(t, err, want)
			require.Nil(t, checked)
		})
	}
}

func TestPreparedOrchestrationBindingsChecksAuxiliaryAgent(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategyParallel)
	// Each child uses its own model so a missing auxiliary binding is observable.
	f.cfg.Settings.Model = ""
	require.NoError(t, f.s.UpdateOrchestration(f.ctx, f.cfg))
	judge, err := f.s.GetByName(f.ctx, "judge")
	require.NoError(t, err)
	judge.Model = "judge-model"
	require.NoError(t, f.s.Update(f.ctx, judge))
	options := []engine.Option{engine.WithStore(f.s), engine.WithLLM(bindingModel{t}), engine.WithModelRevision("team-model", "v1"), engine.WithModelRevision("judge-model", "v2")}
	preparer, err := engine.New(options...)
	require.NoError(t, err)
	prepared, err := preparer.PrepareOrchestration(f.ctx, f.cfg.ID)
	require.NoError(t, err)
	require.NoError(t, preparer.CheckPreparedOrchestrationBindings(f.ctx, prepared))
	checker, err := engine.New(options[:len(options)-1]...)
	require.NoError(t, err)
	require.ErrorIs(t, checker.CheckPreparedOrchestrationBindings(f.ctx, prepared), cortex.ErrInvalidPrepared)
	require.ErrorIs(t, preparer.CheckPreparedOrchestrationBindings(overlayCtx("other"), prepared), cortex.ErrInvalidScope)
	require.ErrorIs(t, preparer.CheckPreparedOrchestrationBindings(f.ctx, nil), cortex.ErrInvalidPrepared)
}
