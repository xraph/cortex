package storetest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

func improvementFixture(t *testing.T) (parent optimization.ExperimentBundle, children []optimization.ExperimentBundle) {
	t.Helper()
	parent = Fixture(t, "improvement-family")
	parent.Experiment.Spec.Budget = optimization.Budget{MaxTokens: 100, TokenOnly: true}
	candidate := parent.Versions[1]
	candidate.ID = id.New(optimization.PrefixVersion)
	controller := *candidate.Controller
	controller.ModelRevision = "2"
	candidate.Controller = &controller
	var err error
	candidate, err = optimization.SealVersion(candidate)
	if err != nil {
		t.Fatal(err)
	}
	parent.Versions = append(parent.Versions, candidate)
	plan := &optimization.ImprovementPlan{Controllers: [2]optimization.VersionRef{parent.Experiment.Spec.Controller, {ID: candidate.ID, Digest: candidate.Digest}}}
	children = make([]optimization.ExperimentBundle, 2)
	for arm := range children {
		child := parent.Experiment
		child.ID = id.New(optimization.PrefixExperiment)
		child.Spec.Controller = plan.Controllers[arm]
		child.Spec.Parent = &optimization.ParentExperiment{ExperimentID: parent.Experiment.ID, Arm: arm, Sequence: 0}
		children[arm] = optimization.ExperimentBundle{Experiment: child}
		plan.Children = append(plan.Children, optimization.ImprovementChild{ExperimentID: child.ID, Spec: child.Spec, EvaluationPlan: child.EvaluationPlan})
	}
	parent.Experiment.Spec.Improvement = plan
	return parent, children
}

