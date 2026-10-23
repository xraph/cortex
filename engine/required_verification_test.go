package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"

	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/verification"
)

type requiredBuilder func(context.Context, engine.VerificationIntentInput) (verification.CaptureIntent, error)

func (b requiredBuilder) BuildVerificationIntent(ctx context.Context, in engine.VerificationIntentInput) (verification.CaptureIntent, error) {
	return b(ctx, in)
}

type captureAuthorizer func(context.Context, evidence.Action, evidence.Ref) error

func (a captureAuthorizer) AuthorizeEvidence(ctx context.Context, action evidence.Action, ref evidence.Ref) error {
	return a(ctx, action, ref)
}

type requiredClient struct {
	recordingLLM
	before   func()
	calls    int
	response *llm.Response
	failure  error
	chunks   []llm.Chunk
}

func (c *requiredClient) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	c.calls++
	if c.before != nil {
		c.before()
	}
	if c.failure != nil {
		return nil, c.failure
	}
	if c.response != nil {
		return c.response, nil
	}
	return c.recordingLLM.Complete(ctx, req)
}

func requiredConfiguration(t *testing.T, f workerFixture) engine.RequiredVerificationConfig {
	t.Helper()
	return engine.RequiredVerificationConfig{
		Revision: "1", ContractID: f.request.Job.ContractID, EvaluationBudget: *f.request.Job.EvaluationBudget,
		WorkerOwner: "required-worker", LeaseDuration: time.Minute,
		CaptureAuthorizer: captureAuthorizer(func(context.Context, evidence.Action, evidence.Ref) error { return nil }),
		Builder: requiredBuilder(func(_ context.Context, in engine.VerificationIntentInput) (verification.CaptureIntent, error) {
			req, _, err := f.request.Job.Freeze()
			require.NoError(t, err)
			req.ID = id.New(verification.PrefixJob)
			req.ExpectedRunRevision = 1
			req.Binding.RunID = in.RunID
			req.Binding.Scope = in.Scope
			req.Binding.Context.PreparedDigest = in.PreparedDigest
			req.Binding.Candidate.ID = id.New(evidence.PrefixArtifact)
			req.Binding.Candidate.Bytes = 0
			req.Binding.Candidate.SHA256 = evidence.Digest(nil)
			req.Deadline = in.Deadline
			data, err := in.Inputs.Encode()
			require.NoError(t, err)
			ref := req.Binding.Candidate
			ref.ID, ref.MediaType = id.New(evidence.PrefixArtifact), "application/json"
			artifact, err := evidence.Seal(evidence.Artifact{Ref: ref, Data: data})
			require.NoError(t, err)
			req.Binding.Context.ExecutionInputs = &artifact.Ref
			req.Binding.Evidence = append(req.Binding.Evidence, artifact.Ref)
			trace := req.Binding.Candidate
			trace.ID, trace.MediaType = id.New(evidence.PrefixArtifact), "application/json"
			return verification.SealCaptureIntent(verification.CaptureIntent{SchemaVersion: 2, Transcript: &trace, MaxTranscriptBytes: 1 << 20, ID: id.New(verification.PrefixCaptureIntent), Template: req, ResultID: id.New(verification.PrefixResult), MaxCandidateBytes: 4096})
		}),
	}
}

func requiredSetup(t *testing.T, f workerFixture, client llm.Client, changes ...func(*engine.RequiredVerificationConfig)) (*engine.Engine, *agent.Config, *engine.RunOverrides) {
	t.Helper()
	ag := &agent.Config{ID: id.NewAgentID(), Name: "required"}
	require.NoError(t, f.storage.Create(f.ctx, ag))
	cfg := requiredConfiguration(t, f)
	for _, change := range changes {
		change(&cfg)
	}
	e, err := engine.New(engine.WithStore(f.storage), engine.WithLLM(client), engine.WithModelRevision("smart", "model:1"), engine.WithVerificationWorker(f.config), engine.WithRequiredVerification(cfg))
	require.NoError(t, err)
	return e, ag, &engine.RunOverrides{ToolsRestricted: true, Budget: &execution.Budget{MaxSteps: 3, MaxTokens: 1000, Deadline: time.Now().Add(2 * time.Minute)}}
}

