package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/scopespy"
	"github.com/xraph/cortex/suspension"
)

func TestTotalTokenBudgetAcrossSteps(t *testing.T) {
	for _, total := range []int{6, 0} {
		for _, streaming := range []bool{false, true} {
			s := scopespy.New()
			dispatched := 0
			e, err := New(WithStore(s), WithLLM(&outcomeClient{response: &llm.Response{ToolCalls: []llm.ToolCall{{ID: "call", Name: "probe", Arguments: `{}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: total}}}), WithTool(llm.Tool{Name: "probe"}, func(context.Context, cortex.Invocation) (string, error) { dispatched++; return "ok", nil }))
			if err != nil {
				t.Fatal(err)
			}
			ctx := cortex.WithScope(context.Background(), scopeA())
			opts := &RunOverrides{MaxSteps: 5, Budget: &execution.Budget{MaxTokens: 10}}
			var r *run.Run
			if streaming {
				events := make(chan StreamEvent, 64)
				if err = e.StreamAgent(ctx, "assistant", "work", opts, events); err != nil {
					t.Fatal(err)
				}
				for event := range events {
					_ = event
				}
				saved := s.Runs()[0]
				r = &saved
			} else {
				r, err = e.RunAgent(ctx, "assistant", "work", opts)
				if err != nil {
					t.Fatal(err)
				}
			}
			wantSteps, wantCalls := 2, 1
			if total == 0 {
				wantSteps, wantCalls = 1, 0
			}
			if r.StopReason != run.StopTokenLimit || r.StepCount != wantSteps || dispatched != wantCalls {
				t.Fatalf("total=%d stream=%v run=%+v calls=%d", total, streaming, r, dispatched)
			}
			if r.Usage.Estimated != (total == 0) {
				t.Errorf("usage provenance: %+v", r.Usage)
			}
		}
	}
}

func TestExpiredBudgetDoesNotDispatch(t *testing.T) {
	s := scopespy.New()
	e, err := New(WithStore(s), WithLLM(&outcomeClient{response: &llm.Response{Content: "must not answer", FinishReason: "stop"}}))
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.RunAgent(cortex.WithScope(context.Background(), scopeA()), "assistant", "work", &RunOverrides{Budget: &execution.Budget{Deadline: time.Now().Add(-time.Second)}})
	if err != nil {
		t.Fatal(err)
	}
	if r.StopReason != run.StopDeadline || r.StepCount != 0 || r.Output != "" {
		t.Fatalf("expired budget dispatched: %+v", r)
	}
}

func TestResumeRetainsPreparedDigestAndUsage(t *testing.T) {
	s := scopespy.New()
	e := mustResumeEngine(t, s)
	r := suspendedFixture(t, s, e)
	saved := s.Suspensions()[0]
	if saved.Cont.SchemaVersion != 1 || saved.Cont.Provenance.PreparedDigest != r.Provenance.PreparedDigest || saved.Cont.Usage.Steps != r.StepCount || saved.Cont.Usage.ToolCalls != 1 {
		t.Fatalf("continuation schema=%d digest=%s usage=%+v", saved.Cont.SchemaVersion, saved.Cont.Provenance.PreparedDigest, saved.Cont.Usage)
	}
	resumed, err := e.Resume(resumeCtx(scopeA()), r.ID, oneResult(t, s, "yes"))
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Provenance.PreparedDigest != r.Provenance.PreparedDigest || resumed.Usage.Steps <= r.StepCount || resumed.Usage.ToolCalls != 1 {
		t.Fatalf("resume changed identity or reset usage: %+v", resumed)
	}
}

func TestResumeUnknownSchemaRejected(t *testing.T) {
	s := scopespy.New()
	e := mustResumeEngine(t, s)
	r := suspendedFixture(t, s, e)
	s.Suspensions()[0].Cont.SchemaVersion = 999
	if _, err := e.Resume(resumeCtx(scopeA()), r.ID, oneResult(t, s, "yes")); !errors.Is(err, cortex.ErrInvalidContinuation) {
		t.Fatalf("unknown schema resumed: %v", err)
	}
	stored, err := s.GetRun(resumeCtx(scopeA()), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != run.StatePaused {
		t.Fatalf("unsupported schema consumed claim: %s", stored.State)
	}
}

type revocableApproval struct{ revoked bool }

func (*revocableApproval) Visible(_ context.Context, _ cortex.Subject, tools []llm.Tool) []llm.Tool {
	return tools
}
func (a *revocableApproval) Authorize(context.Context, cortex.Subject, llm.ToolCall) error {
	if a.revoked {
		return errors.New("grant revoked")
	}
	return cortex.ErrRequiresApproval
}

func TestResumeRechecksRevokedGrant(t *testing.T) {
	s := scopespy.New()
	auth := &revocableApproval{}
	dispatched := 0
	e, err := New(WithStore(s), WithLLM(answeringLLM("probe")), WithToolAuthorizer(auth), WithTool(llm.Tool{Name: "probe"}, func(context.Context, cortex.Invocation) (string, error) { dispatched++; return "ok", nil }))
	if err != nil {
		t.Fatal(err)
	}
	ctx := resumeCtx(scopeA())
	r, err := e.RunAgent(ctx, "assistant", "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != run.StatePaused {
		t.Fatal("approval did not pause")
	}
	auth.revoked = true
	err = e.ResolveCheckpoint(ctx, s.Checkpoints()[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "operator"})
	if err == nil || dispatched != 0 {
		t.Fatalf("revoked call dispatched=%d, err=%v", dispatched, err)
	}
}

func TestPreparedEmptyToolsRejectsInventedCall(t *testing.T) {
	s := scopespy.New()
	dispatched := 0
	e, err := New(WithStore(s), WithLLM(answeringLLM("probe")), WithTool(llm.Tool{Name: "probe"}, func(context.Context, cortex.Invocation) (string, error) { dispatched++; return "ok", nil }))
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.RunAgent(resumeCtx(scopeA()), "assistant", "work", &RunOverrides{ToolsRestricted: true})
	if err != nil {
		t.Fatal(err)
	}
	if dispatched != 0 {
		t.Fatal("model escaped explicit empty allowlist")
	}
}

func TestResumeRejectsPreparedDowngradeAndAccountingRollback(t *testing.T) {
	for _, change := range []string{"downgrade", "steps", "tokens", "tools", "purpose"} {
		t.Run(change, func(t *testing.T) {
			s := scopespy.New()
			e := mustResumeEngine(t, s)
			r := suspendedFixture(t, s, e)
			cont := &s.Suspensions()[0].Cont
			switch change {
			case "downgrade":
				cont.SchemaVersion = 0
				cont.Prepared = nil
				cont.Provenance = run.ExecutionProvenance{}
				cont.Config.Budget = execution.Budget{}
				cont.Usage = execution.Usage{}
			case "steps":
				cont.StepIndex = 0
				cont.Usage.Steps = 0
			case "tokens":
				r.TokensUsed = 7
				r.Usage.Tokens = 7
				r.Usage.ReportedTokens = 7
				if err := s.UpdateRun(resumeCtx(scopeA()), r); err != nil {
					t.Fatal(err)
				}
				cont.TokensUsed = 0
				cont.Usage.Tokens = 0
				cont.Usage.ReportedTokens = 0
			case "tools":
				cont.Usage.ToolCalls = 0
			case "purpose":
				cont.Provenance.Purpose = run.PurposeOptimizationTrial
			}
			if _, err := e.Resume(resumeCtx(scopeA()), r.ID, oneResult(t, s, "yes")); !errors.Is(err, cortex.ErrInvalidContinuation) {
				t.Fatalf("%s accepted: %v", change, err)
			}
		})
	}
}

func TestApprovedBatchRechecksEachDispatch(t *testing.T) {
	s := scopespy.New()
	auth := &revocableApproval{}
	dispatched := 0
	e, err := New(WithStore(s), WithLLM(&scriptedLLM{toolCalls: []llm.ToolCall{{ID: "first", Name: "first", Arguments: `{}`}, {ID: "second", Name: "second", Arguments: `{}`}}}), WithToolAuthorizer(auth),
		WithTool(llm.Tool{Name: "first"}, func(context.Context, cortex.Invocation) (string, error) { auth.revoked = true; return "revoked", nil }),
		WithTool(llm.Tool{Name: "second"}, func(context.Context, cortex.Invocation) (string, error) { dispatched++; return "unsafe", nil }))
	if err != nil {
		t.Fatal(err)
	}
	ctx := resumeCtx(scopeA())
	if _, err = e.RunAgent(ctx, "assistant", "work", nil); err != nil {
		t.Fatal(err)
	}
	_ = e.ResolveCheckpoint(ctx, s.Checkpoints()[0].ID, checkpoint.Decision{Approved: true})
	if dispatched != 0 {
		t.Fatal("batch preflight allowed a later revoked effect")
	}
}

func TestLegacyContinuationRefusesPreparedOnlyPolicy(t *testing.T) {
	for _, cfg := range []suspension.RunConfig{{AllToolsRestricted: true}, {Budget: execution.Budget{MaxSteps: 1}}} {
		if _, _, err := decodeContinuation(suspension.Continuation{Config: cfg}); !errors.Is(err, cortex.ErrInvalidContinuation) {
			t.Fatalf("legacy decoder accepted unsupported policy: %+v", cfg)
		}
	}
}

func TestExternalResumeCannotSatisfyApproval(t *testing.T) {
	s := scopespy.New()
	e := mustResumeEngine(t, s)
	r := suspendedFixture(t, s, e)
	e.authorizer = &revocableApproval{}
	input := oneResult(t, s, "")
	input.ToolResults[0].Execute = true
	if _, err := e.Resume(resumeCtx(scopeA()), r.ID, input); !errors.Is(err, cortex.ErrRequiresApproval) {
		t.Fatalf("external resume supplied nonexistent approval: %v", err)
	}
}

func TestResumeCannotRemoveOriginalCallerDeadline(t *testing.T) {
	s := scopespy.New()
	e := mustResumeEngine(t, s)
	ctx, cancel := context.WithTimeout(resumeCtx(scopeA()), time.Hour)
	defer cancel()
	r, err := e.RunAgent(ctx, "assistant", "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Suspensions()[0].Cont.Config.Budget.Deadline = time.Time{}
	if _, err = e.Resume(resumeCtx(scopeA()), r.ID, oneResult(t, s, "yes")); !errors.Is(err, cortex.ErrInvalidContinuation) {
		t.Fatalf("removed caller deadline accepted: %v", err)
	}
}

func TestApprovedToolReceivesSavedDeadline(t *testing.T) {
	s := scopespy.New()
	deadline := time.Now().Add(300 * time.Millisecond)
	var received time.Time
	e, err := New(WithStore(s), WithLLM(answeringLLM("probe")), WithToolAuthorizer(&revocableApproval{}), WithTool(llm.Tool{Name: "probe"}, func(ctx context.Context, _ cortex.Invocation) (string, error) {
		received, _ = ctx.Deadline()
		if received.IsZero() {
			return "missing deadline", nil
		}
		<-ctx.Done()
		return "", ctx.Err()
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := resumeCtx(scopeA())
	if _, err = e.RunAgent(ctx, "assistant", "work", &RunOverrides{Budget: &execution.Budget{Deadline: deadline}}); err != nil {
		t.Fatal(err)
	}
	_ = e.ResolveCheckpoint(ctx, s.Checkpoints()[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "operator"})
	if !received.Equal(deadline) {
		t.Fatalf("approved handler deadline=%v, want %v", received, deadline)
	}
}

type failedBudgetClient struct{}

func (failedBudgetClient) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	return nil, errors.New("provider disconnected")
}
func (failedBudgetClient) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, errors.New("provider disconnected")
}

func TestFailedProviderAttemptRetainsUnknownUsage(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		s := scopespy.New()
		e, err := New(WithStore(s), WithLLM(failedBudgetClient{}))
		if err != nil {
			t.Fatal(err)
		}
		opts := &RunOverrides{Budget: &execution.Budget{MaxTokens: 20}}
		if streaming {
			events := make(chan StreamEvent, 64)
			if err = e.StreamAgent(resumeCtx(scopeA()), "assistant", "work", opts, events); err != nil {
				t.Fatal(err)
			}
			for event := range events {
				_ = event
			}
		} else {
			_, _ = e.RunAgent(resumeCtx(scopeA()), "assistant", "work", opts)
		}
		r := s.Runs()[0]
		if r.StepCount != 1 || r.Usage.Steps != 1 || r.Usage.Tokens != 20 || !r.Usage.Estimated {
			t.Fatalf("stream=%v failed attempt lost usage: %+v", streaming, r.Usage)
		}
	}
}

func TestToolBudgetRejectsWholeBatchBeforeEffects(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		s := scopespy.New()
		dispatched := 0
		client := &outcomeClient{response: &llm.Response{ToolCalls: []llm.ToolCall{{ID: "one", Name: "probe", Arguments: `{}`}, {ID: "two", Name: "probe", Arguments: `{}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 5}}}
		e, err := New(WithStore(s), WithLLM(client), WithTool(llm.Tool{Name: "probe"}, func(context.Context, cortex.Invocation) (string, error) { dispatched++; return "ok", nil }))
		if err != nil {
			t.Fatal(err)
		}
		opts := &RunOverrides{Budget: &execution.Budget{MaxToolCalls: 1}}
		if streaming {
			events := make(chan StreamEvent, 64)
			if err = e.StreamAgent(resumeCtx(scopeA()), "assistant", "work", opts, events); err != nil {
				t.Fatal(err)
			}
			for event := range events {
				_ = event
			}
		} else if _, err = e.RunAgent(resumeCtx(scopeA()), "assistant", "work", opts); err != nil {
			t.Fatal(err)
		}
		r := s.Runs()[0]
		if dispatched != 0 || r.StopReason != run.StopToolLimit || r.Usage.ToolCalls != 0 {
			t.Fatalf("stream=%v admitted partial batch: calls=%d run=%+v", streaming, dispatched, r)
		}
	}
}
