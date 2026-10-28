package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

type capabilityAuthority struct{ hideProbe, denied bool }

func (a *capabilityAuthority) Visible(_ context.Context, _ cortex.Subject, tools []llm.Tool) []llm.Tool {
	var out []llm.Tool
	for _, tool := range tools {
		if tool.Name != "secret" && (!a.hideProbe || tool.Name != "probe") {
			out = append(out, tool)
		}
	}
	return out
}

func (a *capabilityAuthority) Authorize(context.Context, cortex.Subject, llm.ToolCall) error {
	if a.denied {
		return errors.New("denied")
	}
	return nil
}

type capabilityClient struct {
	requiredToolClient
	before func()
}

func (c *capabilityClient) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	if c.before != nil {
		c.before()
	}
	r, err := c.requiredToolClient.Complete(ctx, req)
	if r != nil && len(r.ToolCalls) > 0 {
		r.ToolCalls[0].Arguments = `{}`
	}
	return r, err
}

func (c *capabilityClient) CompleteStream(ctx context.Context, req *llm.Request) (llm.Stream, error) {
	r, err := c.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	return &requiredStream{chunks: []llm.Chunk{{Content: r.Content, ToolCalls: r.ToolCalls, FinishReason: r.FinishReason}}}, nil
}

func capabilityResult(t *testing.T, req *llm.Request) cognitive.Snapshot {
	t.Helper()
	for _, msg := range req.Messages {
		if msg.Role == "tool" {
			var snap cognitive.Snapshot
			require.NoError(t, json.Unmarshal([]byte(msg.Content), &snap), msg.Content)
			require.Equal(t, 1, snap.SchemaVersion, msg.Content)
			return snap
		}
	}
	t.Fatal("missing operational snapshot")
	return cognitive.Snapshot{}
}

func TestOperationalCapabilityToolUsesCurrentRuntime(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "stream"}[stream], func(t *testing.T) {
			ctx := overlayCtx("capabilities")
			s := newSQLiteStoreForTest(t)
			client := &capabilityClient{requiredToolClient: requiredToolClient{name: engine.ToolOperationalCapabilities}}
			auth := &capabilityAuthority{}
			e, err := engine.New(engine.WithStore(s), engine.WithLLM(client), engine.WithToolAuthorizer(auth), engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[string]()), engine.WithOperationalAwareness(), engine.WithTool(namedTool("probe"), noopHandler), engine.WithTool(namedTool("secret"), noopHandler), engine.WithExternalTool(namedTool("remote")))
			require.NoError(t, err)
			ag := &agent.Config{ID: id.NewAgentID(), Name: "capabilities", SystemPrompt: "I can fly and always know the answer."}
			require.NoError(t, s.Create(ctx, ag))
			o := &engine.RunOverrides{Budget: &execution.Budget{MaxSteps: 3, MaxTokens: 1000, MaxToolCalls: 2, Deadline: time.Now().Add(time.Minute)}}
			if stream {
				events := make(chan engine.StreamEvent, 32)
				require.NoError(t, e.StreamAgent(ctx, ag.Name, "inspect", o, events))
				var done bool
				for event := range events {
					done = done || event.Type == engine.EventDone
				}
				require.True(t, done)
			} else {
				r, runErr := e.RunAgent(ctx, ag.Name, "inspect", o)
				require.NoError(t, runErr)
				require.Equal(t, run.StateCompleted, r.State)
			}
			snap := capabilityResult(t, client.last)
			require.Len(t, snap.Tools, 3)
			require.Equal(t, []string{engine.ToolOperationalCapabilities, "probe", "remote"}, []string{snap.Tools[0].Name, snap.Tools[1].Name, snap.Tools[2].Name})
			require.Equal(t, cognitive.ToolExternal, snap.Tools[2].Kind)
			require.Equal(t, "unknown", snap.Reliability)
			require.Equal(t, 2, *snap.Remaining.Steps)
			require.Equal(t, 995, *snap.Remaining.Tokens)
			require.Equal(t, 1, *snap.Remaining.ToolCalls)
			for _, tool := range snap.Tools {
				require.Equal(t, cognitive.HealthUnknown, tool.Health)
			}
		})
	}
}

