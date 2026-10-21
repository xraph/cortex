package optimization_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store/memory"
	"github.com/xraph/cortex/optimization/store/storetest"
)

type meterAuthorizer func(context.Context, optimization.Action, optimization.Resource) error

func (f meterAuthorizer) Authorize(ctx context.Context, action optimization.Action, resource optimization.Resource) error {
	return f(ctx, action, resource)
}

type meterResolver func(context.Context, optimization.IdentityRef, cortex.Scope) (context.Context, error)

func (f meterResolver) Resolve(ctx context.Context, actor optimization.IdentityRef, scope cortex.Scope) (context.Context, error) {
	return f(ctx, actor, scope)
}

func meterFixture(t *testing.T) (*memory.Store, context.Context, optimization.UsageReservation, optimization.IdentityRef) {
	t.Helper()
	s, err := memory.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	bundle := storetest.Fixture(t, "meter")
	bundle.Experiment.Spec.Budget = optimization.Budget{MaxTokens: 100, TokenOnly: true}
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	var lease optimization.WorkLease
	if err = s.Transact(ctx, func(tx optimization.Transaction) error {
		if createErr := tx.CreateExperiment(bundle); createErr != nil {
			return createErr
		}
		if queueErr := tx.CommitTransition(optimization.Transition{ExperimentID: bundle.Experiment.ID, ExpectedRevision: 1, NextState: optimization.StateQueued, IdempotencyKey: "start", Actor: bundle.Experiment.Spec.Requester}); queueErr != nil {
			return queueErr
		}
		var claimErr error
		lease, claimErr = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: bundle.Experiment.ID, Owner: "worker", Duration: time.Minute})
		return claimErr
	}); err != nil {
		t.Fatal(err)
	}
	return s, ctx, optimization.UsageReservation{ExperimentID: bundle.Experiment.ID, AttemptID: id.New(optimization.PrefixAttempt), CallKey: "model/call", Operation: optimization.OperationProposal, MaxTokens: 60, Lease: lease, ExpiresAt: lease.ExpiresAt}, bundle.Experiment.Spec.Requester
}

