package fabriqbrain

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/command"
)

// rememberer is the narrow slice of *agent.Toolkit the plugin needs.
type rememberer interface {
	Remember(ctx context.Context, req agent.RememberRequest) (command.Result, error)
}

// Plugin is a cortex extension that writes agent run activity back into the
// fabric so fabriq's embed + distillation workers turn it into future recall
// material. It implements plugin.RunStarted and plugin.RunCompleted.
type Plugin struct {
	rem      rememberer
	cfg      config
	inflight sync.Map // runID string -> input string
}

// NewPlugin builds the learning-loop plugin over a fabriq rememberer (a
// *agent.Toolkit satisfies rememberer).
func NewPlugin(rem rememberer, opts ...Option) *Plugin {
	return &Plugin{rem: rem, cfg: applyOptions(opts)}
}

// Name identifies the extension.
func (p *Plugin) Name() string { return "fabriq-brain" }

// OnRunStarted stashes the run input so OnRunCompleted can persist the full Q/A.
func (p *Plugin) OnRunStarted(_ context.Context, _ id.AgentID, runID id.AgentRunID, input string) error {
	p.inflight.Store(runID.String(), input)
	return nil
}

// OnRunCompleted writes a memory row for the finished run. Write failures are
// swallowed (logged by the registry) so memory persistence never fails a run.
func (p *Plugin) OnRunCompleted(ctx context.Context, agentID id.AgentID, runID id.AgentRunID, output string, elapsed time.Duration) error {
	ctx = p.cfg.tenant(ctx)

	input, _ := p.inflight.LoadAndDelete(runID.String())
	in, _ := input.(string)

	payload, err := json.Marshal(map[string]any{
		"agentId":   agentID.String(),
		"runId":     runID.String(),
		"input":     in,
		"output":    output,
		"elapsedMs": elapsed.Milliseconds(),
	})
	if err != nil {
		return nil
	}

	_, _ = p.rem.Remember(ctx, agent.RememberRequest{
		Entity:  p.cfg.memoryEntity,
		Op:      "create",
		Payload: payload,
	})
	return nil
}
