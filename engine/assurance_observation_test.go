package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/store/scopespy"
	"github.com/xraph/cortex/verification"
)

type terminationObserver struct{ summaries []run.TerminalSummary }

func (*terminationObserver) Name() string { return "termination-observer" }
func (h *terminationObserver) OnRunTerminated(_ context.Context, _ id.AgentID, _ id.AgentRunID, s run.TerminalSummary) error {
	h.summaries = append(h.summaries, s)
	return nil
}

func TestTerminalObservationRequiresDurableOutcome(t *testing.T) {
	for _, fail := range []bool{false, true} {
		st := &outcomeStore{Spy: scopespy.New(), failUpdate: fail}
		hook := &terminationObserver{}
		e, err := New(WithStore(st), WithExtension(hook))
		if err != nil {
			t.Fatal(err)
		}
		ctx := cortex.WithScope(context.Background(), scopeA())
		r := &run.Run{ID: id.NewAgentRunID(), AgentID: id.NewAgentID(), State: run.StateRunning}
		if err = st.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		err = e.finalizeRun(ctx, r, run.Outcome{StopReason: run.StopStepLimit})
		if fail {
			if err == nil || len(hook.summaries) != 0 {
				t.Fatal("uncommitted outcome observed")
			}
			continue
		}
		if err != nil || len(hook.summaries) != 1 {
			t.Fatalf("missing outcome: %v %v", hook.summaries, err)
		}
		summary := hook.summaries[0]
		if summary.State != run.StateIncomplete || summary.StopReason != run.StopStepLimit || summary.VerificationStatus != verification.StatusUnchecked {
			t.Fatalf("wrong summary: %+v", summary)
		}
		if err = e.finalizeRun(ctx, r, run.Outcome{StopReason: run.StopStepLimit}); err != nil || len(hook.summaries) != 1 {
			t.Fatal("duplicate terminal observation")
		}
	}
}

func TestRuntimeCapabilitiesDoNotClaimVerification(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatal(err)
	}
	caps := e.RuntimeCapabilities()
	if !caps.PreparedExecution || caps.ContextualVerification || caps.AtomicSourceSnapshot || caps.PublicationGateInstalled {
		t.Fatalf("unsupported capability claimed: %+v", caps)
	}
	if caps.PublicationMode != "buffered" {
		t.Fatalf("wrong publication mode: %+v", caps)
	}
}

type rejectionObserver struct{ reasons []run.StopReason }

func (*rejectionObserver) Name() string { return "rejection-observer" }
func (h *rejectionObserver) OnPublicationRejected(_ context.Context, _ id.AgentID, _ id.AgentRunID, reason run.StopReason) error {
	h.reasons = append(h.reasons, reason)
	return nil
}

type unavailableObservationGate struct{}

func (unavailableObservationGate) Check(context.Context, safety.PublicationRequest) (safety.PublicationDecision, error) {
	return safety.PublicationDecision{}, errors.New("private scanner failure")
}

func TestPublicationFailureBeforeRunIsObservableWithoutContent(t *testing.T) {
	hook := &rejectionObserver{}
	e, err := New(WithStore(scopespy.New()), WithPublicationGate(unavailableObservationGate{}), WithExtension(hook))
	if err != nil {
		t.Fatal(err)
	}
	ctx := cortex.WithScope(context.Background(), scopeA())
	_, err = e.RunAgent(ctx, "assistant", "private input", nil)
	if !errors.Is(err, safety.ErrUnavailable) || len(hook.reasons) != 1 || hook.reasons[0] != run.StopPolicyUnavailable {
		t.Fatalf("policy failure was not observed: %v %v", hook.reasons, err)
	}
}
