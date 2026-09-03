package engine

import (
	"context"
	"time"

	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
)

// a2aConfig is what WithA2A recorded, held until New has finished running
// every option and can actually build the bus.
type a2aConfig struct {
	opts a2a.Options
}

// WithA2A turns on agent-to-agent messaging.
//
// The three tools (agent_send, agent_ask, agent_inbox) appear in an
// agent's tool list only when this is set, so a host that does not
// configure it sees no new tools, no new behaviour, and no rows in the
// four a2a tables.
//
// Messaging needs a store: the ask that suspends a run is durable, and a
// pending ask nobody can read back is a run nothing could ever resume.
func WithA2A(opts a2a.Options) Option {
	return func(e *Engine) error {
		e.a2aCfg = &a2aConfig{opts: opts}
		return nil
	}
}

// buildA2A constructs the bus once every option has run. It is called from
// New rather than from WithA2A, because the bus needs the store and the
// plugin registry and an option cannot know what order the caller put
// them in.
func (e *Engine) buildA2A() error {
	if e.a2aCfg == nil {
		return nil
	}
	if e.store == nil {
		return cortex.ErrNoStore
	}
	bus, err := a2a.NewBus(a2a.BusConfig{
		Store:    e.store,
		Runner:   agentRunnerAdapter{eng: e},
		Resumer:  a2aResumer{eng: e},
		Resolver: a2aResolver{eng: e},
		Hooks:    a2aHooks{eng: e},
		Options:  e.a2aCfg.opts,
	})
	if err != nil {
		return err
	}
	e.a2a = bus
	return nil
}

// A2A returns the message bus, or nil when messaging is off. A host needs
// it to inject a message from outside a run: an operator answering an
// agent, or an HTTP handler carrying one in.
func (e *Engine) A2A() *a2a.Bus { return e.a2a }

// a2aResumer is the bus's way back into a paused run. It is deliberately
// the only thing that reaches resumeSourceAgentReply: the bus gets here
// having claimed the ledger row that proves the reply is genuine, and a
// public caller has no such row to show.
type a2aResumer struct{ eng *Engine }

func (a a2aResumer) ResumeAgentReply(ctx context.Context, runID id.AgentRunID, callID, result string) error {
	in := ResumeInput{ToolResults: []ToolResult{{ToolCallID: callID, Content: result}}}
	_, err := a.eng.resume(ctx, runID, in, resumeSourceAgentReply)
	return err
}

// a2aResolver answers the bus's "does this agent exist" question with an
// agent lookup in the caller's own scope. It is what turns a message to a
// typo into an error the sending model can read, instead of a run that
// pauses against a recipient that will never answer.
type a2aResolver struct{ eng *Engine }

func (a a2aResolver) ResolveAddress(ctx context.Context, addr a2a.Address) error {
	if !addr.IsLocal() {
		// A remote address is the remote transport's to resolve, and
		// there is no remote transport yet. Refusing here would be this
		// resolver overruling a transport that said it could reach it.
		return nil
	}
	if a.eng.store == nil {
		return cortex.ErrNoStore
	}
	if _, err := a.eng.store.GetByName(ctx, addr.Agent); err != nil {
		return err
	}
	return nil
}

// a2aHooks adapts the plugin registry to the bus's emitter.
type a2aHooks struct{ eng *Engine }

func (h a2aHooks) MessageSent(ctx context.Context, msgID id.MessageID, from, to, performative string) {
	h.eng.extensions.EmitMessageSent(ctx, msgID, from, to, performative)
}

func (h a2aHooks) MessageDelivered(ctx context.Context, msgID id.MessageID, to string) {
	h.eng.extensions.EmitMessageDelivered(ctx, msgID, to)
}

func (h a2aHooks) MessageRefused(ctx context.Context, msgID id.MessageID, to, reason string) {
	h.eng.extensions.EmitMessageRefused(ctx, msgID, to, reason)
}

// startA2A brings the bus up with the engine: orphaned deliveries from a
// previous process are redriven first, then the workers start.
func (e *Engine) startA2A(ctx context.Context) {
	if e.a2a == nil {
		return
	}
	// Startup is when abandoned deliveries are most likely: whatever was
	// in flight when the last process stopped is still marked as being
	// carried. Reclaiming before the redrive means the redrive finds them.
	if n, err := e.a2a.ReclaimStaleDeliveries(ctx); err != nil {
		e.logger.Warn("cortex: a2a delivery reclaim failed", log.Error(err))
	} else if n > 0 {
		e.logger.Info("cortex: a2a reclaimed abandoned deliveries", log.Int("count", n))
	}
	if n, err := e.a2a.Redrive(ctx); err != nil {
		e.logger.Warn("cortex: a2a redrive failed", log.Error(err))
	} else if n > 0 {
		e.logger.Info("cortex: a2a redrove orphaned deliveries", log.Int("count", n))
	}
	if err := e.a2a.Start(ctx); err != nil {
		e.logger.Warn("cortex: a2a dispatcher failed to start", log.Error(err))
		return
	}
	e.startAskSweep(ctx)
}

// startAskSweep resolves overdue asks into failures on the bus's own
// interval. It runs AHEAD of the engine's suspension sweep on purpose:
// that sweep fails a run nobody answered in time, which is the wrong verb
// for a peer that did not reply. An agent that learns its peer went quiet
// can do something about it.
func (e *Engine) startAskSweep(ctx context.Context) {
	interval := e.a2aCfg.opts.SweepInterval
	if interval <= 0 {
		interval = a2a.DefaultSweepInterval
	}
	sweepCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	e.a2aSweepCancel = cancel
	e.a2aSweepDone = make(chan struct{})

	go func() {
		defer close(e.a2aSweepDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-ticker.C:
				if _, err := e.a2a.SweepExpiredAsks(sweepCtx); err != nil {
					e.logger.Warn("cortex: a2a ask sweep failed", log.Error(err))
				}
				// A delivery whose worker died is put back in the queue
				// on the same pass. Nothing wedges without this, because
				// an ask resolves on its deadline either way, but an
				// informative caught mid-delivery would simply be lost.
				if n, err := e.a2a.ReclaimStaleDeliveries(sweepCtx); err != nil {
					e.logger.Warn("cortex: a2a delivery reclaim failed", log.Error(err))
				} else if n > 0 {
					e.logger.Info("cortex: a2a reclaimed abandoned deliveries", log.Int("count", n))
				}
			}
		}
	}()
}

// stopA2A stops the dispatcher and the ask sweep and WAITS for both.
// Signalling without waiting would let Stop return while a delivery was
// still writing, which is the same hazard stopSweeper exists to avoid.
func (e *Engine) stopA2A() {
	if e.a2a == nil {
		return
	}
	if e.a2aSweepCancel != nil {
		e.a2aSweepCancel()
		<-e.a2aSweepDone
		e.a2aSweepCancel, e.a2aSweepDone = nil, nil
	}
	e.a2a.Stop()
}
