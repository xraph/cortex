package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/scopespy"
)

// outcomeClient supplies provider observations; the engine performs real tool
// dispatch, state transitions and lifecycle publication against the recording store.
type outcomeClient struct{ response *llm.Response }

func (c *outcomeClient) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	return c.response, nil
}
func (c *outcomeClient) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return &outcomeStream{response: c.response}, nil
}

type outcomeStream struct {
	response *llm.Response
	sent     bool
}

func (s *outcomeStream) Next(context.Context) (*llm.Chunk, error) {
	if s.sent {
		return nil, io.EOF
	}
	s.sent = true
	if s.response == nil {
		return nil, nil
	}
	return &llm.Chunk{Content: s.response.Content, ToolCalls: s.response.ToolCalls, FinishReason: s.response.FinishReason}, nil
}
func (*outcomeStream) Close() error { return nil }
func (s *outcomeStream) Usage() *llm.Usage {
	if s.response == nil {
		return nil
	}
	return &s.response.Usage
}

type outcomeHooks struct{ completed int }

func (*outcomeHooks) Name() string { return "outcome-test" }
func (h *outcomeHooks) OnRunCompleted(context.Context, id.AgentID, id.AgentRunID, string, time.Duration) error {
	h.completed++
	return nil
}

type outcomeStore struct {
	*scopespy.Spy
	failUpdate bool
}

func (s *outcomeStore) UpdateRun(ctx context.Context, r *run.Run) error {
	if s.failUpdate {
		return errors.New("terminal store unavailable")
	}
	return s.Spy.UpdateRun(ctx, r)
}

func runOutcome(t *testing.T, streaming bool, response *llm.Response, failUpdate bool) (*run.Run, []StreamEvent, int, error) {
	t.Helper()
	s := &outcomeStore{Spy: scopespy.New(), failUpdate: failUpdate}
	hooks := &outcomeHooks{}
	e, err := New(WithStore(s), WithLLM(&outcomeClient{response: response}), WithExtension(hooks), WithTool(llm.Tool{Name: "probe"}, func(context.Context, cortex.Invocation) (string, error) {
		if response != nil && response.FinishReason == "length" {
			t.Error("executed a truncated tool call")
		}
		return "ok", nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(cortex.WithScope(context.Background(), scopeA()), 5*time.Second)
	defer cancel()
	opts := &RunOverrides{MaxSteps: 2}
	if !streaming {
		r, runErr := e.RunAgent(ctx, "assistant", "work", opts)
		return r, nil, hooks.completed, runErr
	}
	ch := make(chan StreamEvent, 64)
	if err = e.StreamAgent(ctx, "assistant", "work", opts, ch); err != nil {
		t.Fatal(err)
	}
	var events []StreamEvent
	var runID id.AgentRunID
	for ev := range ch {
		events = append(events, ev)
		if ev.Type == EventRunStarted {
			runID, err = id.ParseAgentRunID(ev.Data["run_id"].(string))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	r, err := s.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	return r, events, hooks.completed, nil
}

func outcomeJSON(t *testing.T, r *run.Run) map[string]any {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err = json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRunStepLimitIsIncomplete(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "stream"}[streaming], func(t *testing.T) {
			r, events, completed, err := runOutcome(t, streaming, &llm.Response{ToolCalls: []llm.ToolCall{{ID: "call", Name: "probe", Arguments: `{}`}}, FinishReason: "tool_calls"}, false)
			if err != nil {
				t.Fatal(err)
			}
			if string(r.State) != "incomplete" {
				t.Errorf("state = %q, want incomplete", r.State)
			}
			if got := outcomeJSON(t, r)["stop_reason"]; got != "step_limit" {
				t.Errorf("stop reason = %v, want step_limit", got)
			}
			if completed != 0 {
				t.Errorf("completion hooks = %d, want 0", completed)
			}
			for _, ev := range events {
				if ev.Type == EventDone {
					t.Error("incomplete execution emitted done")
				}
			}
		})
	}
}
func TestRunLengthFinishIsIncomplete(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "stream"}[streaming], func(t *testing.T) {
			r, _, completed, err := runOutcome(t, streaming, &llm.Response{Content: "partial", FinishReason: "length"}, false)
			if err != nil {
				t.Fatal(err)
			}
			if string(r.State) != "incomplete" || r.Output != "partial" {
				t.Errorf("result = %+v, want incomplete with partial output", r)
			}
			if got := outcomeJSON(t, r)["stop_reason"]; got != "token_limit" {
				t.Errorf("stop reason = %v", got)
			}
			if completed != 0 {
				t.Errorf("completion hooks = %d", completed)
			}
		})
	}
}
func TestRunFinalAnswerIsUnchecked(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "stream"}[streaming], func(t *testing.T) {
			r, _, completed, err := runOutcome(t, streaming, &llm.Response{Content: "answer", FinishReason: "stop"}, false)
			if err != nil {
				t.Fatal(err)
			}
			m := outcomeJSON(t, r)
			if r.State != run.StateCompleted || completed != 1 {
				t.Errorf("state=%q completion hooks=%d", r.State, completed)
			}
			if m["verification_status"] != "unchecked" || m["stop_reason"] != "final_answer" {
				t.Errorf("assurance fields = %v", m)
			}
		})
	}
}
func TestRunTerminalWriteFailureDoesNotEmitDone(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "stream"}[streaming], func(t *testing.T) {
			r, events, completed, err := runOutcome(t, streaming, &llm.Response{Content: "answer", FinishReason: "stop"}, true)
			if !streaming && err == nil {
				t.Error("terminal write failure returned success")
			}
			if streaming && r.State != run.StateRunning {
				t.Errorf("durable state=%q, want running", r.State)
			}
			if completed != 0 {
				t.Errorf("completion hooks=%d, want 0", completed)
			}
			sawError := false
			for _, ev := range events {
				if ev.Type == EventDone {
					t.Error("failed terminal write emitted done")
				}
				if ev.Type == EventError {
					sawError = true
				}
			}
			if streaming && !sawError {
				t.Error("stream did not report persistence error")
			}
		})
	}
}

