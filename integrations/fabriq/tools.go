package fabriqbrain

import (
	"context"
	"encoding/json"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/fabriq/core/agent"
)

// toolLister is the narrow slice of *agent.Toolkit the tool adapter needs.
type toolLister interface {
	Tools() []agent.Tool
}

// skippedTools are not registered as cortex tools. recall overlaps the engine's
// auto knowledge_search tool.
var skippedTools = map[string]bool{"recall": true}

// toolOptions adapts fabriq's tool catalog to cortex engine options. The tenant
// mapper from config is applied before each handler runs.
func toolOptions(tl toolLister, c config) []engine.Option {
	var opts []engine.Option
	for _, t := range tl.Tools() {
		if skippedTools[t.Name] {
			continue
		}
		t := t // capture
		def := llm.Tool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  schemaOf(t.InputSchema),
		}
		handler := func(ctx context.Context, args string) (string, error) {
			ctx = c.tenant(ctx)
			out, err := t.Handler(ctx, json.RawMessage(args))
			if err != nil {
				return "", err
			}
			b, err := json.Marshal(out)
			if err != nil {
				return "", err
			}
			return string(b), nil
		}
		opts = append(opts, engine.WithTool(def, handler))
	}
	return opts
}

// schemaOf decodes a JSON-schema RawMessage into a generic value for llm.Tool
// (whose Parameters is `any`). On failure it passes the raw bytes through.
func schemaOf(raw json.RawMessage) any {
	if len(raw) == 0 {
		return map[string]any{"type": "object"}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}