func runningFamily(t *testing.T, factory Factory, now func() time.Time, budgets ...optimization.Budget) (s store.Store, ctx context.Context, parent optimization.ExperimentBundle, children []optimization.ExperimentBundle, leases []optimization.WorkLease) {
	t.Helper()
	s = factory(t, now)
	parent, children = improvementFixture(t)
	if len(budgets) > 0 {
		parent.Experiment.Spec.Budget = budgets[0]
		for i := range children {
			children[i].Experiment.Spec.Budget = budgets[0]
			parent.Experiment.Spec.Improvement.Children[i].Spec.Budget = budgets[0]
		}
	}
	ctx = scoped(parent.Experiment.Spec.Scope)
	leases = make([]optimization.WorkLease, 3)
	if err := s.Transact(ctx, func(tx store.Tx) error {
		var err error
		leases[0], err = startFixture(tx, parent, "parent")
		if err != nil {
			return err
		}
		for i := range children {
			children[i].ParentLease = &leases[0]
			leases[i+1], err = startFixture(tx, children[i], "child")
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s, ctx, parent, children, leases
}

func improvementFamily(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("ImprovementParentBudgetIncludesChildTrials", func(t *testing.T) {
		s, ctx, parent, children, leases := runningFamily(t, factory, time.Now)
		reserve(ctx, t, s, reservation(parent.Experiment.ID, leases[0], "parent-proposal", 20))
		reserve(ctx, t, s, reservation(children[0].Experiment.ID, leases[1], "child-judge", 60))
		err := s.Transact(ctx, func(tx store.Tx) error {
			_, reserveErr := tx.Reserve(reservation(children[1].Experiment.ID, leases[2], "child-trial", 21))
			return reserveErr
		})
		if !errors.Is(err, optimization.ErrBudgetExceeded) {
			t.Fatalf("parent budget bypassed: %v", err)
		}
	})
	t.Run("ImprovementRejectsChangedChildTemplate", func(t *testing.T) {
		s := factory(t, time.Now)
		parent, children := improvementFixture(t)
		ctx := scoped(parent.Experiment.Spec.Scope)
		var lease optimization.WorkLease
		if err := s.Transact(ctx, func(tx store.Tx) error { var err error; lease, err = startFixture(tx, parent, "parent"); return err }); err != nil {
			t.Fatal(err)
		}
		child := children[0]
		child.ParentLease = &lease
		child.Experiment.Spec.Limits.MaxSteps++
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(child) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("changed frozen child accepted: %v", err)
		}
	})
	t.Run("ImprovementParentPauseFencesChild", func(t *testing.T) {
		s, ctx, parent, children, leases := runningFamily(t, factory, time.Now)
		current, err := s.GetExperiment(ctx, parent.Experiment.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Transact(ctx, func(tx store.Tx) error {
			return tx.CommitTransition(optimization.Transition{ExperimentID: current.ID, ExpectedRevision: current.Revision, NextState: optimization.StatePaused, IdempotencyKey: "pause", Actor: current.Spec.Requester})
		}); err != nil {
			t.Fatal(err)
		}
		err = s.Transact(ctx, func(tx store.Tx) error {
			_, reserveErr := tx.Reserve(reservation(children[0].Experiment.ID, leases[1], "after-pause", 1))
			return reserveErr
		})
		if !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("paused parent permitted child dispatch: %v", err)
		}
	})
}

func improvementRecovery(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("ImprovementParentOverrunStopsSiblingAttempt", func(t *testing.T) {
		s, ctx, _, children, leases := runningFamily(t, factory, time.Now)
		req := reservation(children[0].Experiment.ID, leases[1], "overrun", 1)
		reserve(ctx, t, s, req)
		if err := s.Transact(ctx, func(tx store.Tx) error {
			return tx.Settle(optimization.UsageSettlement{ExperimentID: req.ExperimentID, AttemptID: req.AttemptID, CallKey: req.CallKey, Receipt: "overrun-receipt", Usage: optimization.Usage{Tokens: 110, TokenQuality: optimization.UsageKnown}, Complete: true, Lease: req.Lease})
		}); err != nil {
			t.Fatal(err)
		}
		child := children[1]
		trial := queuedTrial(child, 0)
		commitTrialChange(ctx, t, s, trialTransition(ctx, t, s, child.Experiment.ID, leases[2], "queue", []optimization.Trial{trial}, nil))
		change := trialTransition(ctx, t, s, child.Experiment.ID, leases[2], "attempt", nil, []optimization.TrialUpdate{{TrialID: trial.ID, ExpectedRevision: 1, State: optimization.TrialRunning, StartAttemptID: id.New(optimization.PrefixAttempt)}})
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrBudgetExceeded) {
			t.Fatalf("parent overrun admitted sibling attempt: %v", err)
		}
	})
	t.Run("ImprovementAggregateOverflowStopsHeldDispatch", func(t *testing.T) {
		s, ctx, parent, children, leases := runningFamily(t, factory, time.Now, optimization.Budget{MaxTokens: math.MaxInt64, TokenOnly: true})
		held := reservation(parent.Experiment.ID, leases[0], "held", 1)
		reserve(ctx, t, s, held)
		requests := make([]optimization.UsageReservation, 2)
		for i := range children {
			requests[i] = reservation(children[i].Experiment.ID, leases[i+1], "overrun", 1)
			reserve(ctx, t, s, requests[i])
		}
		for i, req := range requests {
			if err := s.Transact(ctx, func(tx store.Tx) error {
				return tx.Settle(optimization.UsageSettlement{ExperimentID: req.ExperimentID, AttemptID: req.AttemptID, CallKey: req.CallKey, Receipt: fmt.Sprintf("provider/%d", i), Usage: optimization.Usage{Tokens: math.MaxInt64, TokenQuality: optimization.UsageKnown}, Complete: true, Lease: req.Lease})
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := beginUsage(ctx, s, held, strings.Repeat("a", 64)); !errors.Is(err, optimization.ErrBudgetExceeded) {
			t.Fatalf("saturated aggregate admitted dispatch: %v", err)
		}
	})
	t.Run("ImprovementParentTakeoverFencesPriorGeneration", func(t *testing.T) {
		now := time.Now().UTC()
		s, ctx, parent, children, leases := runningFamily(t, factory, func() time.Time { return now })
		childLease := leases[1]
		childLease.ExpiresAt = now.Add(10 * time.Minute)
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.RenewLease(childLease) }); err != nil {
			t.Fatal(err)
		}
		now = now.Add(6 * time.Minute)
		var parentLease, newChild optimization.WorkLease
		if err := s.Transact(ctx, func(tx store.Tx) error {
			var err error
			parentLease, err = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: parent.Experiment.ID, Owner: "replacement", Duration: 5 * time.Minute})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if parentLease.Generation <= leases[0].Generation {
			t.Fatal("parent fence did not advance")
		}
		if err := s.Transact(ctx, func(tx store.Tx) error {
			_, reserveErr := tx.Reserve(reservation(children[0].Experiment.ID, childLease, "stale", 1))
			return reserveErr
		}); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("stale child retained authority: %v", err)
		}
		if err := s.Transact(ctx, func(tx store.Tx) error {
			var err error
			newChild, err = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: children[0].Experiment.ID, Owner: "replacement-child", Duration: time.Minute})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if newChild.Generation <= childLease.Generation || newChild.ParentGeneration != parentLease.Generation {
			t.Fatalf("wrong child fence: %+v", newChild)
		}
		reserve(ctx, t, s, reservation(children[0].Experiment.ID, newChild, "fresh", 1))
	})
	for _, tokens := range []int64{20, 120} {
		t.Run(fmt.Sprintf("ImprovementLateReceipt/%d", tokens), func(t *testing.T) {
			s, ctx, _, children, leases := runningFamily(t, factory, time.Now)
			req := reservation(children[0].Experiment.ID, leases[1], "unknown", 60)
			digest := strings.Repeat("a", 64)
			if err := beginUsage(ctx, s, req, digest); err != nil {
				t.Fatal(err)
			}
			held := reservation(children[1].Experiment.ID, leases[2], "held", 40)
			reserve(ctx, t, s, held)
			current, err := s.GetExperiment(ctx, req.ExperimentID)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Transact(ctx, func(tx store.Tx) error {
				return tx.CommitTransition(optimization.Transition{ExperimentID: current.ID, ExpectedRevision: current.Revision, NextState: optimization.StateCancelled, IdempotencyKey: "cancel", Actor: current.Spec.Requester})
			}); err != nil {
				t.Fatal(err)
			}
			saved, err := s.GetReservation(ctx, req.ExperimentID, req.CallKey)
			if err != nil {
				t.Fatal(err)
			}
			settlement := optimization.UsageSettlement{ExperimentID: req.ExperimentID, AttemptID: req.AttemptID, CallKey: req.CallKey, Receipt: "provider/late", Usage: optimization.Usage{Tokens: tokens, TokenQuality: optimization.UsageKnown}, Complete: true}
			recovery := optimization.UsageReconciliation{Settlement: settlement, RequestDigest: digest, ExpectedRevision: saved.Revision, Actor: optimization.IdentityRef{Subject: "recovery"}}
			for range 2 {
				if err = s.Transact(ctx, func(tx store.Tx) error { return tx.ReconcileUsage(recovery) }); err != nil {
					t.Fatal(err)
				}
			}
			err = beginUsage(ctx, s, held, digest)
			if tokens > 100 {
				if !errors.Is(err, optimization.ErrBudgetExceeded) {
					t.Fatalf("late child overrun did not block sibling: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				reserve(ctx, t, s, reservation(children[1].Experiment.ID, leases[2], "remaining", 40))
				if err = s.Transact(ctx, func(tx store.Tx) error {
					_, reserveErr := tx.Reserve(reservation(children[1].Experiment.ID, leases[2], "over", 1))
					return reserveErr
				}); !errors.Is(err, optimization.ErrBudgetExceeded) {
					t.Fatalf("receipt refunded twice: %v", err)
				}
			}
			current, err = s.GetExperiment(ctx, req.ExperimentID)
			if err != nil || current.State != optimization.StateCancelled {
				t.Fatalf("receipt reopened child: %v %v", current.State, err)
			}
		})
	}
	t.Run("ImprovementReceiptCannotSettleBothArms", func(t *testing.T) {
		s, ctx, _, children, leases := runningFamily(t, factory, time.Now)
		for i := range children {
			req := reservation(children[i].Experiment.ID, leases[i+1], "same-call-key", 40)
			reserve(ctx, t, s, req)
			settlement := optimization.UsageSettlement{ExperimentID: req.ExperimentID, AttemptID: req.AttemptID, CallKey: req.CallKey, Receipt: "same-receipt", Usage: optimization.Usage{Tokens: 1, TokenQuality: optimization.UsageKnown}, Complete: true, Lease: leases[i+1]}
			err := s.Transact(ctx, func(tx store.Tx) error { return tx.Settle(settlement) })
			if i == 0 && err != nil || i == 1 && !errors.Is(err, optimization.ErrConflict) {
				t.Fatalf("receipt reuse arm %d: %v", i, err)
			}
		}
	})
	t.Run("ImprovementParentBudgetRace", func(t *testing.T) {
		s, ctx, _, children, leases := runningFamily(t, factory, time.Now)
		results := make(chan error, 2)
		for i := range children {
			go func() {
				results <- s.Transact(ctx, func(tx store.Tx) error {
					_, reserveErr := tx.Reserve(reservation(children[i].Experiment.ID, leases[i+1], "concurrent", 60))
					return reserveErr
				})
			}()
		}
		wins := 0
		for range 2 {
			err := <-results
			if err == nil {
				wins++
			} else if !errors.Is(err, optimization.ErrBudgetExceeded) && !errors.Is(err, optimization.ErrConflict) {
				t.Fatal(err)
			}
		}
		if wins != 1 {
			t.Fatalf("family admitted %d concurrent arms", wins)
		}
		if err := s.Transact(ctx, func(tx store.Tx) error {
			_, reserveErr := tx.Reserve(reservation(children[0].Experiment.ID, leases[1], "remaining", 41))
			return reserveErr
		}); !errors.Is(err, optimization.ErrBudgetExceeded) {
			t.Fatalf("race lost aggregate debit: %v", err)
		}
	})
}

// ImprovementRecovery verifies family accounting after closing and reopening storage.
func ImprovementRecovery(t *testing.T, open func(*testing.T, func() time.Time) store.Store) {
	t.Helper()
	now := time.Now().UTC()
	first, ctx, parent, children, leases := runningFamily(t, open, func() time.Time { return now })
	req := reservation(children[0].Experiment.ID, leases[1], "lost-response", 60)
	digest := strings.Repeat("b", 64)
	if err := beginUsage(ctx, first, req, digest); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := open(t, func() time.Time { return now })
	savedParent, err := second.GetExperiment(ctx, parent.Experiment.ID)
	if err != nil || len(savedParent.Spec.Improvement.Children) != 2 || savedParent.Spec.Improvement.Children[1].Spec.Controller != children[1].Experiment.Spec.Controller {
		t.Fatalf("reopened roster: %v", err)
	}
	if err = second.Transact(ctx, func(tx store.Tx) error {
		_, reserveErr := tx.Reserve(reservation(children[1].Experiment.ID, leases[2], "too-much", 41))
		return reserveErr
	}); !errors.Is(err, optimization.ErrBudgetExceeded) {
		t.Fatalf("reopen lost child debit: %v", err)
	}
	if err = beginUsage(ctx, second, req, digest); !errors.Is(err, optimization.ErrUnknownAttempt) {
		t.Fatalf("reopen granted replay: %v", err)
	}
	now = now.Add(6 * time.Minute)
	saved, err := second.GetReservation(ctx, req.ExperimentID, req.CallKey)
	if err != nil {
		t.Fatal(err)
	}
	recovery := optimization.UsageReconciliation{Settlement: optimization.UsageSettlement{ExperimentID: req.ExperimentID, AttemptID: req.AttemptID, CallKey: req.CallKey, Receipt: "recovered", Complete: true, Usage: optimization.Usage{Tokens: 20, TokenQuality: optimization.UsageKnown}}, RequestDigest: digest, ExpectedRevision: saved.Revision, Actor: optimization.IdentityRef{Subject: "billing"}}
	if err = second.Transact(ctx, func(tx store.Tx) error { return tx.ReconcileUsage(recovery) }); err != nil {
		t.Fatal(err)
	}
	if err = second.Close(); err != nil {
		t.Fatal(err)
	}
	third := open(t, func() time.Time { return now })
	var fresh optimization.WorkLease
	if err = third.Transact(ctx, func(tx store.Tx) error {
		if _, claimErr := tx.ClaimWork(optimization.ClaimRequest{ExperimentID: parent.Experiment.ID, Owner: "recovered-parent", Duration: time.Minute}); claimErr != nil {
			return claimErr
		}
		var claimErr error
		fresh, claimErr = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: children[1].Experiment.ID, Owner: "recovered-child", Duration: time.Minute})
		return claimErr
	}); err != nil {
		t.Fatal(err)
	}
	reserve(ctx, t, third, reservation(children[1].Experiment.ID, fresh, "remaining", 80))
	if err = third.Transact(ctx, func(tx store.Tx) error {
		_, reserveErr := tx.Reserve(reservation(children[1].Experiment.ID, fresh, "over", 1))
		return reserveErr
	}); !errors.Is(err, optimization.ErrBudgetExceeded) {
		t.Fatalf("recovery refunded twice: %v", err)
	}
}

func improvementBindings(t *testing.T, factory Factory) {
	t.Helper()
	for _, mode := range []string{"nested", "changed-arm", "changed-sequence", "missing-arm", "different-task", "different-requester", "different-price", "unbounded-child", "self-child", "borrowed-child"} {
		t.Run("ImprovementInvalidPlan/"+mode, func(t *testing.T) {
			s := factory(t, time.Now)
			parent, children := improvementFixture(t)
			plan := parent.Experiment.Spec.Improvement
			switch mode {
			case "nested":
				plan.Children[0].Spec.Improvement = &optimization.ImprovementPlan{}
			case "changed-arm":
				plan.Children[0].Spec.Parent.Arm = 1
			case "changed-sequence":
				plan.Children[0].Spec.Parent.Sequence = 2
			case "missing-arm":
				plan.Children = plan.Children[:1]
			case "different-task":
				plan.Children[1].Spec.Limits.MaxSteps++
			case "different-requester":
				plan.Children[1].Spec.Requester.Subject = "foreign"
			case "different-price":
				plan.Children[1].Spec.Budget = optimization.Budget{MaxTokens: 100, MaxUSDMicros: 1, PricingRevision: "foreign"}
			case "unbounded-child":
				plan.Children[1].Spec.Budget.MaxTokens++
			case "self-child":
				plan.Children[0].ExperimentID = parent.Experiment.ID
			case "borrowed-child":
				other := children[0]
				other.Experiment.Spec.Parent = nil
				other.Versions = parent.Versions
				if err := s.Transact(scoped(parent.Experiment.Spec.Scope), func(tx store.Tx) error { return tx.CreateExperiment(other) }); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Transact(scoped(parent.Experiment.Spec.Scope), func(tx store.Tx) error { return tx.CreateExperiment(parent) }); err == nil {
				t.Fatalf("invalid %s plan accepted", mode)
			}
		})
	}
	t.Run("ImprovementParentAuthorityRequired", func(t *testing.T) {
		s := factory(t, time.Now)
		parent, children := improvementFixture(t)
		ctx := scoped(parent.Experiment.Spec.Scope)
		var lease optimization.WorkLease
		if err := s.Transact(ctx, func(tx store.Tx) error { var err error; lease, err = startFixture(tx, parent, "parent"); return err }); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"missing", "stale", "unrelated"} {
			child := children[0]
			wrong := lease
			switch mode {
			case "stale":
				wrong.Generation++
				child.ParentLease = &wrong
			case "unrelated":
				wrong.ExperimentID = child.Experiment.ID
				child.ParentLease = &wrong
			}
			if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(child) }); !errors.Is(err, optimization.ErrConflict) {
				t.Fatalf("%s parent authority accepted: %v", mode, err)
			}
		}
	})
	t.Run("ImprovementCurrentParentGrantRequired", func(t *testing.T) {
		s, ctx, parent, children, leases := runningFamily(t, factory, time.Now)
		authority := &improvementAuthority{denied: parent.Experiment.ID}
		meter, err := optimization.NewLedgerMeter(s, authority, authority)
		if err != nil {
			t.Fatal(err)
		}
		req := reservation(children[0].Experiment.ID, leases[1], "denied", 10)
		if _, err = meter.BeginCall(ctx, req, strings.Repeat("a", 64)); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("parent revocation ignored: %v", err)
		}
		if _, err = s.GetReservation(ctx, req.ExperimentID, req.CallKey); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("denied call persisted: %v", err)
		}
	})
}