func TestRequiredVerificationPrivateUntilCommit(t *testing.T) {
	f := newWorkerFixture(t)
	client := &requiredClient{}
	e, ag, overrides := requiredSetup(t, f, client)
	client.before = func() {
		runs, err := f.storage.ListRuns(f.ctx, &run.ListFilter{AgentID: ag.ID.String()})
		require.NoError(t, err)
		require.Len(t, runs, 1)
		r := runs[0]
		require.Empty(t, r.Output)
		intent, err := f.storage.GetRunCaptureIntent(f.ctx, r.ID)
		require.NoError(t, err)
		require.Equal(t, r.Provenance.PreparedDigest, intent.Template.Binding.Context.PreparedDigest)
		require.Equal(t, 970, r.Budget.MaxTokens)
	}
	f.adapter.afterEvaluate = func() {
		runs, err := f.storage.ListRuns(f.ctx, &run.ListFilter{AgentID: ag.ID.String()})
		require.NoError(t, err)
		require.Len(t, runs, 1)
		require.Empty(t, runs[0].Output)
		steps, err := f.storage.ListSteps(f.ctx, runs[0].ID)
		require.NoError(t, err)
		for _, step := range steps {
			require.Empty(t, step.Input)
			require.Empty(t, step.Output)
		}
		history, err := f.storage.LoadConversation(f.ctx, ag.ID, runs[0].SessionID, 100)
		require.NoError(t, err)
		require.Empty(t, history)
	}
	prepared, err := e.PrepareAgent(f.ctx, ag.ID, overrides)
	require.NoError(t, err)
	encoded, err := json.Marshal(prepared)
	require.NoError(t, err)
	var restored engine.PreparedAgent
	require.NoError(t, json.Unmarshal(encoded, &restored))
	r, err := e.RunPrepared(f.ctx, &restored, "work", engine.PreparedRunOptions{})
	require.NoError(t, err)
	require.Equal(t, verification.StatusPassed, r.VerificationStatus)
	require.Equal(t, "done", r.Output)
	require.Equal(t, 1, client.calls)
	require.Equal(t, 1, f.adapter.evaluations)
}

func (c *requiredClient) CompleteStream(_ context.Context, _ *llm.Request) (llm.Stream, error) {
	c.calls++
	if c.before != nil {
		c.before()
	}
	if c.failure != nil {
		return nil, c.failure
	}
	return &requiredStream{chunks: c.chunks}, nil
}

type requiredStream struct {
	chunks []llm.Chunk
	index  int
}

func (s *requiredStream) Next(context.Context) (*llm.Chunk, error) {
	if s.index >= len(s.chunks) {
		return nil, io.EOF
	}
	chunk := s.chunks[s.index]
	s.index++
	return &chunk, nil
}
func (*requiredStream) Close() error      { return nil }
func (*requiredStream) Usage() *llm.Usage { return &llm.Usage{TotalTokens: 5} }

func TestRequiredVerificationStream(t *testing.T) {
	for _, status := range []verification.Status{verification.StatusPassed, verification.StatusFailed, verification.StatusInconclusive} {
		t.Run(string(status), func(t *testing.T) {
			f := newWorkerFixture(t)
			f.adapter.status = status
			client := &requiredClient{chunks: []llm.Chunk{{Content: "private-"}, {Content: "candidate", FinishReason: "stop"}}}
			e, ag, overrides := requiredSetup(t, f, client)
			events := make(chan engine.StreamEvent, 20)
			f.adapter.afterEvaluate = func() {
				runs, err := f.storage.ListRuns(f.ctx, &run.ListFilter{AgentID: ag.ID.String()})
				require.NoError(t, err)
				require.Len(t, runs, 1)
				require.Empty(t, runs[0].Output)
			}
			require.NoError(t, e.StreamAgent(f.ctx, ag.Name, "work", overrides, events))
			var delivered []engine.StreamEvent
			for event := range events {
				require.NotEqual(t, engine.EventToken, event.Type)
				delivered = append(delivered, event)
			}
			raw, err := json.Marshal(delivered)
			require.NoError(t, err)
			if status == verification.StatusPassed {
				require.Contains(t, string(raw), "private-candidate")
				require.Equal(t, engine.EventDone, delivered[len(delivered)-1].Type)
			} else {
				require.NotContains(t, string(raw), "private-candidate")
				last := delivered[len(delivered)-1]
				require.Equal(t, engine.EventError, last.Type)
				require.Equal(t, "run_unverified", last.Data["code"])
				require.Equal(t, false, last.Data["retryable"])
				require.Equal(t, string(status), last.Data["verification_status"])
				require.NotEmpty(t, last.Data["run_id"])
				for _, event := range delivered {
					require.NotEqual(t, engine.EventDone, event.Type)
				}
			}
			require.Equal(t, 1, client.calls)
			require.Equal(t, 1, f.adapter.evaluations)
		})
	}
}