func TestLedgerMeterPersistsBeforeCallAndRetainsUnknownDebit(t *testing.T) {
	s, ctx, req, actor := meterFixture(t)
	resolved := false
	meter, err := optimization.NewLedgerMeter(s, meterAuthorizer(func(_ context.Context, action optimization.Action, resource optimization.Resource) error {
		if action != optimization.ActionStart || resource.ID != req.ExperimentID || resource.Kind != "experiment" {
			t.Fatal("wrong authorization resource")
		}
		return nil
	}), meterResolver(func(ctx context.Context, got optimization.IdentityRef, scope cortex.Scope) (context.Context, error) {
		if got != actor || scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
			t.Fatal("wrong original identity")
		}
		resolved = true
		return ctx, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	debit, err := meter.BeginCall(ctx, req, strings.Repeat("a", 64))
	if err != nil || debit.Call == nil || !resolved {
		t.Fatalf("dispatch: %+v %v", debit, err)
	}
	stored, err := s.GetReservation(ctx, req.ExperimentID, req.CallKey)
	if err != nil || stored.Call == nil || stored.Call.RequestDigest != strings.Repeat("a", 64) || stored.DebitedTokens != 60 {
		t.Fatalf("dispatch missing durable debit: %+v %v", stored, err)
	}
	if _, err = meter.BeginCall(ctx, req, strings.Repeat("a", 64)); !errors.Is(err, optimization.ErrUnknownAttempt) {
		t.Fatalf("duplicate dispatched: %v", err)
	}
	unknown := optimization.UsageSettlement{ExperimentID: req.ExperimentID, AttemptID: req.AttemptID, CallKey: req.CallKey, Receipt: "timeout-observation", Lease: req.Lease}
	if err = meter.Settle(ctx, unknown); err != nil {
		t.Fatal(err)
	}
	other := req
	other.CallKey = "next"
	other.MaxTokens = 41
	if _, err = meter.Reserve(ctx, other); !errors.Is(err, optimization.ErrBudgetExceeded) {
		t.Fatalf("unknown debit refunded: %v", err)
	}
	known := unknown
	known.Receipt = "provider/reconciled"
	known.Complete = true
	known.Usage = optimization.Usage{Tokens: 20, TokenQuality: optimization.UsageKnown}
	for range 2 {
		if err = meter.Settle(ctx, known); err != nil {
			t.Fatal(err)
		}
	}
	other.MaxTokens = 80
	if _, err = meter.Reserve(ctx, other); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerMeterRevokedIdentityCannotReserveOrDispatch(t *testing.T) {
	s, ctx, req, _ := meterFixture(t)
	denied := errors.New("identity revoked")
	meter, err := optimization.NewLedgerMeter(s, meterAuthorizer(func(context.Context, optimization.Action, optimization.Resource) error { return nil }), meterResolver(func(context.Context, optimization.IdentityRef, cortex.Scope) (context.Context, error) {
		return nil, denied
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = meter.Reserve(ctx, req); !errors.Is(err, denied) {
		t.Fatalf("revoked reservation: %v", err)
	}
	if _, err = meter.BeginCall(ctx, req, strings.Repeat("a", 64)); !errors.Is(err, denied) {
		t.Fatalf("revoked dispatch: %v", err)
	}
	if _, err = s.GetReservation(ctx, req.ExperimentID, req.CallKey); !errors.Is(err, optimization.ErrNotFound) {
		t.Fatalf("denied request wrote debit: %v", err)
	}
}

func TestLedgerMeterResolverCannotDropDeadline(t *testing.T) {
	s, ctx, req, _ := meterFixture(t)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	deadline, _ := ctx.Deadline()
	meter, err := optimization.NewLedgerMeter(s, meterAuthorizer(func(ctx context.Context, _ optimization.Action, _ optimization.Resource) error {
		got, ok := ctx.Deadline()
		if !ok || got.After(deadline) {
			t.Fatal("resolver extended operation deadline")
		}
		return nil
	}), meterResolver(func(_ context.Context, _ optimization.IdentityRef, scope cortex.Scope) (context.Context, error) {
		return cortex.WithScope(context.Background(), scope), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = meter.BeginCall(ctx, req, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
}

type afterMeterCommit struct {
	optimization.MeterRepository
	after func()
}

func (s afterMeterCommit) Transact(ctx context.Context, fn func(optimization.Transaction) error) error {
	if err := s.MeterRepository.Transact(ctx, fn); err != nil {
		return err
	}
	s.after()
	return nil
}

func TestLedgerMeterCancellationAfterCommitDoesNotPermitDispatch(t *testing.T) {
	s, ctx, req, _ := meterFixture(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	metadata := afterMeterCommit{MeterRepository: s, after: cancel}
	meter, err := optimization.NewLedgerMeter(metadata, meterAuthorizer(func(context.Context, optimization.Action, optimization.Resource) error { return nil }), meterResolver(func(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
		return ctx, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	value, err := meter.BeginCall(ctx, req, strings.Repeat("a", 64))
	if !errors.Is(err, context.Canceled) || value.Call != nil {
		t.Fatalf("cancelled call authorized dispatch: %+v %v", value, err)
	}
	readCtx := cortex.WithScope(context.Background(), cortex.ScopeFromContext(ctx))
	saved, err := s.GetReservation(readCtx, req.ExperimentID, req.CallKey)
	if err != nil || saved.Call == nil || saved.DebitedTokens != 60 {
		t.Fatalf("cancelled commit lost conservative debit: %+v %v", saved, err)
	}
}

func TestLedgerMeterChecksResolvedGrants(t *testing.T) {
	s, ctx, req, _ := meterFixture(t)
	type requesterKey struct{}
	denied := errors.New("requester cannot start")
	meter, err := optimization.NewLedgerMeter(s, meterAuthorizer(func(ctx context.Context, _ optimization.Action, _ optimization.Resource) error {
		if ctx.Value(requesterKey{}) != nil {
			return denied
		}
		return nil
	}), meterResolver(func(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
		return context.WithValue(ctx, requesterKey{}, true), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = meter.BeginCall(ctx, req, strings.Repeat("a", 64)); !errors.Is(err, denied) {
		t.Fatalf("worker grants replaced requester grants: %v", err)
	}
	if _, err = s.GetReservation(ctx, req.ExperimentID, req.CallKey); !errors.Is(err, optimization.ErrNotFound) {
		t.Fatalf("denied requester debited budget: %v", err)
	}
}

func TestLedgerMeterRejectsMissingDependenciesAndScope(t *testing.T) {
	s, ctx, req, _ := meterFixture(t)
	auth := meterAuthorizer(func(context.Context, optimization.Action, optimization.Resource) error { return nil })
	resolver := meterResolver(func(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
		return ctx, nil
	})
	for _, args := range []struct {
		repository optimization.MeterRepository
		authorizer optimization.Authorizer
		resolver   optimization.IdentityResolver
	}{{nil, auth, resolver}, {s, nil, resolver}, {s, auth, nil}} {
		if _, err := optimization.NewLedgerMeter(args.repository, args.authorizer, args.resolver); !errors.Is(err, optimization.ErrInvalidSpec) {
			t.Fatalf("missing dependency accepted: %v", err)
		}
	}
	meter, err := optimization.NewLedgerMeter(s, auth, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = meter.BeginCall(context.Background(), req, strings.Repeat("a", 64)); !errors.Is(err, cortex.ErrNoScope) {
		t.Fatalf("unscoped dispatch: %v", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = meter.Reserve(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reservation: %v", err)
	}
}