type improvementAuthority struct{ denied id.ID }

func (a *improvementAuthority) Authorize(_ context.Context, _ optimization.Action, r optimization.Resource) error {
	if r.ID == a.denied {
		return optimization.ErrPolicyDenied
	}
	return nil
}
func (*improvementAuthority) Resolve(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
	return ctx, nil
}

func improvementMoney(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("ImprovementMoneyIncludesAllOperations", func(t *testing.T) {
		s := factory(t, time.Now)
		parent, children := improvementFixture(t)
		budget := optimization.Budget{MaxTokens: 1000, MaxUSDMicros: 100, PricingRevision: "pinned"}
		parent.Experiment.Spec.Budget = budget
		for i := range children {
			children[i].Experiment.Spec.Budget = budget
			parent.Experiment.Spec.Improvement.Children[i].Spec.Budget = budget
		}
		ctx := scoped(parent.Experiment.Spec.Scope)
		leases := make([]optimization.WorkLease, 3)
		if err := s.Transact(ctx, func(tx store.Tx) error {
			var err error
			leases[0], err = startFixture(tx, parent, "parent")
			if err != nil {
				return err
			}
			for i := range children {
				children[i].ParentLease = &leases[0]
				leases[i+1], err = startFixture(tx, children[i], "child")
				if err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		operations := []optimization.OperationClass{optimization.OperationBaseline, optimization.OperationProposal, optimization.OperationProbe, optimization.OperationComposition, optimization.OperationValidation, optimization.OperationReflection, optimization.OperationJudge, optimization.OperationEmbedding, optimization.OperationRetrieval, optimization.OperationTool}
		for i, operation := range operations {
			req := reservation(children[i%2].Experiment.ID, leases[i%2+1], fmt.Sprintf("operation/%d", i), 1)
			req.Operation, req.MaxUSDMicros, req.PricingRevision = operation, 10, "pinned"
			reserve(ctx, t, s, req)
		}
		req := reservation(parent.Experiment.ID, leases[0], "over", 1)
		req.MaxUSDMicros, req.PricingRevision = 1, "pinned"
		if err := s.Transact(ctx, func(tx store.Tx) error { _, reserveErr := tx.Reserve(req); return reserveErr }); !errors.Is(err, optimization.ErrBudgetExceeded) {
			t.Fatalf("parent bypassed child monetary spend: %v", err)
		}
	})
}