func TestRunUnknownFinishIsIncomplete(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		r, _, completed, err := runOutcome(t, streaming, &llm.Response{Content: "partial", FinishReason: "provider_interrupted"}, false)
		if err != nil {
			t.Fatal(err)
		}
		if r.State != run.StateIncomplete || completed != 0 {
			t.Errorf("unknown finish accepted: %+v, hooks=%d", r, completed)
		}
	}
}

func TestTruncatedToolCallsAreNotExecuted(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		r, _, completed, err := runOutcome(t, streaming, &llm.Response{ToolCalls: []llm.ToolCall{{ID: "call", Name: "probe", Arguments: `{}`}}, FinishReason: "length"}, false)
		if err != nil {
			t.Fatal(err)
		}
		if r.StepCount != 1 || r.StopReason != run.StopTokenLimit || completed != 0 {
			t.Errorf("truncated tool call continued: %+v, hooks=%d", r, completed)
		}
	}
}

func TestFinalizeRunIsIdempotentAndPreservesCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		s := scopespy.New()
		hooks := &outcomeHooks{}
		e, err := New(WithStore(s), WithExtension(hooks))
		if err != nil {
			t.Fatal(err)
		}
		ctx := cortex.WithScope(context.Background(), scopeA())
		r := &run.Run{ID: id.NewAgentRunID(), AgentID: id.NewAgentID(), State: run.StateRunning}
		if err = s.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		if cancelled {
			saved := *r
			saved.State = run.StateCancelled
			saved.StopReason = run.StopCancelled
			if err = s.UpdateRun(ctx, &saved); err != nil {
				t.Fatal(err)
			}
		}
		for range 2 {
			if err = e.finalizeRun(ctx, r, run.Outcome{StopReason: run.StopFinalAnswer, Output: "answer"}); err != nil {
				t.Fatal(err)
			}
		}
		wantHooks := 1
		if cancelled {
			wantHooks = 0
			if r.State != run.StateCancelled {
				t.Fatal("overwrote cancellation")
			}
		}
		if hooks.completed != wantHooks {
			t.Errorf("hooks=%d, want %d", hooks.completed, wantHooks)
		}
	}
}

func TestRunMalformedResponseIsIncomplete(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		r, _, completed, err := runOutcome(t, streaming, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if r.State != run.StateIncomplete || r.StopReason != run.StopInvalidResponse || completed != 0 {
			t.Errorf("malformed response: %+v, hooks=%d", r, completed)
		}
	}
}

func TestMockTerminalWriteFailureDoesNotEmitDone(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		s := &outcomeStore{Spy: scopespy.New(), failUpdate: true}
		hooks := &outcomeHooks{}
		e, err := New(WithStore(s), WithExtension(hooks))
		if err != nil {
			t.Fatal(err)
		}
		ctx := cortex.WithScope(context.Background(), scopeA())
		if !streaming {
			if _, err = e.RunAgent(ctx, "assistant", "x", nil); err == nil {
				t.Error("mock returned success after write failure")
			}
		} else {
			events := make(chan StreamEvent, 32)
			if err = e.StreamAgent(ctx, "assistant", "x", nil, events); err != nil {
				t.Fatal(err)
			}
			sawError := false
			for ev := range events {
				if ev.Type == EventDone {
					t.Error("mock emitted done after write failure")
				}
				if ev.Type == EventError {
					sawError = true
				}
			}
			if !sawError {
				t.Error("mock omitted persistence failure")
			}
		}
		if hooks.completed != 0 {
			t.Errorf("mock completion hooks=%d", hooks.completed)
		}
	}
}

func TestRunEmptyAndMissingToolResponsesAreIncomplete(t *testing.T) {
	for _, response := range []*llm.Response{{}, {FinishReason: "tool_calls"}} {
		for _, streaming := range []bool{false, true} {
			r, _, completed, err := runOutcome(t, streaming, response, false)
			if err != nil {
				t.Fatal(err)
			}
			if r.State != run.StateIncomplete || r.StopReason != run.StopInvalidResponse || completed != 0 {
				t.Errorf("malformed response completed: %+v", r)
			}
		}
	}
}

func TestStreamPrematureEOFIsIncomplete(t *testing.T) {
	r, _, completed, err := runOutcome(t, true, &llm.Response{Content: "partial"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != run.StateIncomplete || r.StopReason != run.StopProviderUnknown || completed != 0 {
		t.Errorf("premature EOF completed: %+v", r)
	}
}

func TestOrchestrationAdapterRejectsIncompleteWorker(t *testing.T) {
	e, err := New(WithStore(scopespy.New()), WithLLM(&outcomeClient{response: &llm.Response{Content: "partial", FinishReason: "length"}}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := (agentRunnerAdapter{eng: e}).RunAgent(cortex.WithScope(context.Background(), scopeA()), "assistant", "work", nil)
	if !errors.Is(err, cortex.ErrRunIncomplete) {
		t.Fatalf("incomplete worker = %+v, %v; want ErrRunIncomplete", result, err)
	}
	if result == nil || result.RunID.IsNil() || !errors.Is(result.Err, cortex.ErrRunIncomplete) {
		t.Fatalf("incomplete worker lost its execution receipt: %+v", result)
	}
}
