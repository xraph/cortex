package optimization_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/store/scopespy"
)

type interceptedMeter struct {
	optimization.CallMeter
	begin     func(optimization.Reservation) optimization.Reservation
	settleErr error
}

func (m interceptedMeter) BeginCall(ctx context.Context, req optimization.UsageReservation, digest string) (optimization.Reservation, error) {
	saved, err := m.CallMeter.BeginCall(ctx, req, digest)
	if err == nil && m.begin != nil {
		saved = m.begin(saved)
	}
	return saved, err
}

func (m interceptedMeter) Settle(ctx context.Context, settlement optimization.UsageSettlement) error {
	if m.settleErr != nil {
		return m.settleErr
	}
	return m.CallMeter.Settle(ctx, settlement)
}

func allowModelMeter(t *testing.T, repo optimization.MeterRepository) optimization.CallMeter {
	t.Helper()
	meter, err := optimization.NewLedgerMeter(repo, meterAuthorizer(func(context.Context, optimization.Action, optimization.Resource) error { return nil }), meterResolver(func(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
		return ctx, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return meter
}

func TestMeteredRejectsInvalidRequestBeforeDispatch(t *testing.T) {
	for _, mode := range []string{"scope", "no-scope", "cancelled", "model", "output-limit", "request-limit", "estimate-operation", "estimate-bound", "estimate-expiry", "invalid-json"} {
		t.Run(mode, func(t *testing.T) {
			s, ctx, reservation, _ := meterFixture(t)
			config := modelConfig(ctx, reservation)
			req := &llm.Request{Model: "fixture", MaxTokens: 10}
			callCtx := ctx
			switch mode {
			case "scope":
				callCtx = cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "foreign"}}})
			case "no-scope":
				callCtx = context.Background()
			case "cancelled":
				var cancel context.CancelFunc
				callCtx, cancel = context.WithCancel(ctx)
				cancel()
			case "model":
				req.Model = "another"
			case "output-limit":
				req.MaxTokens = 0
			case "request-limit":
				req.System = strings.Repeat("x", 5000)
			case "estimate-operation":
				reservation.Operation = optimization.OperationJudge
			case "estimate-bound":
				reservation.MaxTokens = 1
			case "estimate-expiry":
				reservation.ExpiresAt = time.Now().Add(-time.Hour)
			case "invalid-json":
				req.Tools = []llm.Tool{{Parameters: make(chan int)}}
			}
			client, err := optimization.NewMeteredClient(modelClient{complete: func(context.Context, *llm.Request) (*llm.Response, error) {
				t.Fatal("invalid request dispatched")
				return nil, nil
			}}, allowModelMeter(t, s), modelEstimator(func(context.Context, *llm.Request) (optimization.UsageReservation, error) { return reservation, nil }), nil, config)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = client.Complete(callCtx, req); err == nil {
				t.Fatal("invalid input accepted")
			}
			if _, err = s.GetReservation(ctx, reservation.ExperimentID, reservation.CallKey); !errors.Is(err, optimization.ErrNotFound) {
				t.Fatalf("invalid input reserved usage: %v", err)
			}
		})
	}
}

func TestMeteredRejectsMismatchedDispatchLease(t *testing.T) {
	s, ctx, reservation, _ := meterFixture(t)
	meter := interceptedMeter{CallMeter: allowModelMeter(t, s), begin: func(saved optimization.Reservation) optimization.Reservation {
		saved.Call.Lease.Generation++
		return saved
	}}
	client, err := optimization.NewMeteredClient(modelClient{complete: func(context.Context, *llm.Request) (*llm.Response, error) {
		t.Fatal("foreign dispatch authority reached provider")
		return nil, nil
	}}, meter, modelEstimator(func(context.Context, *llm.Request) (optimization.UsageReservation, error) { return reservation, nil }), nil, modelConfig(ctx, reservation))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Complete(ctx, &llm.Request{Model: "fixture", MaxTokens: 10}); !errors.Is(err, optimization.ErrIncompleteEvidence) {
		t.Fatalf("mismatched lease: %v", err)
	}
}

