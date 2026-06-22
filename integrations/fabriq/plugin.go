package fabriqbrain

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/command"
	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex/id"
)

// rememberer is the narrow slice of *agent.Toolkit the plugin needs.
type rememberer interface {
	Remember(ctx context.Context, req agent.RememberRequest) (command.Result, error)
}

// opCreate is the fabriq command op the learning-loop plugin uses for memory writes.
const opCreate = "create"

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

	v, _ := p.inflight.LoadAndDelete(runID.String())
	var in string
	if s, ok := v.(string); ok {
		in = s
	}

	content := strings.TrimSpace(in + "\n" + output)
	if content == "" {
		p.cfg.logger.Warn("fabriq-brain: empty run content; skipping memory write")
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"content": content,
		"meta": map[string]any{
			"kind":      "completed",
			"agentId":   agentID.String(),
			"runId":     runID.String(),
			"input":     in,
			"output":    output,
			"elapsedMs": elapsed.Milliseconds(),
		},
	})
	if err != nil {
		p.cfg.logger.Warn("fabriq-brain: marshal memory payload failed", log.String("error", err.Error()))
		return nil
	}
	if _, rerr := p.rem.Remember(ctx, agent.RememberRequest{
		Entity:  p.cfg.memoryEntity,
		Op:      opCreate,
		Payload: payload,
	}); rerr != nil {
		p.cfg.logger.Warn("fabriq-brain: memory write failed", log.String("error", rerr.Error()))
	}
	return nil
}

// OnRunFailed removes the run's stashed input (preventing an inflight-map leak)
// and records the failure as memory so the brain can learn from failed runs.
// Write/marshal errors are logged and swallowed.
func (p *Plugin) OnRunFailed(ctx context.Context, agentID id.AgentID, runID id.AgentRunID, runErr error) error {
	ctx = p.cfg.tenant(ctx)
	v, _ := p.inflight.LoadAndDelete(runID.String())
	var in string
	if s, ok := v.(string); ok {
		in = s
	}
	errStr := ""
	if runErr != nil {
		errStr = runErr.Error()
	}
	content := strings.TrimSpace(in + "\n" + errStr)
	if content == "" {
		p.cfg.logger.Warn("fabriq-brain: empty run content; skipping memory write")
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"content": content,
		"meta": map[string]any{
			"kind":    "failed",
			"agentId": agentID.String(),
			"runId":   runID.String(),
			"input":   in,
			"error":   errStr,
			"failed":  true,
		},
	})
	if err != nil {
		p.cfg.logger.Warn("fabriq-brain: marshal failure payload failed", log.String("error", err.Error()))
		return nil
	}
	if _, rerr := p.rem.Remember(ctx, agent.RememberRequest{
		Entity:  p.cfg.memoryEntity,
		Op:      opCreate,
		Payload: payload,
	}); rerr != nil {
		p.cfg.logger.Warn("fabriq-brain: failure memory write failed", log.String("error", rerr.Error()))
	}
	return nil
}