func TestRequiredVerificationRejectsUnverifiedAnswers(t *testing.T) {
	for _, status := range []verification.Status{verification.StatusFailed, verification.StatusInconclusive} {
		t.Run(string(status), func(t *testing.T) {
			f := newWorkerFixture(t)
			f.adapter.status = status
			e, ag, overrides := requiredSetup(t, f, &requiredClient{})
			r, err := e.RunAgent(f.ctx, ag.Name, "work", overrides)
			require.ErrorIs(t, err, cortex.ErrRunUnverified)
			require.NotNil(t, r)
			require.Empty(t, r.Output)
			require.Equal(t, status, r.VerificationStatus)
		})
	}
}

func TestRequiredVerificationAdmission(t *testing.T) {
	cases := []struct {
		name   string
		change func(*engine.RequiredVerificationConfig)
	}{
		{"wrong run", func(c *engine.RequiredVerificationConfig) {
			original := c.Builder
			c.Builder = requiredBuilder(func(ctx context.Context, in engine.VerificationIntentInput) (verification.CaptureIntent, error) {
				in.RunID = id.NewAgentRunID()
				return original.BuildVerificationIntent(ctx, in)
			})
		}},
		{"wrong budget", func(c *engine.RequiredVerificationConfig) {
			original := c.Builder
			c.Builder = requiredBuilder(func(ctx context.Context, in engine.VerificationIntentInput) (verification.CaptureIntent, error) {
				v, err := original.BuildVerificationIntent(ctx, in)
				if err != nil {
					return v, err
				}
				v.Template.EvaluationBudget.MaxTokens++
				return verification.SealCaptureIntent(v)
			})
		}},
		{"builder error", func(c *engine.RequiredVerificationConfig) {
			c.Builder = requiredBuilder(func(context.Context, engine.VerificationIntentInput) (verification.CaptureIntent, error) {
				return verification.CaptureIntent{}, errors.New("private-context")
			})
		}},
		{"denied capture", func(c *engine.RequiredVerificationConfig) {
			c.CaptureAuthorizer = captureAuthorizer(func(context.Context, evidence.Action, evidence.Ref) error { return errors.New("private-context") })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newWorkerFixture(t)
			client := &requiredClient{}
			e, ag, overrides := requiredSetup(t, f, client, tc.change)
			_, err := e.RunAgent(f.ctx, ag.Name, "work", overrides)
			require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
			require.NotContains(t, err.Error(), "private-context")
			require.Zero(t, client.calls)
			require.Zero(t, f.adapter.evaluations)
			runs, err := f.storage.ListRuns(f.ctx, &run.ListFilter{AgentID: ag.ID.String()})
			require.NoError(t, err)
			require.Empty(t, runs)
		})
	}
}

func TestRequiredVerificationRejectsMissingBudget(t *testing.T) {
	for _, budget := range []execution.Budget{{MaxSteps: 3}, {MaxSteps: 3, MaxTokens: 30, Deadline: time.Now().Add(time.Minute)}, {MaxSteps: 3, MaxTokens: 1000}} {
		t.Run("budget", func(t *testing.T) {
			f := newWorkerFixture(t)
			client := &requiredClient{}
			e, ag, overrides := requiredSetup(t, f, client)
			overrides.Budget = &budget
			_, err := e.PrepareAgent(f.ctx, ag.ID, overrides)
			require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
			require.Zero(t, client.calls)
		})
	}
}

