package engine

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/run"
)

// finalizeUncertainEffect records the stop without publishing an unacknowledged
// tool result or saving an unfinished assistant/tool exchange into history.
// The durable effect journal remains the authority for provider reconciliation.
func (e *Engine) finalizeUncertainEffect(ctx context.Context, r *run.Run, st *reactState) error {
	r.Error = cortex.ErrEffectUncertain.Error()
	r.Usage = st.usage
	return e.finalizeRun(ctx, r, run.Outcome{StopReason: run.StopToolError, StepCount: st.stepIndex, TokensUsed: st.tokensUsed, CompletedAt: time.Now().UTC()})
}
