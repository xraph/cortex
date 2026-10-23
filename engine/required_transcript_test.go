package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/verification"
)

func TestRequiredTranscriptIncludesLocalAndResumedResults(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "resumed"}[external], func(t *testing.T) {
			f := newWorkerFixture(t)
			client := &requiredToolClient{}
			authority := &requiredToolAuthority{}
			option := engine.WithTool(llm.Tool{Name: "probe"}, func(context.Context, cortex.Invocation) (string, error) { return "SECRET result", nil })
			if external {
				option = engine.WithExternalTool(llm.Tool{Name: "probe"})
			}
			e := requiredToolEngine(t, f, client, authority, option)
			ag, overrides := requiredToolAgent(t, f)
			r, err := e.RunAgent(f.ctx, ag.Name, "work", overrides)
			require.NoError(t, err)
			if external {
				r, err = e.Resume(f.ctx, r.ID, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "SECRET-ID", Content: "SECRET result"}}})
				require.NoError(t, err)
			}
			intent, err := f.storage.GetRunCaptureIntent(f.ctx, r.ID)
			require.NoError(t, err)
			job, err := f.storage.GetVerificationJob(f.ctx, intent.Template.ID)
			require.NoError(t, err)
			require.NotNil(t, job.Request.Binding.Context.ExecutionTranscript)
			artifact, err := f.storage.LoadEvidence(f.ctx, *job.Request.Binding.Context.ExecutionTranscript)
			require.NoError(t, err)
			trace, err := verification.DecodeExecutionTranscript(artifact.Data)
			require.NoError(t, err)
			require.Len(t, trace.Messages, 3)
			require.Equal(t, "SECRET result", trace.Messages[1].Content)
			require.Equal(t, verification.ToolCompleted, trace.Tools[0].Outcome)
			source := verification.ToolLocal
			if external {
				source = verification.ToolExternal
			}
			require.Equal(t, source, trace.Tools[0].Source)
			require.Equal(t, r.Usage, trace.Usage)
			assertRequiredToolPrivacy(t, f, ag)
		})
	}
}

type changingTranscriptScanner struct{ changed bool }

func (s *changingTranscriptScanner) ScanInput(_ context.Context, req *safety.ScanRequest) (*safety.ScanResult, error) {
	if s.changed && req.Content == "work" {
		return &safety.ScanResult{Decision: safety.DecisionRedact, Redacted: "different task"}, nil
	}
	return &safety.ScanResult{Decision: safety.DecisionAllow}, nil
}
func (*changingTranscriptScanner) ScanOutput(context.Context, *safety.ScanRequest) (*safety.ScanResult, error) {
	return &safety.ScanResult{Decision: safety.DecisionAllow}, nil
}

func TestRequiredTranscriptRejectsResumeRewritingRetainedInputs(t *testing.T) {
	f := newWorkerFixture(t)
	client := &requiredToolClient{}
	scanner := &changingTranscriptScanner{}
	e := requiredToolEngine(t, f, client, &requiredToolAuthority{}, engine.WithExternalTool(llm.Tool{Name: "probe"}), engine.WithSafety(scanner))
	ag, overrides := requiredToolAgent(t, f)
	r, err := e.RunAgent(f.ctx, ag.Name, "work", overrides)
	require.NoError(t, err)
	scanner.changed = true
	_, err = e.Resume(f.ctx, r.ID, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "SECRET-ID", Content: "SECRET result"}}})
	require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
	require.Equal(t, 1, client.calls)
	require.Zero(t, f.adapter.evaluations)
}

func TestRequiredTranscriptRecordsOutcomes(t *testing.T) {
	for _, mode := range []string{"failed", "denied", "unknown", "external failure", "approval", "stream"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			client := &requiredToolClient{}
			auth := &requiredToolAuthority{denied: mode == "denied", approval: mode == "approval"}
			executed := 0
			option := engine.WithTool(llm.Tool{Name: "probe"}, func(context.Context, cortex.Invocation) (string, error) {
				executed++
				if mode == "failed" {
					return "", errors.New("SECRET failure")
				}
				return "SECRET result", nil
			})
			if mode == "external failure" {
				option = engine.WithExternalTool(llm.Tool{Name: "probe"})
			}
			if mode == "unknown" {
				client.name = "unknown"
			}
			e := requiredToolEngine(t, f, client, auth, option)
			ag, overrides := requiredToolAgent(t, f)
			var r *run.Run
			var err error
			if mode == "stream" {
				events := make(chan engine.StreamEvent, 20)
				require.NoError(t, e.StreamAgent(f.ctx, ag.Name, "work", overrides, events))
				for event := range events {
					require.NotEqual(t, engine.EventError, event.Type)
				}
				rows, listErr := f.storage.ListRuns(f.ctx, &run.ListFilter{AgentID: ag.ID.String()})
				require.NoError(t, listErr)
				require.Len(t, rows, 1)
				r = rows[0]
			} else {
				r, err = e.RunAgent(f.ctx, ag.Name, "work", overrides)
				require.NoError(t, err)
			}
			if mode == "external failure" {
				r, err = e.Resume(f.ctx, r.ID, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "SECRET-ID", Error: "SECRET external failure"}}})
				require.NoError(t, err)
			}
			if mode == "approval" {
				pending, readErr := e.ListPendingCheckpoints(f.ctx, nil)
				require.NoError(t, readErr)
				require.Len(t, pending, 1)
				require.NoError(t, e.ResolveCheckpoint(f.ctx, pending[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "operator"}))
				r, err = f.storage.GetRun(f.ctx, r.ID)
				require.NoError(t, err)
				require.Equal(t, 1, executed)
			}
			intent, err := f.storage.GetRunCaptureIntent(f.ctx, r.ID)
			require.NoError(t, err)
			job, err := f.storage.GetVerificationJob(f.ctx, intent.Template.ID)
			require.NoError(t, err)
			artifact, err := f.storage.LoadEvidence(f.ctx, *job.Request.Binding.Context.ExecutionTranscript)
			require.NoError(t, err)
			trace, err := verification.DecodeExecutionTranscript(artifact.Data)
			require.NoError(t, err)
			require.Len(t, trace.Tools, 1)
			want := verification.ToolCompleted
			if mode == "failed" || mode == "external failure" {
				want = verification.ToolFailed
			}
			if mode == "denied" || mode == "unknown" {
				want = verification.ToolDenied
				require.Zero(t, executed)
			}
			require.Equal(t, want, trace.Tools[0].Outcome)
			source := verification.ToolLocal
			if mode == "external failure" {
				source = verification.ToolExternal
			}
			require.Equal(t, source, trace.Tools[0].Source)
			require.Equal(t, r.Usage, trace.Usage)
			if mode == "unknown" {
				require.Zero(t, trace.Usage.ToolCalls)
			}
			assertRequiredToolPrivacy(t, f, ag)
		})
	}
}