func TestRequiredVerificationPolicyCannotBeDropped(t *testing.T) {
	f := newWorkerFixture(t)
	client := &requiredClient{}
	e, ag, overrides := requiredSetup(t, f, client)
	required, err := e.PrepareAgent(f.ctx, ag.ID, overrides)
	require.NoError(t, err)
	plain, err := engine.New(engine.WithStore(f.storage), engine.WithLLM(client))
	require.NoError(t, err)
	_, err = plain.RunPrepared(f.ctx, required, "work", engine.PreparedRunOptions{})
	require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
	unverified, err := plain.PrepareAgent(f.ctx, ag.ID, overrides)
	require.NoError(t, err)
	_, err = e.RunPrepared(f.ctx, unverified, "work", engine.PreparedRunOptions{})
	require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
	_, err = e.RunPrepared(f.ctx, required, "work", engine.PreparedRunOptions{AcceptanceRef: "other"})
	require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
	require.Zero(t, client.calls)
}

func TestRequiredVerificationWithholdsUnexpectedResponses(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, mode := range []string{"tools", "length", "error"} {
			t.Run(mode+map[bool]string{false: "/sync", true: "/stream"}[streaming], func(t *testing.T) {
				f := newWorkerFixture(t)
				client := &requiredClient{}
				switch mode {
				case "tools":
					client.response = &llm.Response{Content: "private-candidate", ToolCalls: []llm.ToolCall{{ID: "private-call", Name: "probe", Arguments: `{"secret":"private-candidate"}`}}, FinishReason: "tool_calls"}
					client.chunks = []llm.Chunk{{Content: "private-candidate", ToolCalls: client.response.ToolCalls, FinishReason: "tool_calls"}}
				case "length":
					client.response = &llm.Response{Content: "private-candidate", FinishReason: "length"}
					client.chunks = []llm.Chunk{{Content: "private-candidate", FinishReason: "length"}}
				case "error":
					client.failure = errors.New("private-candidate")
				}
				e, ag, overrides := requiredSetup(t, f, client)
				if streaming {
					events := make(chan engine.StreamEvent, 20)
					require.NoError(t, e.StreamAgent(f.ctx, ag.Name, "work", overrides, events))
					for event := range events {
						raw, err := json.Marshal(event)
						require.NoError(t, err)
						require.NotContains(t, string(raw), "private-candidate")
						require.NotEqual(t, engine.EventDone, event.Type)
					}
				} else {
					r, err := e.RunAgent(f.ctx, ag.Name, "work", overrides)
					if err != nil {
						require.NotContains(t, err.Error(), "private-candidate")
					}
					if r != nil {
						require.Empty(t, r.Output)
						require.NotEqual(t, run.StateCompleted, r.State)
					}
				}
				require.Zero(t, f.adapter.evaluations)
				runs, err := f.storage.ListRuns(f.ctx, &run.ListFilter{AgentID: ag.ID.String()})
				require.NoError(t, err)
				require.Len(t, runs, 1)
				raw, err := json.Marshal(runs[0])
				require.NoError(t, err)
				require.NotContains(t, string(raw), "private-candidate")
			})
		}
	}
}

