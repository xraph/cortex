package orchestration

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
)

// ReservedInvocation identifies a child before execution. Execute must start
// that exact run at most once. An unexecuted reservation retains its allocation
// until the host can prove that releasing it is safe.
type ReservedInvocation = cortex.ReservedInvocation

// ReservedAgentRunner reserves bounded capacity and an identity without calling
// a provider or executing agent tools. Return the invocation alongside an error
// when its identity needs reconciliation. The service persists that identity
// before execution and never executes an invocation returned with an error.
type ReservedAgentRunner = cortex.ReservedAgentRunner

// NewReservedService builds a service that records child identities before
// execution. Use RunConfig or RunConfigWithID with a captured configuration.
func NewReservedService(runner ReservedAgentRunner, runs RunStore, hooks HookEmitter) *Service {
	s := NewService(nil, nil, runs, hooks)
	s.reserved = runner
	return s
}

// RunConfigWithID executes a fresh team under a host-selected budget account ID.
// The run store must reject duplicate IDs, including concurrent creation. This
// method cannot resume or restart an existing team.
func (s *Service) RunConfigWithID(ctx context.Context, cfg Config, input string, team id.OrchestrationID) (*Run, error) {
	if hostport.IsNil(s.runs) {
		return nil, cortex.ErrNoStore
	}
	if team.IsNil() || team.Prefix() != id.PrefixOrchestration || (hostport.IsNil(s.runner) && hostport.IsNil(s.reserved)) {
		return nil, cortex.ErrRunIncomplete
	}
	return s.runConfig(ctx, cfg, input, team)
}
