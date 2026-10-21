package optimization_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/optimization"
)

type modelClient struct {
	complete func(context.Context, *llm.Request) (*llm.Response, error)
	stream   func(context.Context, *llm.Request) (llm.Stream, error)
}

func (c modelClient) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	return c.complete(ctx, req)
}
func (c modelClient) CompleteStream(ctx context.Context, req *llm.Request) (llm.Stream, error) {
	return c.stream(ctx, req)
}

type modelEstimator func(context.Context, *llm.Request) (optimization.UsageReservation, error)

func (f modelEstimator) Estimate(ctx context.Context, req *llm.Request) (optimization.UsageReservation, error) {
	return f(ctx, req)
}

type modelReporter func(context.Context, optimization.ModelObservation) (optimization.ModelUsageReport, error)

func (f modelReporter) Observe(ctx context.Context, value optimization.ModelObservation) (optimization.ModelUsageReport, error) {
	return f(ctx, value)
}

func modelConfig(ctx context.Context, req optimization.UsageReservation) optimization.MeteredClientConfig {
	return optimization.MeteredClientConfig{Scope: cortex.ScopeFromContext(ctx), ExperimentID: req.ExperimentID, AttemptID: req.AttemptID, Operation: req.Operation, Model: "fixture", ModelRevision: "1", MaxRequestBytes: 4096, MaxResponseBytes: 4096}
}