func TestRequiredVerificationConfiguration(t *testing.T) {
	for _, name := range []string{"builder", "authorizer", "revision", "contract", "owner", "lease", "budget", "worker", "store", "llm", "order"} {
		t.Run(name, func(t *testing.T) {
			f := newWorkerFixture(t)
			cfg := requiredConfiguration(t, f)
			switch name {
			case "builder":
				cfg.Builder = nil
			case "authorizer":
				cfg.CaptureAuthorizer = nil
			case "revision":
				cfg.Revision = ""
			case "contract":
				cfg.ContractID = id.NewAgentID()
			case "owner":
				cfg.WorkerOwner = ""
			case "lease":
				cfg.LeaseDuration = verification.MaxJobLease + time.Second
			case "budget":
				cfg.EvaluationBudget.MaxCalls = 0
			}
			opts := []engine.Option{engine.WithRequiredVerification(cfg)}
			if name != "store" {
				opts = append(opts, engine.WithStore(f.storage))
			}
			if name != "llm" {
				opts = append(opts, engine.WithLLM(&requiredClient{}))
			}
			if name != "worker" {
				opts = append(opts, engine.WithVerificationWorker(f.config))
			}
			_, err := engine.New(opts...)
			if name == "order" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestRequiredVerificationRejectsToolsWithoutAuthoritiesAndSegments(t *testing.T) {
	for _, mode := range []string{"tool", "segments"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			client := &requiredClient{}
			cfg := requiredConfiguration(t, f)
			opts := []engine.Option{engine.WithStore(f.storage), engine.WithLLM(client), engine.WithModelRevision("smart", "model:1"), engine.WithVerificationWorker(f.config), engine.WithRequiredVerification(cfg)}
			if mode == "tool" {
				opts = append(opts, engine.WithTool(llm.Tool{Name: "probe"}, func(context.Context, cortex.Invocation) (string, error) { t.Fatal("tool dispatched"); return "", nil }))
			} else {
				opts = append(opts, engine.WithPublicationPolicy(safety.Policy{Mode: safety.ModeCheckedSegments}), engine.WithPublicationGate(requiredSegmentGate{}))
			}
			e, err := engine.New(opts...)
			require.NoError(t, err)
			ag := &agent.Config{ID: id.NewAgentID(), Name: "gate-policy"}
			require.NoError(t, f.storage.Create(f.ctx, ag))
			_, err = e.PrepareAgent(f.ctx, ag.ID, &engine.RunOverrides{Budget: &execution.Budget{MaxSteps: 3, MaxTokens: 1000, Deadline: time.Now().Add(time.Minute)}})
			require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
			require.Zero(t, client.calls)
		})
	}
}

type requiredSegmentGate struct{}

func (requiredSegmentGate) Check(_ context.Context, r safety.PublicationRequest) (safety.PublicationDecision, error) {
	return safety.PublicationDecision{Text: r.Content, Status: safety.StatusApproved}, nil
}
func (requiredSegmentGate) NewSegmentChecker(context.Context, safety.PublicationRequest) (safety.SegmentChecker, error) {
	return nil, errors.New("must not open")
}

type requiredHooks struct{ outputs []string }

func (*requiredHooks) Name() string { return "required-verification-hooks" }
func (h *requiredHooks) OnRunCompleted(_ context.Context, _ id.AgentID, _ id.AgentRunID, output string, _ time.Duration) error {
	h.outputs = append(h.outputs, output)
	return nil
}

func TestRequiredVerificationRecoveryDoesNotGenerateAgain(t *testing.T) {
	f := newWorkerFixture(t)
	f.adapter.lost = true
	client := &requiredClient{}
	cfg := requiredConfiguration(t, f)
	hooks := &requiredHooks{}
	e, err := engine.New(engine.WithStore(f.storage), engine.WithLLM(client), engine.WithModelRevision("smart", "model:1"), engine.WithVerificationWorker(f.config), engine.WithRequiredVerification(cfg), engine.WithExtension(hooks))
	require.NoError(t, err)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "lost-required"}
	require.NoError(t, f.storage.Create(f.ctx, ag))
	r, err := e.RunAgent(f.ctx, ag.Name, "work", &engine.RunOverrides{Budget: &execution.Budget{MaxSteps: 3, MaxTokens: 1000, Deadline: time.Now().Add(2 * time.Minute)}})
	require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
	require.NotNil(t, r)
	require.Empty(t, r.Output)
	require.Empty(t, hooks.outputs)
	intent, err := f.storage.GetRunCaptureIntent(f.ctx, r.ID)
	require.NoError(t, err)
	job, err := f.storage.GetVerificationJob(f.ctx, intent.Template.ID)
	require.NoError(t, err)
	require.NotNil(t, job.Lease)
	require.NoError(t, f.storage.Close())
	reopened := openSQLiteStoreForTest(t, f.path)
	recovery, err := engine.New(engine.WithStore(reopened), engine.WithVerificationWorker(f.config))
	require.NoError(t, err)
	result, err := recovery.VerifyRun(f.ctx, engine.VerifyRunRequest{Job: job.Request, ResultID: intent.ResultID, Claim: verification.JobClaim{JobID: job.Request.ID, ClaimID: job.Lease.ClaimID, Owner: job.Lease.Owner, Duration: time.Minute}})
	require.NoError(t, err)
	require.NotNil(t, result.Receipt)
	saved, err := reopened.GetRun(f.ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, "done", saved.Output)
	require.Equal(t, 1, client.calls)
	require.Equal(t, 1, f.adapter.evaluations)
	require.Equal(t, 1, f.adapter.reconciliations)
	require.Empty(t, hooks.outputs) // Explicit recovery does not replay completion hooks.
}

func TestRequiredVerificationCaptureRevocation(t *testing.T) {
	f := newWorkerFixture(t)
	client := &requiredClient{}
	checks := 0
	e, ag, overrides := requiredSetup(t, f, client, func(c *engine.RequiredVerificationConfig) {
		c.CaptureAuthorizer = captureAuthorizer(func(_ context.Context, action evidence.Action, ref evidence.Ref) error {
			require.Equal(t, evidence.ActionPut, action)
			if ref.MediaType == "application/json" {
				return nil
			}
			checks++
			if checks > 1 {
				require.Positive(t, ref.Bytes)
				return errors.New("private-revocation")
			}
			require.Zero(t, ref.Bytes)
			return nil
		})
	})
	r, err := e.RunAgent(f.ctx, ag.Name, "work", overrides)
	require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
	require.NotContains(t, err.Error(), "private-revocation")
	require.Empty(t, r.Output)
	require.Equal(t, 2, checks)
	require.Equal(t, 1, client.calls)
	require.Zero(t, f.adapter.evaluations)
	require.Equal(t, run.StateFailed, r.State)
}

func TestRequiredVerificationResumeFailsBeforeClaim(t *testing.T) {
	for _, marker := range []bool{false, true} {
		t.Run(map[bool]string{false: "required engine", true: "required record"}[marker], func(t *testing.T) {
			f := newWorkerFixture(t)
			ag := &agent.Config{ID: id.NewAgentID(), Name: "paused"}
			require.NoError(t, f.storage.Create(f.ctx, ag))
			legacy, err := engine.New(engine.WithStore(f.storage), engine.WithLLM(&preparedPauseClient{}), engine.WithExternalTool(llm.Tool{Name: "external"}))
			require.NoError(t, err)
			r, err := legacy.RunAgent(f.ctx, ag.Name, "work", nil)
			require.NoError(t, err)
			require.Equal(t, run.StatePaused, r.State)
			client := &requiredClient{}
			opts := []engine.Option{engine.WithStore(f.storage), engine.WithLLM(client)}
			if marker {
				r.Provenance.VerificationRequired = true
				require.NoError(t, f.storage.UpdateRun(f.ctx, r))
			} else {
				opts = append(opts, engine.WithVerificationWorker(f.config), engine.WithRequiredVerification(requiredConfiguration(t, f)))
			}
			e, err := engine.New(opts...)
			require.NoError(t, err)
			_, err = e.Resume(f.ctx, r.ID, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "pending", Content: "result"}}})
			require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
			saved, err := f.storage.GetRun(f.ctx, r.ID)
			require.NoError(t, err)
			require.Equal(t, run.StatePaused, saved.State)
			require.Equal(t, r.Revision, saved.Revision)
			_, err = f.storage.GetSuspension(f.ctx, r.ID)
			require.NoError(t, err)
			require.Zero(t, client.calls)
		})
	}
}

