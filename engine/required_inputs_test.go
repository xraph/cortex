package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/session"
	"github.com/xraph/cortex/store/sqlite"
	"github.com/xraph/cortex/verification"
)

func TestRequiredVerificationBindsExecutionInputs(t *testing.T) {
	f := newWorkerFixture(t)
	client := &requiredClient{}
	var captured verification.ExecutionInputs
	e, ag, overrides := requiredSetup(t, f, client, func(c *engine.RequiredVerificationConfig) {
		builder := c.Builder
		c.Builder = requiredBuilder(func(ctx context.Context, in engine.VerificationIntentInput) (verification.CaptureIntent, error) {
			captured = in.Inputs
			intent, err := builder.BuildVerificationIntent(ctx, in)
			// A host builder cannot mutate the history later sent to the model.
			in.Inputs.History[0].Content = "builder mutation"
			return intent, err
		})
	})
	sessionID := id.NewSessionID()
	overrides.SessionID = sessionID
	require.NoError(t, f.storage.CreateSession(f.ctx, &session.Session{ID: sessionID, AgentID: ag.ID}))
	require.NoError(t, f.storage.SaveConversation(f.ctx, ag.ID, sessionID, []memory.Message{{Role: "user", Content: "initial history"}}))
	client.before = func() {
		rows, err := f.storage.ListRuns(f.ctx, &run.ListFilter{AgentID: ag.ID.String()})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		intent, err := f.storage.GetRunCaptureIntent(f.ctx, rows[0].ID)
		require.NoError(t, err)
		require.NotNil(t, intent.Template.Binding.Context.ExecutionInputs)
		artifact, err := f.storage.LoadEvidence(f.ctx, *intent.Template.Binding.Context.ExecutionInputs)
		require.NoError(t, err)
		inputs, err := verification.DecodeExecutionInputs(artifact.Data)
		require.NoError(t, err)
		require.Equal(t, "actual task", inputs.Input)
		require.Equal(t, "initial history", inputs.History[0].Content)
		require.Equal(t, sessionID, inputs.SessionID)
		require.NoError(t, rows[0].ValidateExecutionInputs(inputs))
	}
	r, err := e.RunAgent(f.ctx, ag.Name, "actual task", overrides)
	require.NoError(t, err)
	require.Equal(t, "actual task", captured.Input)
	require.Equal(t, r.ID, captured.RunID)
	require.Equal(t, "model:1", captured.ModelRevision)
	require.Equal(t, verification.StatusPassed, r.VerificationStatus)
	require.Equal(t, "initial history", client.Request(t).Messages[0].Content)
}

func TestRequiredVerificationRejectsMissingOrAlteredInputEvidence(t *testing.T) {
	for _, mode := range []string{"missing", "altered", "denied"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			client := &requiredClient{}
			e, ag, overrides := requiredSetup(t, f, client, func(c *engine.RequiredVerificationConfig) {
				builder := c.Builder
				c.Builder = requiredBuilder(func(ctx context.Context, in engine.VerificationIntentInput) (verification.CaptureIntent, error) {
					if mode == "altered" {
						in.Inputs.Input = "another task"
					}
					intent, err := builder.BuildVerificationIntent(ctx, in)
					if err != nil {
						return intent, err
					}
					if mode == "missing" {
						intent.Template.Binding.Context.ExecutionInputs = nil
					}
					return verification.SealCaptureIntent(intent)
				})
				if mode == "denied" {
					c.CaptureAuthorizer = captureAuthorizer(func(_ context.Context, _ evidence.Action, ref evidence.Ref) error {
						if ref.MediaType == "application/json" {
							return cortex.ErrEvidenceAccessDenied
						}
						return nil
					})
				}
			})
			_, err := e.RunAgent(f.ctx, ag.Name, "work", overrides)
			require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
			require.Zero(t, client.calls)
			require.Zero(t, f.adapter.evaluations)
		})
	}
}

func TestRequiredVerificationResumeBindsInitialContext(t *testing.T) {
	for _, mode := range []string{"user message", "history boundary", "deleted inputs", "pending name"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			client := &requiredToolClient{}
			auth := &requiredToolAuthority{}
			e := requiredToolEngine(t, f, client, auth, engine.WithExternalTool(llm.Tool{Name: "probe"}))
			ag, overrides := requiredToolAgent(t, f)
			r, err := e.RunAgent(f.ctx, ag.Name, "work", overrides)
			require.NoError(t, err)
			s, err := f.storage.GetSuspension(f.ctx, r.ID)
			require.NoError(t, err)
			switch mode {
			case "user message":
				s.Cont.Messages[0].Content = "changed task"
			case "history boundary":
				s.Cont.NewMessagesFrom++
			case "pending name":
				s.Pending[0].Name = "unknown"
			case "deleted inputs":
				intent, readErr := f.storage.GetRunCaptureIntent(f.ctx, r.ID)
				require.NoError(t, readErr)
				require.NoError(t, f.storage.TombstoneEvidence(evidence.WithDeletionActor(f.ctx, "operator"), *intent.Template.Binding.Context.ExecutionInputs, "revoked"))
			}
			require.NoError(t, f.storage.DeleteSuspension(f.ctx, r.ID))
			require.NoError(t, f.storage.CreateSuspension(f.ctx, s))
			_, err = e.Resume(f.ctx, r.ID, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "SECRET-ID", Content: "result"}}})
			require.Error(t, err)
			saved, err := f.storage.GetRun(f.ctx, r.ID)
			require.NoError(t, err)
			require.Equal(t, r.Revision, saved.Revision)
			require.Equal(t, run.StatePaused, saved.State)
			require.Equal(t, 1, client.calls)
		})
	}
}

