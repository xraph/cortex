package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/xraph/cortex/optimization/store"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/optimization"
)

type discoveryReflectProvider struct {
	mode  string
	calls int
}

func (p *discoveryReflectProvider) Complete(_ context.Context, request *llm.Request) (*llm.Response, error) {
	p.calls++
	var input optimization.ReflectionInput
	if err := json.Unmarshal([]byte(request.Messages[0].Content), &input); err != nil {
		return nil, err
	}
	if p.mode == "lost" {
		return nil, context.DeadlineExceeded
	}
	lessons := []optimization.LessonDraft{}
	if p.mode != "empty" {
		for _, value := range input.Evidence {
			if value.Trial.Split != optimization.SplitDiscovery {
				return nil, optimization.ErrPolicyDenied
			}
		}
		if len(input.Evidence) == 0 {
			return nil, optimization.ErrIncompleteEvidence
		}
		lessons = append(lessons, optimization.LessonDraft{Statement: "Check the recorded outcome.", Domain: input.Domain, EvidenceIDs: []id.ID{input.Evidence[0].Trial.ID}})
	}
	body, err := json.Marshal(map[string]any{"lessons": lessons})
	if err != nil {
		return nil, err
	}
	return &llm.Response{Content: string(body), Model: "fixture"}, nil
}
func (*discoveryReflectProvider) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, optimization.ErrUnavailable
}

// DiscoveryReflection checks metered early reflection and ambiguous-answer recovery.
func DiscoveryReflection(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("WorkerPersistsDiscoveryBeforeValidation", func(t *testing.T) {
		f := discoveryFamily(t, factory)
		provider := &discoveryReflectProvider{mode: "lessons"}
		handler := discoveryReflectionHandler(t, f, provider)
		authority := controllerAuthority{actor: f.parent.Experiment.Spec.Requester}
		op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: f.s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: handler, Worker: optimization.DefaultWorkerConfig()})
		if err != nil {
			t.Fatal(err)
		}
		current, err := f.s.GetExperiment(f.ctx, f.children[0].Experiment.ID)
		if err != nil {
			t.Fatal(err)
		}
		paused, err := op.Pause(f.ctx, current.ID, current.Revision)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = op.Resume(f.ctx, current.ID, paused.Revision); err != nil {
			t.Fatal(err)
		}
		if err = op.WorkOnce(f.ctx, current.ID); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("worker did not park before holdout: %v", err)
		}
		stored, err := f.s.GetDiscoveryLessons(f.ctx, current.ID)
		if err != nil || len(stored.Lessons) != 1 || provider.calls != 1 {
			t.Fatalf("worker lost reflection: %+v %v calls=%d", stored, err, provider.calls)
		}
	})
	t.Run("ReflectionPersistenceRechecksAuthority", func(t *testing.T) {
		f := discoveryFamily(t, factory)
		current, err := f.s.GetExperiment(f.ctx, f.children[0].Experiment.ID)
		if err != nil {
			t.Fatal(err)
		}
		authority := &improvementAuthority{}
		underlying := f.s
		f.s = reflectionRevocationStore{Store: underlying, after: func() { authority.denied = current.ID }}
		provider := &discoveryReflectProvider{mode: "lessons"}
		handler := reflectionHandlerWithAuthority(t, f, provider, authority, authority)
		if _, err = handler.Execute(f.ctx, optimization.WorkItem{Experiment: current, Lease: f.leases[1]}); !errors.Is(err, optimization.ErrPolicyDenied) || provider.calls != 1 {
			t.Errorf("revocation after settlement ignored: %v calls=%d", err, provider.calls)
		}
		if _, err = underlying.GetDiscoveryLessons(f.ctx, current.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("revoked reflection persisted: %v", err)
		}
	})
	for _, mode := range []string{"lessons", "empty", "lost"} {
		t.Run("DiscoveryReflection/"+mode, func(t *testing.T) {
			f := discoveryFamily(t, factory)
			current, err := f.s.GetExperiment(f.ctx, f.children[0].Experiment.ID)
			if err != nil {
				t.Fatal(err)
			}
			provider := &discoveryReflectProvider{mode: mode}
			handler := discoveryReflectionHandler(t, f, provider)
			item := optimization.WorkItem{Experiment: current, Lease: f.leases[1]}
			_, err = handler.Execute(f.ctx, item)
			want := optimization.ErrConflict
			if mode == "lost" {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) || provider.calls != 1 {
				t.Fatalf("early reflection: %v calls=%d", err, provider.calls)
			}
			batch, getErr := f.s.GetDiscoveryLessons(f.ctx, current.ID)
			if mode == "lost" {
				if !errors.Is(getErr, optimization.ErrNotFound) {
					t.Fatalf("lost answer persisted a reflection: %v", getErr)
				}
			} else if getErr != nil || batch.Domain != "comparison" || batch.Lessons == nil || (mode == "empty" && len(batch.Lessons) != 0) || (mode == "lessons" && len(batch.Lessons) != 1) {
				t.Fatalf("reflection result: %+v %v", batch, getErr)
			}
			_, err = handler.Execute(f.ctx, item)
			want = optimization.ErrConflict
			if mode == "lost" {
				want = optimization.ErrUnknownAttempt
			}
			if !errors.Is(err, want) || provider.calls != 1 {
				t.Fatalf("reflection repeated: %v calls=%d", err, provider.calls)
			}
			usage, err := f.s.GetReservation(f.ctx, current.ID, "controller/discovery-reflect/1")
			if err != nil || usage.Call == nil || usage.DebitedTokens != 11 {
				t.Fatalf("missing reflection charge: %+v %v", usage, err)
			}
			if _, err = f.s.GetReservation(f.ctx, current.ID, "controller/reflect/1"); !errors.Is(err, optimization.ErrNotFound) {
				t.Fatalf("discovery consumed final reflection key: %v", err)
			}
			saved, err := f.s.GetExperiment(f.ctx, current.ID)
			if err != nil || saved.Phase != optimization.PhaseValidate || !saved.DecisionID.IsNil() {
				t.Fatalf("early reflection crossed validation barrier: %+v %v", saved, err)
			}
			advanceFixture(f.ctx, t, f.s, f.parent, f.leases[0], optimization.PhaseValidate)
			if _, err = handler.Execute(f.ctx, item); !errors.Is(err, optimization.ErrUnavailable) || provider.calls != 1 {
				t.Fatalf("validation reopened discovery reflection: %v calls=%d", err, provider.calls)
			}
		})
	}
}

