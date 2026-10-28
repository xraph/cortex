// Package engine provides the central Cortex agent orchestration coordinator.
package engine

import (
	"context"
	"fmt"
	"time"

	log "github.com/xraph/go-utils/log"
	gometrics "github.com/xraph/go-utils/metrics"

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
//
// A nil l keeps the no-op logger.
func WithLogger(l log.Logger) Option {
	return func(e *Engine) error {
		if l == nil {
			l = log.NewNoopLogger()
		}
		e.logger = l
		return nil
	}
}

// WithMetrics records the engine's metrics on m: runs, steps, tool calls,
// publication checks, suspensions and orchestrations, plus the message
// bus's deliveries when WithA2A is set. Every label is a bounded enum. In a
// Forge app pass app.Metrics(); the cortex Forge extension does it for you,
// and the scrape shows each name with a forge_ prefix. A nil m, the
// default, records nothing.
//
// Spans need no option: the engine traces through the global OpenTelemetry
// tracer provider, which Forge installs.
func WithMetrics(m gometrics.Metrics) Option {
	return func(e *Engine) error {
		e.metrics = m
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

// WithRunConcurrency caps how many runs execute at once. A run that finds
// every slot taken waits for one, or for its ctx to end, in which case it
// returns an error matching ctx.Err() and leaves no run row. Zero, the
// default, is unlimited; a negative n is an error.
//
// It covers RunAgent, StreamAgent, RunPrepared, StreamPrepared, Resume,
// ResumeStream, ResolveCheckpoint and the resumes the message bus and
// agent-reply recovery start. A stream holds its slot until its channel
// closes. A run started from inside an admitted run, on the ctx it was
// given, shares its parent's slot, so a tool that runs another agent
// cannot deadlock on a limit of one. Runs started by the experimental task
// workers, prepared orchestrations and plan replanning are not counted.
//
// cortex.Config.RunConcurrency is not read by the engine; the Forge
// extension applies its run_concurrency setting through this option.
func WithRunConcurrency(n int) Option {
	return func(e *Engine) error {
		if n < 0 {
			return fmt.Errorf("cortex: run concurrency %d is negative; use 0 for no limit", n)
		}
		e.runConcurrency = n
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

// WithToolRetrySafe marks registered tools as safe to run again after a
// crash: they only read, or they are idempotent through the effect
// journal. Every name must be registered with WithTool by the time New
// returns, or New fails.
func WithToolRetrySafe(names ...string) Option {
	return func(e *Engine) error {
		e.retrySafeNames = append(e.retrySafeNames, names...)
		return nil
	}
}

// WithToolAuthorizer installs the host's authorization decisions. A nil
// authorizer, or none at all, allows every tool and every call. A non-nil
// one needs WithPrincipalCodec as well, so a resumed run reaches it under
// the principal that started the run.
func WithToolAuthorizer(a cortex.ToolAuthorizer) Option {
	return func(e *Engine) error {
		e.authorizer = a
		return nil
	}
}

// WithPrincipalCodec installs the codec that carries a run's principal
// across a pause. The engine encodes cortex.PrincipalFromContext when it
// creates a run and stores the bytes with it; every resume decodes them
// and continues under that principal, not the resumer's. ResolveCheckpoint
// also encodes the decider's principal into Decision.DecidedByPrincipal.
//
// A host that installs a tool authorizer must install one, or New returns
// cortex.ErrPrincipalCodecRequired: without it, an authorizer would see
// every resumed run as nobody. Without a codec a resumed run has a nil
// principal.
func WithPrincipalCodec(c cortex.PrincipalCodec) Option {
	return func(e *Engine) error {
		e.principalCodec = c
		return nil
	}
}

// WithRejectionsReturnToModel makes a rejected approval an error result the
// model reads, and the run continues. Each pending call is answered with
// "rejected by <DecidedBy>: <Reason>" and recorded as denied; none of them
// is dispatched. DecideCheckpoint marks the run ready, exactly as an
// approval does, and ResolveCheckpoint continues it before returning.
//
// Without it, the default, a rejection fails the run with the decider's
// reason.
func WithRejectionsReturnToModel() Option {
	return func(e *Engine) error {
		e.rejectionsToModel = true
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
//
// Pass ExternalTTL to give a pause on this tool its own deadline.
func WithExternalTool(def llm.Tool, opts ...ExternalToolOption) Option {
	return func(e *Engine) error {
		t := registeredExternal{def: def}
		for _, opt := range opts {
			opt(&t)
		}
		if t.ttl < 0 {
			return fmt.Errorf("cortex: external tool %q TTL %s is negative", def.Name, t.ttl)
		}
		e.externalTools = append(e.externalTools, t)
		return nil
	}
}

// ExternalToolOption configures one WithExternalTool registration.
type ExternalToolOption func(*registeredExternal)

// registeredExternal is one WithExternalTool registration, named after
// registeredTool, its WithTool counterpart.
type registeredExternal struct {
	def llm.Tool
	// ttl is how long a pause on this tool waits, zero for the engine's
	// suspension TTL.
	ttl time.Duration
}

// ExternalTTL sets how long a run paused on this tool waits for its
// result before the sweeper fails it, in place of the engine's suspension
// TTL. Zero keeps the engine's. A negative duration is refused, for the
// same reason WithSuspensionTTL refuses one.
func ExternalTTL(d time.Duration) ExternalToolOption {
	return func(t *registeredExternal) { t.ttl = d }
}

// WithAgentReplyGrace sets how long past an ask's reply-by a run waiting
// on another agent's answer stays paused before the sweeper fails it. It
// defaults to defaultAgentReplyGrace.
//
// The grace exists because the ask has a deadline of its own, and the
// message bus answers it first: at reply-by the ask sweep resumes the
// asking run with a timeout the model can read and react to. The engine's
// deadline comes later, so its sweep only fails a run the ask sweep never
// reached, a bus that was down or a resume that failed. Zero puts the two
// deadlines on one instant, and which sweep wins is then down to timing.
// A negative duration is refused.
func WithAgentReplyGrace(d time.Duration) Option {
	return func(e *Engine) error {
		if d < 0 {
			return fmt.Errorf("cortex: agent reply grace %s is negative", d)
		}
		e.agentReplyGrace = d
		return nil
	}
}

// WithSuspensionTTL sets the default deadline for a suspended run: how
// long it waits for the outside world before the expiry sweeper fails it.
// It defaults to defaultSuspensionTTL.
//
// It is a default, not the only deadline. An approval can carry its own
// (cortex.ApprovalRequired.TTL), an external tool can (ExternalTTL), and
// a run waiting on another agent follows that ask's reply-by. A
// suspension expires at the earliest deadline among its pending calls.
//
// A TTL of zero means no default deadline: a pending call with no TTL of
// its own is written with none and never expires. That is the right
// setting for a host whose pauses are answered by a person on their own
// schedule and who would rather have a run sit paused for a week than
// have it failed out from under them. The sweeper still runs, because a
// call that does carry its own deadline still has to be failed when it
// passes. Zero is not the default, because the failure it prevents is
// visible and recoverable while the one it allows (a run paused forever
// on a browser tab somebody closed) is neither.
//
// A negative duration is refused rather than clamped. It reads as an
// already-passed deadline, so accepting it would have every suspension
// swept on the next tick, which is the opposite of what anyone typing a
// negative number means.
func WithSuspensionTTL(d time.Duration) Option {
	return func(e *Engine) error {
		if d < 0 {
			return fmt.Errorf("cortex: suspension TTL %s is negative; use 0 for no default deadline", d)
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
			return fmt.Errorf("cortex: suspension sweep interval %s must be positive", d)
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

// WithToolRevision records the host's immutable implementation revision. It is
// checked on prepared execution and resume; definition hashes are stored too.
func WithToolRevision(name, revision string) Option {
	return func(e *Engine) error {
		if name == "" || revision == "" {
			return fmt.Errorf("cortex: tool revision requires name and revision")
		}
		if e.toolRevisions == nil {
			e.toolRevisions = make(map[string]string)
		}
		e.toolRevisions[name] = revision
		return nil
	}
}

// WithModelRevision pins a host-resolved model version behind a model selector.
func WithModelRevision(model, revision string) Option {
	return func(e *Engine) error {
		if model == "" || revision == "" {
			return fmt.Errorf("cortex: model revision requires model and revision")
		}
		if e.modelRevisions == nil {
			e.modelRevisions = make(map[string]string)
		}
		e.modelRevisions[model] = revision
		return nil
	}
}

// WithPublicationPolicy configures the host's publication boundary.
func WithPublicationPolicy(policy safety.Policy) Option {
	return func(e *Engine) error { e.publicationPolicy = policy; return nil }
}

// WithPublicationGate installs a host checker. It takes precedence over the
// scanner adapter, and must advertise IncrementalGate for checked segments.
func WithPublicationGate(gate safety.PublicationGate) Option {
	return func(e *Engine) error { e.publicationGate = gate; return nil }
}

// WithAtomicPreparation requires a consistent store snapshot for each new
// preparation. Unsupported stores and external knowledge references fail closed.
// Default preparation continues to use diagnostic read-set checks.
func WithAtomicPreparation() Option {
	return func(e *Engine) error { e.atomicPreparation = true; return nil }
}