func TestRequiredVerificationRequiresRevisionPins(t *testing.T) {
	for _, mode := range []string{"model", "tool"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			client := &requiredToolClient{}
			auth := &requiredToolAuthority{}
			cfg := requiredConfiguration(t, f)
			cfg.HandoffAuthorizer = auth
			opts := []engine.Option{engine.WithStore(f.storage), engine.WithLLM(client), engine.WithVerificationWorker(f.config), engine.WithRequiredVerification(cfg), engine.WithToolAuthorizer(auth), engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[string]()), engine.WithExternalTool(llm.Tool{Name: "probe"})}
			if mode != "model" {
				opts = append(opts, engine.WithModelRevision("smart", "1"))
			}
			if mode != "tool" {
				opts = append(opts, engine.WithToolRevision("probe", "1"))
			}
			e, err := engine.New(opts...)
			require.NoError(t, err)
			ag, overrides := requiredToolAgent(t, f)
			_, err = e.PrepareAgent(f.ctx, ag.ID, overrides)
			require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
			require.Zero(t, client.calls)
		})
	}
}

type requiredHistoryFailure struct{ *sqlite.Store }

func (*requiredHistoryFailure) LoadConversation(context.Context, id.AgentID, id.SessionID, int) ([]memory.Message, error) {
	return nil, errors.New("private history failure")
}

func TestRequiredVerificationHistoryFailurePreventsGeneration(t *testing.T) {
	f := newWorkerFixture(t)
	client := &requiredClient{}
	e, err := engine.New(engine.WithStore(&requiredHistoryFailure{Store: f.storage}), engine.WithLLM(client), engine.WithModelRevision("smart", "1"), engine.WithVerificationWorker(f.config), engine.WithRequiredVerification(requiredConfiguration(t, f)))
	require.NoError(t, err)
	ag, overrides := requiredToolAgent(t, f)
	ag.Tools = nil
	require.NoError(t, f.storage.Update(f.ctx, ag))
	_, err = e.RunAgent(f.ctx, ag.Name, "work", overrides)
	require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
	require.NotContains(t, err.Error(), "private history failure")
	require.Zero(t, client.calls)
	rows, err := f.storage.ListRuns(f.ctx, &run.ListFilter{AgentID: ag.ID.String()})
	require.NoError(t, err)
	require.Empty(t, rows)
}

type requiredInputScanner struct{}

func (requiredInputScanner) ScanInput(_ context.Context, req *safety.ScanRequest) (*safety.ScanResult, error) {
	if strings.Contains(req.Content, "secret") {
		return &safety.ScanResult{Decision: safety.DecisionRedact, Redacted: strings.ReplaceAll(req.Content, "secret", "safe")}, nil
	}
	return &safety.ScanResult{Decision: safety.DecisionAllow}, nil
}
func (requiredInputScanner) ScanOutput(context.Context, *safety.ScanRequest) (*safety.ScanResult, error) {
	return &safety.ScanResult{Decision: safety.DecisionAllow}, nil
}

func TestRequiredVerificationCapturesScannedInputs(t *testing.T) {
	f := newWorkerFixture(t)
	client := &requiredToolClient{final: true}
	auth := &requiredToolAuthority{}
	e := requiredToolEngine(t, f, client, auth, engine.WithExternalTool(llm.Tool{Name: "probe"}), engine.WithSafety(requiredInputScanner{}))
	ag, overrides := requiredToolAgent(t, f)
	sessionID := id.NewSessionID()
	overrides.SessionID = sessionID
	require.NoError(t, f.storage.CreateSession(f.ctx, &session.Session{ID: sessionID, AgentID: ag.ID}))
	require.NoError(t, f.storage.SaveConversation(f.ctx, ag.ID, sessionID, []memory.Message{{Role: "user", Content: "secret history"}}))
	r, err := e.RunAgent(f.ctx, ag.Name, "secret task", overrides)
	require.NoError(t, err)
	intent, err := f.storage.GetRunCaptureIntent(f.ctx, r.ID)
	require.NoError(t, err)
	artifact, err := f.storage.LoadEvidence(f.ctx, *intent.Template.Binding.Context.ExecutionInputs)
	require.NoError(t, err)
	inputs, err := verification.DecodeExecutionInputs(artifact.Data)
	require.NoError(t, err)
	require.Equal(t, "safe task", inputs.Input)
	require.Equal(t, "safe history", inputs.History[0].Content)
	require.NotContains(t, string(artifact.Data), "secret")
}
