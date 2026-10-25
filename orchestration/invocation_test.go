package orchestration

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

func TestStrategyInvocationSlots(t *testing.T) {
	for _, strategy := range []string{StrategySequential, StrategyParallel, StrategyRouter, StrategyHierarchical, StrategyDebate} {
		t.Run(strategy, func(t *testing.T) {
			var previous map[string]string
			for range 2 {
				slots := map[string]string{}
				var mu sync.Mutex
				runner := hierarchyRunnerFunc(func(ctx context.Context, name, _ string, _ *RunOpts) (*AgentResult, error) {
					slot := InvocationSlot(ctx)
					mu.Lock()
					defer mu.Unlock()
					require.NotEmpty(t, slot)
					require.NotContains(t, slots, slot)
					slots[slot] = name
					output := "answer"
					if strategy == StrategyHierarchical && name == "a" && len(slots) == 1 {
						output = `[{"agent":"b","task":"work"}]`
					}
					if strategy == StrategyRouter {
						output = "b"
					}
					return &AgentResult{AgentName: name, Output: output}, nil
				})
				cfg := reservedConfig(strategy)
				cfg.Settings.Rounds = 2
				cfg.Settings.RouterAgent = "a"
				cfg.Settings.Aggregator = "a"
				cfg.Settings.Judge = "a"
				o, err := Build(strategy, runner, cfg.Participants, cfg.Settings)
				require.NoError(t, err)
				_, err = o.Run(context.Background(), "work", NewBlackboard(id.NewOrchestrationID(), cfg.Participants, nil))
				require.NoError(t, err)
				if previous != nil {
					require.Equal(t, previous, slots)
				}
				previous = slots
			}
		})
	}
}

type invocationTestStore struct {
	executionRunStore
	intents             map[string]InvocationIntent
	bindings            map[string]id.AgentRunID
	failBegin, failBind error
}

func (s *invocationTestStore) BeginOrchestrationInvocation(_ context.Context, in InvocationIntent) error {
	if _, ok := s.intents[in.Slot]; ok {
		return cortex.ErrAlreadyExists
	}
	s.intents[in.Slot] = in
	return s.failBegin
}
func (s *invocationTestStore) BindOrchestrationInvocation(_ context.Context, in InvocationIntent, child id.AgentRunID) error {
	s.bindings[in.Slot] = child
	return s.failBind
}
func (s *invocationTestStore) GetOrchestrationInvocation(_ context.Context, _ id.OrchestrationID, slot string) (InvocationRecord, error) {
	return InvocationRecord{Intent: s.intents[slot], RunID: s.bindings[slot]}, nil
}

func TestReservedInvocationJournalBeforeDispatch(t *testing.T) {
	for _, mode := range []string{"success", "lost_intent_ack", "lost_binding_ack", "existing_intent"} {
		t.Run(mode, func(t *testing.T) {
			failure := errors.New("write acknowledgment lost")
			s := &invocationTestStore{intents: map[string]InvocationIntent{}, bindings: map[string]id.AgentRunID{}}
			if mode == "lost_intent_ack" {
				s.failBegin = failure
			}
			if mode == "lost_binding_ack" {
				s.failBind = failure
			}
			if mode == "existing_intent" {
				s.intents["sequential/0"] = InvocationIntent{}
				failure = cortex.ErrAlreadyExists
			}
			reserved, executed := 0, 0
			runner := reservingRunnerFunc(func(ctx context.Context, _ id.OrchestrationID, name, _ string, _ *RunOpts) (*ReservedInvocation, error) {
				reserved++
				slot := InvocationSlot(ctx)
				require.Contains(t, s.intents, slot)
				child := id.NewAgentRunID()
				return &ReservedInvocation{RunID: child, Execute: func(context.Context) (*AgentResult, error) {
					executed++
					require.Equal(t, child, s.bindings[slot])
					return &AgentResult{RunID: child, AgentName: name, Output: "answer"}, nil
				}}, nil
			})
			ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "journal"}}})
			_, err := NewReservedService(runner, s, nil).RunConfig(ctx, reservedConfig(StrategySequential), "work")
			if mode == "success" {
				require.NoError(t, err)
				require.Equal(t, 2, executed)
			} else {
				require.ErrorIs(t, err, failure)
				require.Zero(t, executed)
			}
			if mode == "lost_intent_ack" || mode == "existing_intent" {
				require.Zero(t, reserved)
			}
		})
	}
}

func TestHierarchicalRepairHasDistinctInvocation(t *testing.T) {
	slots := []string{}
	runner := hierarchyRunnerFunc(func(ctx context.Context, name, _ string, _ *RunOpts) (*AgentResult, error) {
		slots = append(slots, InvocationSlot(ctx))
		output := "answer"
		if len(slots) == 1 {
			output = "invalid plan"
		}
		if len(slots) == 2 {
			output = `[{"agent":"b","task":"work"}]`
		}
		return &AgentResult{AgentName: name, Output: output}, nil
	})
	cfg := reservedConfig(StrategyHierarchical)
	cfg.Settings.PlanRepairAttempts = 1
	o, err := Build(cfg.Strategy, runner, cfg.Participants, cfg.Settings)
	require.NoError(t, err)
	_, err = o.Run(t.Context(), "work", NewBlackboard(id.NewOrchestrationID(), cfg.Participants, nil))
	require.NoError(t, err)
	require.Equal(t, []string{"hierarchical/plan/0", "hierarchical/plan/1", "hierarchical/worker/0", "hierarchical/synthesis"}, slots)
}
