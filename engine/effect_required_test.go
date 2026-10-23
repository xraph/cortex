package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

func TestRequiredVerificationStopsBeforeCheckingUncertainEffect(t *testing.T) {
	for _, mode := range []string{"sync", "stream", "approval"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			client := &requiredToolClient{}
			authority := &requiredToolAuthority{approval: mode == "approval"}
			effects := 0
			e := requiredToolEngine(t, f, client, authority, engine.WithTool(llm.Tool{Name: "probe"}, func(context.Context, cortex.Invocation) (string, error) {
				effects++
				return "SECRET provider reply", fmt.Errorf("SECRET provider error: %w", cortex.ErrEffectUncertain)
			}))
			ag, overrides := requiredToolAgent(t, f)
			if mode == "stream" {
				events := make(chan engine.StreamEvent, 20)
				require.NoError(t, e.StreamAgent(f.ctx, ag.Name, "work", overrides, events))
				incomplete := false
				for event := range events {
					require.NotEqual(t, engine.EventDone, event.Type)
					data, err := json.Marshal(event)
					require.NoError(t, err)
					require.NotContains(t, string(data), "SECRET")
					incomplete = incomplete || event.Type == engine.EventIncomplete
				}
				require.True(t, incomplete)
			} else {
				r, err := e.RunAgent(f.ctx, ag.Name, "work", overrides)
				if mode == "approval" {
					require.NoError(t, err)
					require.Equal(t, run.StatePaused, r.State)
					cps, listErr := f.storage.ListPending(f.ctx, &checkpoint.ListFilter{RunID: r.ID.String()})
					require.NoError(t, listErr)
					require.Len(t, cps, 1)
					err = e.ResolveCheckpoint(f.ctx, cps[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "operator"})
				}
				require.ErrorIs(t, err, cortex.ErrEffectUncertain)
				require.NotContains(t, err.Error(), "SECRET")
			}
			rows, err := f.storage.ListRuns(f.ctx, &run.ListFilter{AgentID: ag.ID.String()})
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.Equal(t, run.StateIncomplete, rows[0].State)
			require.Equal(t, run.StopToolError, rows[0].StopReason)
			require.Equal(t, verification.StatusUnchecked, rows[0].VerificationStatus)
			require.Empty(t, rows[0].Output)
			require.Empty(t, rows[0].ResultRef)
			require.Equal(t, cortex.ErrEffectUncertain.Error(), rows[0].Error)
			require.Equal(t, 1, effects)
			require.Equal(t, 1, client.calls)
			require.Zero(t, f.adapter.evaluations)
			assertRequiredToolPrivacy(t, f, ag)
		})
	}
}