func TestMeteredStreamAccountingEOFIsFailure(t *testing.T) {
	s, ctx, reservation, _ := meterFixture(t)
	meter := interceptedMeter{CallMeter: allowModelMeter(t, s), settleErr: io.EOF}
	client, err := optimization.NewMeteredClient(modelClient{stream: func(context.Context, *llm.Request) (llm.Stream, error) { return &modelStream{nextErr: io.EOF}, nil }}, meter, modelEstimator(func(context.Context, *llm.Request) (optimization.UsageReservation, error) { return reservation, nil }), nil, modelConfig(ctx, reservation))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.CompleteStream(ctx, &llm.Request{Model: "fixture", MaxTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Next(ctx); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("accounting failure treated as completion: %v", err)
	}
	if _, err = client.CompleteStream(ctx, &llm.Request{Model: "fixture", MaxTokens: 10}); !errors.Is(err, optimization.ErrUnknownAttempt) {
		t.Fatalf("failed settlement permitted replay: %v", err)
	}
}

type blockedModelStream struct {
	started chan struct{}
	closed  int
}

func (s *blockedModelStream) Next(ctx context.Context) (*llm.Chunk, error) {
	close(s.started)
	<-ctx.Done()
	return nil, ctx.Err()
}
func (s *blockedModelStream) Close() error    { s.closed++; return nil }
func (*blockedModelStream) Usage() *llm.Usage { return nil }

func TestMeteredConcurrentCloseCancelsNextAndSettlesOnce(t *testing.T) {
	s, ctx, reservation, _ := meterFixture(t)
	upstream := &blockedModelStream{started: make(chan struct{})}
	client, err := optimization.NewMeteredClient(modelClient{stream: func(context.Context, *llm.Request) (llm.Stream, error) { return upstream, nil }}, allowModelMeter(t, s), modelEstimator(func(context.Context, *llm.Request) (optimization.UsageReservation, error) { return reservation, nil }), nil, modelConfig(ctx, reservation))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.CompleteStream(ctx, &llm.Request{Model: "fixture", MaxTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, nextErr := stream.Next(ctx); done <- nextErr }()
	<-upstream.started
	if err = stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Next did not observe close: %v", err)
	}
	if err = stream.Close(); err != nil {
		t.Fatal(err)
	}
	saved, err := s.GetReservation(ctx, reservation.ExperimentID, reservation.CallKey)
	if err != nil || upstream.closed != 1 || len(saved.History) != 1 || saved.DebitedTokens != 60 {
		t.Fatalf("close lost accounting or repeated it: closed=%d %+v %v", upstream.closed, saved, err)
	}
}

func TestMeteredRequestChangesCannotReuseDispatch(t *testing.T) {
	for _, change := range []string{"message", "tool", "limit", "revision", "stream"} {
		t.Run(change, func(t *testing.T) {
			s, ctx, reservation, _ := meterFixture(t)
			calls := 0
			provider := modelClient{complete: func(context.Context, *llm.Request) (*llm.Response, error) {
				calls++
				return &llm.Response{Content: "answer"}, nil
			}, stream: func(context.Context, *llm.Request) (llm.Stream, error) { calls++; return nil, nil }}
			config := modelConfig(ctx, reservation)
			meter := allowModelMeter(t, s)
			estimator := modelEstimator(func(context.Context, *llm.Request) (optimization.UsageReservation, error) { return reservation, nil })
			client, err := optimization.NewMeteredClient(provider, meter, estimator, nil, config)
			if err != nil {
				t.Fatal(err)
			}
			req := &llm.Request{Model: "fixture", MaxTokens: 10}
			if _, err = client.Complete(ctx, req); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "message":
				req.Messages = []llm.Message{{Role: "user", Content: "changed"}}
			case "tool":
				req.Tools = []llm.Tool{{Name: "read", Parameters: map[string]any{"type": "object"}}}
			case "limit":
				req.MaxTokens++
			case "revision":
				config.ModelRevision = "2"
				client, err = optimization.NewMeteredClient(provider, meter, estimator, nil, config)
				if err != nil {
					t.Fatal(err)
				}
			}
			if change == "stream" {
				_, err = client.CompleteStream(ctx, req)
			} else {
				_, err = client.Complete(ctx, req)
			}
			if !errors.Is(err, optimization.ErrConflict) || calls != 1 {
				t.Fatalf("changed request dispatched: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestMeteredInvalidUsageCannotReleaseDebit(t *testing.T) {
	for _, mode := range []string{"underreported", "negative", "inconsistent", "wrong-model", "no-receipt", "wrong-price"} {
		t.Run(mode, func(t *testing.T) {
			budget := optimization.Budget{MaxTokens: 100, TokenOnly: true}
			if mode == "wrong-price" {
				budget = optimization.Budget{MaxTokens: 100, MaxUSDMicros: 100, PricingRevision: "frozen"}
			}
			s, ctx, reservation, _ := meterFixtureWithBudget(t, budget)
			usage := llm.Usage{TotalTokens: 20}
			model := "fixture"
			report := optimization.ModelUsageReport{Usage: optimization.Usage{Tokens: 20, TokenQuality: optimization.UsageKnown, Receipt: "provider/receipt"}, Complete: true}
			switch mode {
			case "underreported":
				report.Usage.Tokens = 1
			case "negative":
				usage.PromptTokens = -1
			case "inconsistent":
				usage.PromptTokens = 120
			case "wrong-model":
				model = "different"
			case "no-receipt":
				report.Usage.Receipt = ""
			case "wrong-price":
				reservation.PricingRevision = "frozen"
				report.Usage.MoneyQuality = optimization.UsageKnown
				report.Usage.PricingRevision = "another"
			}
			client, err := optimization.NewMeteredClient(modelClient{complete: func(context.Context, *llm.Request) (*llm.Response, error) {
				return &llm.Response{Content: "answer", Model: model, Usage: usage}, nil
			}}, allowModelMeter(t, s), modelEstimator(func(context.Context, *llm.Request) (optimization.UsageReservation, error) { return reservation, nil }), modelReporter(func(context.Context, optimization.ModelObservation) (optimization.ModelUsageReport, error) {
				return report, nil
			}), modelConfig(ctx, reservation))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = client.Complete(ctx, &llm.Request{Model: "fixture", MaxTokens: 10}); !errors.Is(err, optimization.ErrIncompleteEvidence) {
				t.Fatalf("invalid usage accepted: %v", err)
			}
			want := int64(60)
			if mode == "inconsistent" {
				want = 120
			}
			saved, err := s.GetReservation(ctx, reservation.ExperimentID, reservation.CallKey)
			if err != nil || saved.State != optimization.ReservationUnknown || saved.DebitedTokens != want {
				t.Fatalf("invalid report released usage: %+v %v", saved, err)
			}
		})
	}
}

func TestMeteredFailedOpenCapturesUsageOnClose(t *testing.T) {
	s, ctx, reservation, _ := meterFixture(t)
	upstream := &modelStream{closeUsage: &llm.Usage{TotalTokens: 120}}
	failed := errors.New("failed open")
	client, err := optimization.NewMeteredClient(modelClient{stream: func(context.Context, *llm.Request) (llm.Stream, error) { return upstream, failed }}, allowModelMeter(t, s), modelEstimator(func(context.Context, *llm.Request) (optimization.UsageReservation, error) { return reservation, nil }), nil, modelConfig(ctx, reservation))
	if err != nil {
		t.Fatal(err)
	}
	if stream, openErr := client.CompleteStream(ctx, &llm.Request{Model: "fixture", MaxTokens: 10}); stream != nil || !errors.Is(openErr, failed) {
		t.Fatalf("failed open: %v %v", stream, openErr)
	}
	saved, err := s.GetReservation(ctx, reservation.ExperimentID, reservation.CallKey)
	if err != nil || saved.DebitedTokens != 120 || upstream.closed != 1 {
		t.Fatalf("close usage lost: %+v %v", saved, err)
	}
}

func TestMeteredEngineStreamReportsAccountingFailure(t *testing.T) {
	s, ctx, reservation, _ := meterFixture(t)
	failed := errors.New("accounting unavailable")
	client, err := optimization.NewMeteredClient(modelClient{stream: func(context.Context, *llm.Request) (llm.Stream, error) { return &modelStream{}, nil }}, interceptedMeter{CallMeter: allowModelMeter(t, s), settleErr: failed}, modelEstimator(func(context.Context, *llm.Request) (optimization.UsageReservation, error) { return reservation, nil }), nil, modelConfig(ctx, reservation))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := engine.New(engine.WithStore(scopespy.New()), engine.WithLLM(client))
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan engine.StreamEvent, 64)
	if err = runtime.StreamAgent(ctx, "assistant", "hello", &engine.RunOverrides{Model: "fixture", MaxTokens: 10}, events); err != nil {
		t.Fatal(err)
	}
	sawFailure := false
	for event := range events {
		if event.Type == engine.EventDone {
			t.Fatal("engine reported success after accounting failure")
		}
		if event.Type == engine.EventError {
			sawFailure = true
		}
	}
	if !sawFailure {
		t.Fatal("engine did not surface stream failure")
	}
	saved, err := s.GetReservation(ctx, reservation.ExperimentID, reservation.CallKey)
	if err != nil || saved.Call == nil || saved.DebitedTokens != 60 {
		t.Fatalf("failed engine run lost dispatch debit: %+v %v", saved, err)
	}
}