func TestRequiredTranscriptReservationAndCaptureAuthorization(t *testing.T) {
	for _, mode := range []string{"missing", "too large", "small limit", "deny reservation", "deny capture"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			client := &requiredClient{}
			e, ag, overrides := requiredSetup(t, f, client, func(cfg *engine.RequiredVerificationConfig) {
				builder := cfg.Builder
				var traceID id.ID
				cfg.Builder = requiredBuilder(func(ctx context.Context, in engine.VerificationIntentInput) (verification.CaptureIntent, error) {
					intent, err := builder.BuildVerificationIntent(ctx, in)
					if err != nil {
						return intent, err
					}
					traceID = intent.Transcript.ID
					switch mode {
					case "missing":
						intent.SchemaVersion = 1
						intent.Transcript = nil
						intent.MaxTranscriptBytes = 0
					case "too large":
						intent.MaxTranscriptBytes = 1<<20 + 1
					case "small limit":
						intent.MaxTranscriptBytes = 1
					}
					return verification.SealCaptureIntent(intent)
				})
				cfg.CaptureAuthorizer = captureAuthorizer(func(_ context.Context, _ evidence.Action, ref evidence.Ref) error {
					if ref.ID == traceID && (mode == "deny reservation" || (mode == "deny capture" && ref.Bytes > 0)) {
						return errors.New("SECRET denied")
					}
					return nil
				})
			})
			r, err := e.RunAgent(f.ctx, ag.Name, "work", overrides)
			require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
			if r != nil {
				require.Empty(t, r.Output)
			}
			require.Zero(t, f.adapter.evaluations)
			if mode == "missing" || mode == "too large" || mode == "deny reservation" {
				require.Zero(t, client.calls)
			}
		})
	}
}

type mixedTranscriptClient struct{ requiredToolClient }

func (c *mixedTranscriptClient) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	response, err := c.requiredToolClient.Complete(ctx, req)
	if err == nil && c.calls == 1 {
		response.ToolCalls = append(response.ToolCalls, llm.ToolCall{ID: "external-call", Name: "wait", Arguments: `{}`})
	}
	return response, err
}

func TestRequiredTranscriptRetainsCompletedOutcomesAcrossReopen(t *testing.T) {
	f := newWorkerFixture(t)
	client := &mixedTranscriptClient{}
	auth := &requiredToolAuthority{}
	options := []engine.Option{engine.WithTool(llm.Tool{Name: "probe"}, func(context.Context, cortex.Invocation) (string, error) { return "SECRET local", nil }), engine.WithExternalTool(llm.Tool{Name: "wait"}), engine.WithToolRevision("wait", "1")}
	e := requiredToolEngine(t, f, client, auth, options...)
	ag, overrides := requiredToolAgent(t, f)
	ag.Tools = append(ag.Tools, "wait")
	require.NoError(t, f.storage.Update(f.ctx, ag))
	r, err := e.RunAgent(f.ctx, ag.Name, "work", overrides)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, r.State)
	paused, err := f.storage.GetSuspension(f.ctx, r.ID)
	require.NoError(t, err)
	require.Len(t, paused.Cont.ToolResults, 1)
	require.NoError(t, f.storage.Close())
	f.storage = openSQLiteStoreForTest(t, f.path)
	e = requiredToolEngine(t, f, &requiredToolClient{final: true}, auth, options...)
	r, err = e.Resume(f.ctx, r.ID, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "external-call", Content: "SECRET external"}}})
	require.NoError(t, err)
	intent, err := f.storage.GetRunCaptureIntent(f.ctx, r.ID)
	require.NoError(t, err)
	job, err := f.storage.GetVerificationJob(f.ctx, intent.Template.ID)
	require.NoError(t, err)
	artifact, err := f.storage.LoadEvidence(f.ctx, *job.Request.Binding.Context.ExecutionTranscript)
	require.NoError(t, err)
	trace, err := verification.DecodeExecutionTranscript(artifact.Data)
	require.NoError(t, err)
	require.Equal(t, []verification.ToolResult{{CallID: "SECRET-ID", Outcome: verification.ToolCompleted, Source: verification.ToolLocal}, {CallID: "external-call", Outcome: verification.ToolCompleted, Source: verification.ToolExternal}}, trace.Tools)
	require.Equal(t, r.Usage, trace.Usage)
	require.Len(t, trace.Messages, 4)
}
