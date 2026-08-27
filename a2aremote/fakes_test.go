package a2aremote

import (
	"context"
	"errors"
	"sync"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/skill"
)

func testScope() cortex.Scope {
	return cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "resolved"}}}
}

// fakeGateway records the context and params it was called with, which
// is how the scope tests see what actually reached cortex.
type fakeGateway struct {
	mu         sync.Mutex
	lastCtx    context.Context //nolint:containedctx // the test asserts on the scope it carried
	lastParams a2a.SendParams
	sendCalls  int

	agents     map[string]*agent.Config
	runs       map[string]*run.Run
	skills     map[string]*skill.Skill
	deliveries map[string]*a2a.Delivery

	sendResult *a2a.SendResult
	sendErr    error
	cancelErr  error
}

func newFakeGateway() *fakeGateway {
	return &fakeGateway{
		agents:     map[string]*agent.Config{"worker": {ID: id.NewAgentID(), Name: "worker"}},
		runs:       map[string]*run.Run{},
		skills:     map[string]*skill.Skill{},
		deliveries: map[string]*a2a.Delivery{},
	}
}

func (g *fakeGateway) SendMessage(ctx context.Context, p a2a.SendParams) (*a2a.SendResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastCtx, g.lastParams = ctx, p
	g.sendCalls++
	if g.sendErr != nil {
		return nil, g.sendErr
	}
	if g.sendResult != nil {
		return g.sendResult, nil
	}
	return &a2a.SendResult{MessageID: id.NewMessageID(), ConversationID: id.NewConversationID()}, nil
}

func (g *fakeGateway) GetRun(_ context.Context, runID id.AgentRunID) (*run.Run, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, ok := g.runs[runID.String()]
	if !ok {
		return nil, errors.New("run not found")
	}
	return r, nil
}

func (g *fakeGateway) GetDelivery(_ context.Context, deliveryID id.DeliveryID) (*a2a.Delivery, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	d, ok := g.deliveries[deliveryID.String()]
	if !ok {
		return nil, errors.New("delivery not found")
	}
	return d, nil
}

func (g *fakeGateway) addDelivery(d *a2a.Delivery) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deliveries[d.ID.String()] = d
}

func (g *fakeGateway) ListRuns(_ context.Context, _ *run.ListFilter) ([]*run.Run, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*run.Run, 0, len(g.runs))
	for _, r := range g.runs {
		out = append(out, r)
	}
	return out, nil
}

func (g *fakeGateway) CancelRun(_ context.Context, _ id.AgentRunID) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cancelErr
}

func (g *fakeGateway) GetAgentByName(_ context.Context, name string) (*agent.Config, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	a, ok := g.agents[name]
	if !ok {
		return nil, errors.New("agent not found")
	}
	return a, nil
}

func (g *fakeGateway) GetSkillByName(_ context.Context, name string) (*skill.Skill, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s, ok := g.skills[name]
	if !ok {
		return nil, errors.New("skill not found")
	}
	return s, nil
}

func (g *fakeGateway) calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sendCalls
}

func (g *fakeGateway) params() a2a.SendParams {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastParams
}

func (g *fakeGateway) scopeSeen() cortex.Scope {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lastCtx == nil {
		return cortex.Scope{}
	}
	return cortex.ScopeFromContext(g.lastCtx)
}

func (g *fakeGateway) addRun(r *run.Run) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.runs[r.ID.String()] = r
}

// staticResolver answers with one peer, or refuses.
type staticResolver struct {
	peer Peer
	err  error
}

func (s staticResolver) ResolvePeer(context.Context, Credentials) (Peer, error) {
	return s.peer, s.err
}

func testService(t interface{ Helper() }, gw Gateway, res PeerResolver) *Service {
	t.Helper()
	return NewService(gw, res, Options{})
}