func discoveryReflectionHandler(t *testing.T, f validationFamily, provider *discoveryReflectProvider) *optimization.ControllerPhaseHandler {
	t.Helper()
	authority := controllerAuthority{actor: f.parent.Experiment.Spec.Requester}
	return reflectionHandlerWithAuthority(t, f, provider, authority, authority)
}

func reflectionHandlerWithAuthority(t *testing.T, f validationFamily, provider *discoveryReflectProvider, auth optimization.Authorizer, resolver optimization.IdentityResolver) *optimization.ControllerPhaseHandler {
	t.Helper()
	models, err := optimization.NewControllerFactory(f.s, auth, resolver, []optimization.ModelBinding{{Model: "fixture", Revision: "1", Provider: provider, Counter: discoveryCounter{}}}, optimization.ControllerLimits{MaxInputBytes: 1 << 20, MaxResponseBytes: 4096, MaxOutputTokens: 10, MaxProposals: 2, MaxLessons: 2})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := optimization.NewControllerPhaseHandler(f.s, models, nil, "comparison")
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

// DiscoveryReflectionRecovery preserves outcomes and unknown calls across restart.
func DiscoveryReflectionRecovery(t *testing.T, open func(*testing.T, func() time.Time) store.Store) {
	t.Helper()
	for _, mode := range []string{"lessons", "lost"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().UTC()
			f := discoveryFamily(t, func(t *testing.T, _ func() time.Time) store.Store { return open(t, func() time.Time { return now }) })
			provider := &discoveryReflectProvider{mode: mode}
			current, err := f.s.GetExperiment(f.ctx, f.children[0].Experiment.ID)
			if err != nil {
				t.Fatal(err)
			}
			handler := discoveryReflectionHandler(t, f, provider)
			_, err = handler.Execute(f.ctx, optimization.WorkItem{Experiment: current, Lease: f.leases[1]})
			want := optimization.ErrConflict
			if mode == "lost" {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) {
				t.Fatal(err)
			}
			before, err := f.s.GetReservation(f.ctx, current.ID, "controller/discovery-reflect/1")
			if err != nil {
				t.Fatal(err)
			}
			prior, getErr := f.s.GetDiscoveryLessons(f.ctx, current.ID)
			if mode == "lessons" && getErr != nil {
				t.Fatal(getErr)
			}
			if err = f.s.Close(); err != nil {
				t.Fatal(err)
			}
			now = f.leases[0].ExpiresAt.Add(time.Second)
			f.s = open(t, func() time.Time { return now })
			if err = f.s.Transact(f.ctx, func(tx store.Tx) error {
				var claimErr error
				f.leases[0], claimErr = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: f.parent.Experiment.ID, Owner: "recovered-parent", Duration: time.Minute})
				if claimErr != nil {
					return claimErr
				}
				f.leases[1], claimErr = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: current.ID, Owner: "recovered-child", Duration: time.Minute})
				return claimErr
			}); err != nil {
				t.Fatal(err)
			}
			if mode == "lost" {
				if err = f.s.Transact(f.ctx, func(tx store.Tx) error {
					return tx.Settle(optimization.UsageSettlement{ExperimentID: current.ID, AttemptID: before.Request.AttemptID, CallKey: before.Request.CallKey, Receipt: "late-reflection/" + current.ID.String(), Usage: optimization.Usage{Tokens: 3, TokenQuality: optimization.UsageKnown}, Complete: true, Lease: f.leases[1]})
				}); err != nil {
					t.Fatal(err)
				}
				before, err = f.s.GetReservation(f.ctx, current.ID, before.Request.CallKey)
				if err != nil || before.DebitedTokens != 3 || before.State != optimization.ReservationSettled {
					t.Fatalf("late receipt did not settle original call: %+v %v", before, err)
				}
			}
			current, err = f.s.GetExperiment(f.ctx, current.ID)
			if err != nil {
				t.Fatal(err)
			}
			handler = discoveryReflectionHandler(t, f, provider)
			_, err = handler.Execute(f.ctx, optimization.WorkItem{Experiment: current, Lease: f.leases[1]})
			want = optimization.ErrConflict
			if mode == "lost" {
				want = optimization.ErrUnknownAttempt
			}
			if !errors.Is(err, want) || provider.calls != 1 {
				t.Fatalf("recovery replayed reflection: %v calls=%d", err, provider.calls)
			}
			after, err := f.s.GetReservation(f.ctx, current.ID, before.Request.CallKey)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("recovery rewrote charge: %v", err)
			}
			saved, err := f.s.GetDiscoveryLessons(f.ctx, current.ID)
			if mode == "lessons" {
				if err != nil || !reflect.DeepEqual(saved, prior) {
					t.Fatalf("recovery rewrote result: %v", err)
				}
			} else if !errors.Is(err, optimization.ErrNotFound) {
				t.Fatalf("lost result invented during recovery: %v", err)
			}
		})
	}
}

type reflectionRevocationStore struct {
	store.Store
	after func()
}

func (s reflectionRevocationStore) Transact(ctx context.Context, apply func(optimization.Transaction) error) error {
	settled := false
	err := s.Store.Transact(ctx, func(tx optimization.Transaction) error {
		return apply(reflectionSettlementTx{Transaction: tx, settled: &settled})
	})
	if err == nil && settled {
		s.after()
	}
	return err
}

type reflectionSettlementTx struct {
	optimization.Transaction
	settled *bool
}

func (tx reflectionSettlementTx) Settle(value optimization.UsageSettlement) error {
	if err := tx.Transaction.Settle(value); err != nil {
		return err
	}
	*tx.settled = true
	return nil
}
