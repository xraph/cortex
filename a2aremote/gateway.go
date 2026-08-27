package a2aremote

import (
	"context"
	"errors"
	"net/http"

	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/skill"
)

// ErrNoMessaging is returned when attaching to an engine that never
// turned messaging on. Without a bus there is nothing to deliver into
// and nothing to carry out.
var ErrNoMessaging = errors.New("cortex/a2aremote: the engine has no message bus: build it with engine.WithA2A")

// engineGateway adapts an engine to Gateway.
//
// The adapter lives on this side of the seam on purpose: the core module
// keeps knowing nothing about A2A, and the direction of the dependency
// stays one way.
type engineGateway struct{ eng *engine.Engine }

// EngineGateway wraps an engine as the Gateway this package needs.
func EngineGateway(eng *engine.Engine) Gateway { return engineGateway{eng: eng} }

func (g engineGateway) SendMessage(ctx context.Context, p a2a.SendParams) (*a2a.SendResult, error) {
	return g.eng.SendMessage(ctx, p)
}

func (g engineGateway) GetRun(ctx context.Context, runID id.AgentRunID) (*run.Run, error) {
	return g.eng.GetRun(ctx, runID)
}

func (g engineGateway) ListRuns(ctx context.Context, filter *run.ListFilter) ([]*run.Run, error) {
	return g.eng.ListRuns(ctx, filter)
}

func (g engineGateway) CancelRun(ctx context.Context, runID id.AgentRunID) error {
	return g.eng.CancelRun(ctx, runID)
}

func (g engineGateway) GetAgentByName(ctx context.Context, name string) (*agent.Config, error) {
	return g.eng.GetAgentByName(ctx, name)
}

func (g engineGateway) GetSkillByName(ctx context.Context, name string) (*skill.Skill, error) {
	return g.eng.GetSkillByName(ctx, name)
}

func (g engineGateway) GetDelivery(ctx context.Context, deliveryID id.DeliveryID) (*a2a.Delivery, error) {
	return g.eng.Store().GetDelivery(ctx, deliveryID)
}

// AttachOptions is everything a host supplies to put an engine on the
// network.
type AttachOptions struct {
	// Resolver authenticates inbound callers. There is no default: a
	// service that authenticates nobody is an open door onto every agent
	// in the process.
	Resolver PeerResolver
	// Peers this engine may call out to. Trust is configuration, so an
	// agent's own output cannot add one.
	Peers []PeerConfig
	// Service tunes card serving and exposure.
	Service Options
	// Client tunes outbound calls.
	Client ClientOptions
}

// Attach wires an engine for remote A2A in both directions.
//
// It builds the inbound service, builds the outbound client over the
// configured peers, and registers that client as a transport on the
// engine's bus. Registration happens here rather than at bus
// construction because the two need each other: a peer's reply re-enters
// through the bus, and the bus routes outbound messages through the
// client.
func Attach(eng *engine.Engine, opts AttachOptions) (*Service, error) {
	if eng == nil {
		return nil, errors.New("cortex/a2aremote: no engine")
	}
	bus := eng.A2A()
	if bus == nil {
		return nil, ErrNoMessaging
	}
	if opts.Resolver == nil {
		return nil, errors.New("cortex/a2aremote: no peer resolver: inbound requests would be unauthenticated")
	}

	svc := NewService(EngineGateway(eng), opts.Resolver, opts.Service)
	if svc == nil {
		return nil, errors.New("cortex/a2aremote: the service could not be built")
	}

	if len(opts.Peers) > 0 {
		// The sink is Bus.Send, so a peer's answer travels the same path
		// a local agent's answer does and resolves a waiting ask the same
		// way. That convergence is what makes a resumed agent unable to
		// tell where its peer was running.
		sink := func(ctx context.Context, p a2a.SendParams) error {
			_, err := bus.Send(ctx, p)
			return err
		}
		bus.AddTransport(NewClient(opts.Peers, sink, opts.Client))
	}
	return svc, nil
}

// Handler serves everything an A2A peer needs from this engine: the RPC
// endpoint and the agent cards.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(WellKnownCardPath, s.CardHandler())
	mux.Handle("/agents/", s.CardHandler())
	mux.Handle("/", s.JSONRPCHandler())
	return mux
}