func TestRequiredVerificationCompletionHook(t *testing.T) {
	for _, status := range []verification.Status{verification.StatusPassed, verification.StatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			f := newWorkerFixture(t)
			f.adapter.status = status
			client := &requiredClient{}
			hooks := &requiredHooks{}
			e, err := engine.New(engine.WithStore(f.storage), engine.WithLLM(client), engine.WithModelRevision("smart", "model:1"), engine.WithVerificationWorker(f.config), engine.WithRequiredVerification(requiredConfiguration(t, f)), engine.WithExtension(hooks))
			require.NoError(t, err)
			ag := &agent.Config{ID: id.NewAgentID(), Name: "hooks"}
			require.NoError(t, f.storage.Create(f.ctx, ag))
			f.adapter.afterEvaluate = func() { require.Empty(t, hooks.outputs) }
			_, err = e.RunAgent(f.ctx, ag.Name, "work", &engine.RunOverrides{Budget: &execution.Budget{MaxSteps: 3, MaxTokens: 1000, Deadline: time.Now().Add(time.Minute)}})
			if status == verification.StatusPassed {
				require.NoError(t, err)
				require.Equal(t, []string{"done"}, hooks.outputs)
			} else {
				require.ErrorIs(t, err, cortex.ErrRunUnverified)
				require.Empty(t, hooks.outputs)
			}
		})
	}
}