func TestMeteredCompleteReservesBeforeProviderAndSettlesOnce(t *testing.T) {
	s, ctx, reservation, _ := meterFixture(t)
	meter, err := optimization.NewLedgerMeter(s, meterAuthorizer(func(context.Context, optimization.Action, optimization.Resource) error { return nil }), meterResolver(func(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
		return ctx, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	provider := modelClient{complete: func(ctx context.Context, req *llm.Request) (*llm.Response, error) {
		calls++
		saved, getErr := s.GetReservation(ctx, reservation.ExperimentID, reservation.CallKey)
		if getErr != nil || saved.Call == nil || saved.DebitedTokens != 60 {
			t.Fatalf("provider started before durable debit: %+v %v", saved, getErr)
		}
		if req.System != "original" || req.MaxTokens != 10 {
			t.Fatal("estimator changed dispatched request")
		}
		return &llm.Response{Content: "answer", Model: "fixture", Usage: llm.Usage{TotalTokens: 20}}, nil
	}}
	reporter := modelReporter(func(_ context.Context, observation optimization.ModelObservation) (optimization.ModelUsageReport, error) {
		if !observation.Completed || observation.RequestDigest == "" || observation.Reservation.CallKey != reservation.CallKey || observation.Usage.TotalTokens != 20 {
			t.Fatalf("wrong observation: %+v", observation)
		}
		return optimization.ModelUsageReport{Usage: optimization.Usage{Tokens: 20, TokenQuality: optimization.UsageKnown, Receipt: "provider/receipt"}, Complete: true}, nil
	})
	client, err := optimization.NewMeteredClient(provider, meter, modelEstimator(func(_ context.Context, req *llm.Request) (optimization.UsageReservation, error) {
		req.System = "mutated"
		return reservation, nil
	}), reporter, modelConfig(ctx, reservation))
	if err != nil {
		t.Fatal(err)
	}
	request := &llm.Request{Model: "fixture", System: "original", MaxTokens: 10}
	response, err := client.Complete(ctx, request)
	if err != nil || response == nil || response.Content != "answer" || request.System != "original" {
		t.Fatalf("completion: %+v %v", response, err)
	}
	saved, err := s.GetReservation(ctx, reservation.ExperimentID, reservation.CallKey)
	if err != nil || saved.State != optimization.ReservationSettled || saved.DebitedTokens != 20 || len(saved.History) != 1 {
		t.Fatalf("settlement: %+v %v", saved, err)
	}
	if _, err = client.Complete(ctx, request); !errors.Is(err, optimization.ErrUnknownAttempt) || calls != 1 {
		t.Fatalf("duplicate reached provider: calls=%d err=%v", calls, err)
	}
}

func TestMeteredCompleteFailureAndUnreportedUsageKeepDebit(t *testing.T) {
	for _, mode := range []string{"failure", "missing", "overrun"} {
		t.Run(mode, func(t *testing.T) {
			s, ctx, reservation, _ := meterFixture(t)
			meter, err := optimization.NewLedgerMeter(s, meterAuthorizer(func(context.Context, optimization.Action, optimization.Resource) error { return nil }), meterResolver(func(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
				return ctx, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			failed := errors.New("provider timeout")
			provider := modelClient{complete: func(context.Context, *llm.Request) (*llm.Response, error) {
				if mode == "failure" {
					return nil, failed
				}
				usage := llm.Usage{}
				if mode == "overrun" {
					usage.TotalTokens = 120
				}
				return &llm.Response{Content: "answer", Model: "fixture", Usage: usage}, nil
			}}
			client, err := optimization.NewMeteredClient(provider, meter, modelEstimator(func(context.Context, *llm.Request) (optimization.UsageReservation, error) { return reservation, nil }), nil, modelConfig(ctx, reservation))
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Complete(ctx, &llm.Request{Model: "fixture", MaxTokens: 10})
			if mode == "failure" && !errors.Is(err, failed) {
				t.Fatalf("provider error lost: %v", err)
			}
			if mode != "failure" && err != nil {
				t.Fatal(err)
			}
			saved, getErr := s.GetReservation(ctx, reservation.ExperimentID, reservation.CallKey)
			want := int64(60)
			if mode == "overrun" {
				want = 120
			}
			if getErr != nil || saved.State != optimization.ReservationUnknown || saved.DebitedTokens != want || saved.Call == nil {
				t.Fatalf("uncertain usage released debit: %+v %v", saved, getErr)
			}
		})
	}
}

type modelStream struct {
	steps      int
	closed     int
	usage      *llm.Usage
	closeUsage *llm.Usage
	nextErr    error
	chunk      *llm.Chunk
}

func (s *modelStream) Next(context.Context) (*llm.Chunk, error) {
	s.steps++
	if s.nextErr != nil {
		return s.chunk, s.nextErr
	}
	if s.steps == 1 {
		return &llm.Chunk{Content: "answer"}, nil
	}
	return nil, io.EOF
}
func (s *modelStream) Close() error {
	s.closed++
	if s.closeUsage != nil {
		s.usage = s.closeUsage
	}
	return nil
}
func (s *modelStream) Usage() *llm.Usage { return s.usage }

func TestMeteredStreamFailuresCannotBecomeEOF(t *testing.T) {
	for _, mode := range []string{"settlement", "oversized-final", "cancelled-next", "late-usage"} {
		t.Run(mode, func(t *testing.T) {
			s, ctx, reservation, _ := meterFixture(t)
			meter, err := optimization.NewLedgerMeter(s, meterAuthorizer(func(context.Context, optimization.Action, optimization.Resource) error { return nil }), meterResolver(func(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
				return ctx, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			upstream := &modelStream{nextErr: io.EOF, usage: &llm.Usage{TotalTokens: 20}}
			failed := errors.New("usage verification failed")
			var reporter optimization.ModelUsageReporter
			if mode == "settlement" {
				reporter = modelReporter(func(context.Context, optimization.ModelObservation) (optimization.ModelUsageReport, error) {
					return optimization.ModelUsageReport{}, failed
				})
			}
			if mode == "oversized-final" {
				upstream.chunk = &llm.Chunk{Content: strings.Repeat("x", 5000)}
			}
			if mode == "late-usage" {
				upstream.closeUsage = &llm.Usage{TotalTokens: 120}
			}
			client, err := optimization.NewMeteredClient(modelClient{stream: func(context.Context, *llm.Request) (llm.Stream, error) { return upstream, nil }}, meter, modelEstimator(func(context.Context, *llm.Request) (optimization.UsageReservation, error) { return reservation, nil }), reporter, modelConfig(ctx, reservation))
			if err != nil {
				t.Fatal(err)
			}
			stream, err := client.CompleteStream(ctx, &llm.Request{Model: "fixture", MaxTokens: 10})
			if err != nil {
				t.Fatal(err)
			}
			nextCtx := ctx
			if mode == "cancelled-next" {
				var cancel context.CancelFunc
				nextCtx, cancel = context.WithCancel(ctx)
				cancel()
			}
			_, err = stream.Next(nextCtx)
			if mode == "late-usage" {
				if !errors.Is(err, io.EOF) {
					t.Fatalf("clean EOF: %v", err)
				}
			} else if err == nil || errors.Is(err, io.EOF) {
				t.Fatalf("failure became EOF: %v", err)
			}
			if mode == "settlement" && !errors.Is(err, failed) {
				t.Fatalf("lost settlement failure: %v", err)
			}
			if upstream.closed != 1 {
				t.Fatalf("source closed %d times", upstream.closed)
			}
			saved, getErr := s.GetReservation(ctx, reservation.ExperimentID, reservation.CallKey)
			want := int64(60)
			if mode == "late-usage" {
				want = 120
			}
			if getErr != nil || saved.State != optimization.ReservationUnknown || saved.DebitedTokens != want {
				t.Fatalf("wrong debit: %+v %v", saved, getErr)
			}
		})
	}
}

func TestMeteredStreamSettlesAtEOFAndClosesOnce(t *testing.T) {
	s, ctx, reservation, _ := meterFixture(t)
	meter, err := optimization.NewLedgerMeter(s, meterAuthorizer(func(context.Context, optimization.Action, optimization.Resource) error { return nil }), meterResolver(func(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
		return ctx, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	upstream := &modelStream{usage: &llm.Usage{TotalTokens: 20}}
	provider := modelClient{stream: func(ctx context.Context, _ *llm.Request) (llm.Stream, error) {
		saved, getErr := s.GetReservation(ctx, reservation.ExperimentID, reservation.CallKey)
		if getErr != nil || saved.Call == nil {
			t.Fatal("stream opened before debit")
		}
		return upstream, nil
	}}
	reporter := modelReporter(func(_ context.Context, observation optimization.ModelObservation) (optimization.ModelUsageReport, error) {
		if !observation.Completed {
			t.Fatal("EOF observation incomplete")
		}
		return optimization.ModelUsageReport{Usage: optimization.Usage{Tokens: 20, TokenQuality: optimization.UsageKnown, Receipt: "provider/stream"}, Complete: true}, nil
	})
	client, err := optimization.NewMeteredClient(provider, meter, modelEstimator(func(context.Context, *llm.Request) (optimization.UsageReservation, error) { return reservation, nil }), reporter, modelConfig(ctx, reservation))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.CompleteStream(ctx, &llm.Request{Model: "fixture", MaxTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := stream.Next(ctx)
	if err != nil || chunk.Content != "answer" {
		t.Fatalf("chunk: %+v %v", chunk, err)
	}
	if _, err = stream.Next(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("end: %v", err)
	}
	for range 2 {
		if err = stream.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if upstream.closed != 1 {
		t.Fatalf("upstream closed %d times", upstream.closed)
	}
	usage := stream.Usage()
	usage.TotalTokens = 999
	if stream.Usage().TotalTokens != 20 {
		t.Fatal("usage pointer leaked")
	}
	saved, err := s.GetReservation(ctx, reservation.ExperimentID, reservation.CallKey)
	if err != nil || saved.DebitedTokens != 20 || len(saved.History) != 1 {
		t.Fatalf("stream settlement: %+v %v", saved, err)
	}
}
