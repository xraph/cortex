package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
)

// defaultsLLM answers every call with a final message, so a run ends on
// its first step.
type defaultsLLM struct{}

func (defaultsLLM) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	return &llm.Response{Content: "done", FinishReason: "stop"}, nil
}

func (defaultsLLM) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, errors.New("defaultsLLM: CompleteStream not supported")
}

// TestDefaultEngineEnablesNoExperimentalServices pins today's defaults:
// an engine built with only a store and a model turns on no experimental
// service. If this fails, a default flipped. Ask before changing it.
func TestDefaultEngineEnablesNoExperimentalServices(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), approvalScope())
	st := newApprovalStore(ctx, t)
	e, err := New(WithStore(st), WithLLM(defaultsLLM{}))
	require.NoError(t, err)

	// PublicationMode is a setting, not a switch: an empty policy resolves
	// to buffered, as it did in v1.9.0, and with no gate installed nothing
	// is checked. Checked segments, the experimental mode, must stay off.
	caps := e.RuntimeCapabilities()
	require.Equal(t, RuntimeCapabilities{PreparedExecution: true, PublicationMode: safety.ModeBuffered}, caps,
		"only PreparedExecution may be on by default")

	gates := map[string]bool{
		"verification worker":         e.verificationConfig != nil || e.verificationWorker != nil,
		"verification recovery":       e.verificationRecovery != nil,
		"required verification":       e.requiredVerification != nil,
		"context budget":              e.contextBudget != nil,
		"result content":              e.resultContentConfig != nil || e.resultContentReader != nil,
		"public event replay":         e.publicEventConfigured || e.publicEventRecording || e.publicEventReader != nil || e.publicEventRecorder != nil,
		"prepared-team messaging":     e.preparedMessaging != nil,
		"task scheduling":             e.taskScheduling != nil,
		"claim recovery":              e.claimRecovery != nil,
		"provider recovery":           e.providerRecovery != nil,
		"task adoption":               e.taskAdoption != nil,
		"task settlement":             e.taskSettlement != nil,
		"task retirement":             e.taskRetirement != nil,
		"task completion":             e.taskCompletion != nil,
		"plan replanning":             e.planReplanning != nil,
		"coordination events":         e.coordinationEvents != nil,
		"artifact publication":        e.artifactPublication != nil,
		"agent-reply recovery":        e.agentReplyRecovery != nil,
		"orchestration recovery":      e.orchestrationRecovery != nil,
		"orchestration expiry":        e.orchestrationExpiry != nil,
		"effect continuation":         e.effectContinuation != nil,
		"effect recovery":             e.effectRecovery != nil,
		"reflection":                  e.reflection != nil,
		"operational awareness":       e.operationalAwareness != nil,
		"a2a messaging":               e.a2a != nil,
		"background lifecycle helper": len(e.taskServices()) > 0,
	}
	for name, on := range gates {
		require.Falsef(t, on, "%s is configured on a default engine", name)
	}

	require.NoError(t, e.Start(ctx))
	require.Empty(t, e.taskServices(), "Start configured a background service")
	require.Nil(t, e.a2a, "Start configured messaging")
	e.sweep.mu.Lock()
	sweeping := e.sweep.cancel != nil
	e.sweep.mu.Unlock()
	require.True(t, sweeping, "Start did not start the sweeper")

	r, err := e.RunAgent(ctx, "assistant", "hello", nil)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, r.State)
	require.False(t, r.Provenance.VerificationRequired, "a default run required verification")

	require.NoError(t, e.Stop(ctx))
	e.sweep.mu.Lock()
	sweeping = e.sweep.cancel != nil
	e.sweep.mu.Unlock()
	require.False(t, sweeping, "Stop left the sweeper running")
	require.Empty(t, e.taskServices(), "Stop left a background service configured")
}