func TestRequiredVerificationConcurrentCaptureCannotImplySuccess(t *testing.T) {
	f := newWorkerFixture(t)
	client := &requiredClient{}
	e, ag, overrides := requiredSetup(t, f, client)
	client.before = func() {
		rows, err := f.storage.ListRuns(f.ctx, &run.ListFilter{AgentID: ag.ID.String()})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		intent, err := f.storage.GetRunCaptureIntent(f.ctx, rows[0].ID)
		require.NoError(t, err)
		trace := verification.ExecutionTranscript{SchemaVersion: 1, RunID: rows[0].ID, InputsDigest: rows[0].Provenance.ExecutionInputsDigest, Messages: []llm.Message{{Role: "assistant", Content: "other candidate"}}, Usage: execution.Usage{Steps: 1}}
		data, err := trace.Encode()
		require.NoError(t, err)
		_, err = f.storage.CaptureCandidate(f.ctx, run.CandidateCapture{SchemaVersion: 2, Transcript: data, Usage: trace.Usage, IntentID: intent.ID, ExpectedRunRevision: rows[0].Revision, Output: "other candidate", FinishReason: "stop"})
		require.NoError(t, err)
	}
	r, err := e.RunAgent(f.ctx, ag.Name, "work", overrides)
	require.ErrorIs(t, err, cortex.ErrRunUnverified)
	require.Empty(t, r.Output)
	require.Equal(t, verification.StatusPending, r.VerificationStatus)
	require.Zero(t, f.adapter.evaluations)
}

func TestRequiredVerificationStreamUncertainRecoveryMetadata(t *testing.T) {
	f := newWorkerFixture(t)
	f.adapter.lost = true
	client := &requiredClient{chunks: []llm.Chunk{{Content: "SECRET candidate", FinishReason: "stop"}}}
	e, ag, overrides := requiredSetup(t, f, client)
	events := make(chan engine.StreamEvent, 20)
	require.NoError(t, e.StreamAgent(f.ctx, ag.Name, "work", overrides, events))
	var last engine.StreamEvent
	for event := range events {
		require.NotEqual(t, engine.EventDone, event.Type)
		require.NotEqual(t, engine.EventToken, event.Type)
		last = event
	}
	require.Equal(t, engine.EventError, last.Type)
	require.Equal(t, "verification_unavailable", last.Data["code"])
	require.Equal(t, false, last.Data["retryable"])
	require.Equal(t, "pending", last.Data["verification_status"])
	runID, err := id.ParseAgentRunID(last.Data["run_id"].(string))
	require.NoError(t, err)
	saved, err := f.storage.GetRun(f.ctx, runID)
	require.NoError(t, err)
	require.Empty(t, saved.Output)
	intent, err := f.storage.GetRunCaptureIntent(f.ctx, runID)
	require.NoError(t, err)
	job, err := f.storage.GetVerificationJob(f.ctx, intent.Template.ID)
	require.NoError(t, err)
	require.Equal(t, verification.JobRunning, job.State)
	require.Equal(t, 1, f.adapter.evaluations)
	raw, err := json.Marshal(last.Data)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "SECRET")
	require.NotContains(t, string(raw), "lost provider reply")
}
