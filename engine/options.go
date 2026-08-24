// Package engine provides the central Cortex agent orchestration coordinator.
package engine

import (
	"context"
	"fmt"
	"time"

	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/knowledge"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/plugin"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/store"
)

// Option configures the Engine.
type Option func(*Engine) error

// WithStore sets the composite store.
func WithStore(s store.Store) Option {
	return func(e *Engine) error {
		e.store = s
		return nil
	}
}

// WithExtension registers an extension with the engine.
func WithExtension(ext plugin.Extension) Option {
	return func(e *Engine) error {
		e.pendingExts = append(e.pendingExts, ext)
		return nil
	}
}

// WithLogger sets the structured logger.
func WithLogger(l log.Logger) Option {
	return func(e *Engine) error {
		e.logger = l
		return nil
	}
}

// WithConfig sets the engine configuration.
func WithConfig(cfg cortex.Config) Option {
	return func(e *Engine) error {
		e.config = cfg
		return nil
	}
}

// WithLLM sets the LLM client for real model execution.
// When set, RunAgent and StreamAgent use this client instead of mock/echo mode.
func WithLLM(client llm.Client) Option {
	return func(e *Engine) error {
		e.llm = client
		return nil
	}
}

// WithSafety sets the safety scanner for content scanning.
// When set, RunAgent and StreamAgent scan input before LLM calls
// and scan output after LLM responses.
func WithSafety(scanner safety.Scanner) Option {
	return func(e *Engine) error {
		e.safety = scanner
		return nil
	}
}

// WithKnowledge sets the knowledge provider for RAG-based knowledge retrieval.
// When set, skills with KnowledgeRef entries can inject relevant context
// into agent system prompts and agents gain access to a knowledge_search tool.
func WithKnowledge(provider knowledge.Provider) Option {
	return func(e *Engine) error {
		e.knowledge = provider
		return nil
	}
}

// ToolHandler executes a registered tool. The Invocation carries the
// scope, the host's principal and the call itself, so a handler never
// has to reach into the context for them.
type ToolHandler func(ctx context.Context, inv cortex.Invocation) (string, error)

// WithTool registers an externally-provided executable tool. The def is
// advertised to the LLM (resolveTools); the handler runs when the model calls
// it (executeTool). Registering tools with the same name appends both; the
// first match wins at dispatch.
func WithTool(def llm.Tool, h ToolHandler) Option {
	return func(e *Engine) error {
		e.tools = append(e.tools, registeredTool{def: def, handler: h})
		return nil
	}
}

// WithToolAuthorizer installs the host's authorization decisions. A nil
// authorizer, or none at all, allows every tool and every call.
func WithToolAuthorizer(a cortex.ToolAuthorizer) Option {
	return func(e *Engine) error {
		e.authorizer = a
		return nil
	}
}

// WithExternalTool registers a tool the engine advertises but never
// executes. When the model calls one, the run suspends and waits for the
// caller to run it and hand the result back.
//
// The definition is advertised exactly like a WithTool registration
// (resolveTools), and the authorizer gates a call to it exactly like any
// other (executeTool). The only difference is what happens at dispatch:
// there is no handler to run, so the call becomes pending and the loop
// stops.
func WithExternalTool(def llm.Tool) Option {
	return func(e *Engine) error {
		e.externalTools = append(e.externalTools, def)
		return nil
	}
}

// WithSuspensionTTL sets how long a suspended run waits for the outside
// world before the expiry sweeper fails it. It defaults to
// defaultSuspensionTTL.
//
// A TTL of zero disables expiry outright: suspensions are written with
// no deadline, and no sweeper runs. That is the right setting for a host
// whose pauses are answered by a person on their own schedule and who
// would rather have a run sit paused for a week than have it failed out
// from under them. It is not the default, because the failure it
// prevents is visible and recoverable while the one it allows (a run
// paused forever on a browser tab somebody closed) is neither.
//
// A negative duration is refused rather than clamped. It reads as an
// already-passed deadline, so accepting it would have every suspension
// swept on the next tick, which is the opposite of what anyone typing a
// negative number means.
func WithSuspensionTTL(d time.Duration) Option {
	return func(e *Engine) error {
		if d < 0 {
			return fmt.Errorf("cortex: suspension TTL %s is negative; use 0 to disable expiry", d)
		}
		e.suspensionTTL = d
		return nil
	}
}

// WithSuspensionSweepInterval sets how often the expiry sweeper looks
// for suspensions past their deadline. It defaults to
// defaultSweepInterval.
//
// The interval is the granularity of expiry, not its accuracy: a run
// whose deadline passes just after a sweep waits up to one more interval
// to be failed. Setting it far below the TTL buys nothing but load,
// since a deadline measured in hours does not need checking every
// second.
func WithSuspensionSweepInterval(d time.Duration) Option {
	return func(e *Engine) error {
		if d <= 0 {
			return fmt.Errorf("cortex: suspension sweep interval %s must be positive; use WithSuspensionTTL(0) to switch expiry off", d)
		}
		e.sweepInterval = d
		return nil
	}
}

// WithSuspensionSweepLimit caps how many expired suspensions one sweep
// takes on. A sweep that tried to drain an unbounded backlog would hold
// the store for as long as the backlog was long; the leftovers are
// picked up by the next tick.
func WithSuspensionSweepLimit(n int) Option {
	return func(e *Engine) error {
		if n <= 0 {
			return fmt.Errorf("cortex: suspension sweep limit %d must be positive", n)
		}
		e.sweepLimit = n
		return nil
	}
}