func TestRevokedGrantRemovesCapability(t *testing.T) {
	ctx := overlayCtx("revoked-capability")
	s := newSQLiteStoreForTest(t)
	auth := &capabilityAuthority{}
	client := &capabilityClient{requiredToolClient: requiredToolClient{name: engine.ToolOperationalCapabilities}, before: func() { auth.hideProbe = true }}
	e, err := engine.New(engine.WithStore(s), engine.WithLLM(client), engine.WithToolAuthorizer(auth), engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[string]()), engine.WithOperationalAwareness(), engine.WithTool(namedTool("probe"), noopHandler))
	require.NoError(t, err)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "capabilities"}
	require.NoError(t, s.Create(ctx, ag))
	_, err = e.RunAgent(ctx, ag.Name, "inspect", nil)
	require.NoError(t, err)
	snap := capabilityResult(t, client.last)
	require.Len(t, snap.Tools, 1)
	require.Equal(t, engine.ToolOperationalCapabilities, snap.Tools[0].Name)
}

func TestOperationalCapabilityToolCannotBypassAuthorization(t *testing.T) {
	ctx := overlayCtx("denied-capability")
	s := newSQLiteStoreForTest(t)
	client := &capabilityClient{requiredToolClient: requiredToolClient{name: engine.ToolOperationalCapabilities}}
	e, err := engine.New(engine.WithStore(s), engine.WithLLM(client), engine.WithToolAuthorizer(&capabilityAuthority{denied: true}), engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[string]()), engine.WithOperationalAwareness())
	require.NoError(t, err)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "capabilities"}
	require.NoError(t, s.Create(ctx, ag))
	_, err = e.RunAgent(ctx, ag.Name, "inspect", nil)
	require.NoError(t, err)
	encoded, err := json.Marshal(client.last.Messages)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "denied")
	require.NotContains(t, string(encoded), "schema_version")
}

func TestOperationalCapabilityToolUsesVerifiedTranscriptAndApproval(t *testing.T) {
	for _, mode := range []string{"sync", "stream", "approval_restart"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			client := &capabilityClient{requiredToolClient: requiredToolClient{name: engine.ToolOperationalCapabilities}}
			auth := &requiredToolAuthority{approval: mode == "approval_restart"}
			e := requiredToolEngine(t, f, client, auth, engine.WithOperationalAwareness())
			ag, overrides := requiredToolAgent(t, f)
			overrides.Tools = []string{engine.ToolOperationalCapabilities}
			var r *run.Run
			if mode == "stream" {
				events := make(chan engine.StreamEvent, 32)
				require.NoError(t, e.StreamAgent(f.ctx, ag.Name, "inspect", overrides, events))
				for event := range events {
					encoded, err := json.Marshal(event)
					require.NoError(t, err)
					require.NotContains(t, string(encoded), "provider_health")
				}
				rows, err := f.storage.ListRuns(f.ctx, &run.ListFilter{AgentID: ag.ID.String()})
				require.NoError(t, err)
				require.Len(t, rows, 1)
				r = rows[0]
			} else {
				var err error
				r, err = e.RunAgent(f.ctx, ag.Name, "inspect", overrides)
				require.NoError(t, err)
			}
			if mode == "approval_restart" {
				require.Equal(t, run.StatePaused, r.State)
				client = &capabilityClient{requiredToolClient: requiredToolClient{final: true}}
				fresh := requiredToolEngine(t, f, client, auth, engine.WithOperationalAwareness())
				pending, err := fresh.ListPendingCheckpoints(f.ctx, nil)
				require.NoError(t, err)
				require.Len(t, pending, 1)
				require.NoError(t, fresh.ResolveCheckpoint(f.ctx, pending[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "operator"}))
				r, err = f.storage.GetRun(f.ctx, r.ID)
				require.NoError(t, err)
			}
			require.Equal(t, verification.StatusPassed, r.VerificationStatus)
			snap := capabilityResult(t, client.last)
			require.Equal(t, 3, *snap.Remaining.Steps)
			require.Equal(t, 1, snap.Usage.ToolCalls)
			intent, err := f.storage.GetRunCaptureIntent(f.ctx, r.ID)
			require.NoError(t, err)
			job, err := f.storage.GetVerificationJob(f.ctx, intent.Template.ID)
			require.NoError(t, err)
			require.NotNil(t, job.Request.Binding.Context.ExecutionTranscript)
			artifact, err := f.storage.LoadEvidence(f.ctx, *job.Request.Binding.Context.ExecutionTranscript)
			require.NoError(t, err)
			trace, err := verification.DecodeExecutionTranscript(artifact.Data)
			require.NoError(t, err)
			require.Equal(t, verification.ToolCompleted, trace.Tools[0].Outcome)
			require.Contains(t, trace.Messages[1].Content, "provider_health")
			assertRequiredToolPrivacy(t, f, ag)
		})
	}
}
