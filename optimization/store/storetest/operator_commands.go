package storetest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

func commandOptimizer(t *testing.T, s store.Store, authority *publicationAuthority) *optimization.Optimizer {
	t.Helper()
	op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: unusedPhase{}, Worker: optimization.DefaultWorkerConfig()})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func operatorCommands(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("OperatorCommandsBindActionRevisionActorAndKey", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := Fixture(t, "commands")
		ctx := scoped(bundle.Experiment.Spec.Scope)
		authority := &publicationAuthority{actor: bundle.Experiment.Spec.Requester}
		op := commandOptimizer(t, s, authority)
		created, err := op.Create(ctx, bundle)
		if err != nil {
			t.Fatal(err)
		}

		for _, change := range []optimization.Transition{
			{CommandAction: optimization.ActionStart, NextState: optimization.StateCancelled},
			{CommandAction: optimization.ActionActivate, NextState: optimization.StateQueued},
			{CommandAction: optimization.ActionStart, NextState: optimization.StateQueued, NextPhase: optimization.PhaseBaseline},
		} {
			change.ExperimentID = created.ID
			change.ExpectedRevision = created.Revision
			change.IdempotencyKey = "invalid-command"
			change.Actor = authority.actor
			if err = s.Transact(ctx, func(tx optimization.Transaction) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrInvalidSpec) {
				t.Fatalf("forged command transition: %v", err)
			}
		}
		request := optimization.CommandRequest{ExperimentID: created.ID, ExpectedRevision: created.Revision, IdempotencyKey: "start-1"}
		for _, key := range []string{"", " padded", "line\nbreak", strings.Repeat("x", 513)} {
			invalid := request
			invalid.IdempotencyKey = key
			if _, err = op.StartCommand(ctx, invalid); !errors.Is(err, optimization.ErrInvalidSpec) {
				t.Fatalf("invalid key: %v", err)
			}
		}
		if _, err = op.ResumeCommand(ctx, request); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("resume draft: %v", err)
		}
		queued, err := op.StartCommand(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if queued.State != optimization.StateQueued || queued.Revision != 2 {
			t.Fatalf("queued: %+v", queued)
		}
		before, err := s.ListEvents(ctx, created.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		again, err := op.StartCommand(ctx, request)
		if err != nil || again.Revision != queued.Revision {
			t.Fatalf("repeat: %+v %v", again, err)
		}

		outcomes := make(chan error, 8)
		for range 8 {
			go func() {
				result, callErr := op.StartCommand(ctx, request)
				if callErr == nil && result.Revision != queued.Revision {
					callErr = optimization.ErrConflict
				}
				outcomes <- callErr
			}()
		}
		for range 8 {
			if callErr := <-outcomes; callErr != nil {
				t.Fatalf("concurrent retry: %v", callErr)
			}
		}
		for _, invoke := range []func(context.Context, optimization.CommandRequest) (optimization.Experiment, error){op.ResumeCommand, op.CancelCommand} {
			if _, err = invoke(ctx, request); !errors.Is(err, optimization.ErrConflict) {
				t.Fatalf("different action reused key: %v", err)
			}
		}
		changed := request
		changed.ExpectedRevision++
		if _, err = op.StartCommand(ctx, changed); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("different revision reused key: %v", err)
		}
		authority.actor.Subject = "other"
		if _, err = op.StartCommand(ctx, request); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("different actor reused key: %v", err)
		}
		authority.actor = bundle.Experiment.Spec.Requester
		authority.denied = true
		if _, err = op.StartCommand(ctx, request); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("repeat bypassed current grant: %v", err)
		}
		authority.denied = false
		after, err := s.ListEvents(ctx, created.ID, 0, 100)
		if err != nil || len(after.Items) != len(before.Items) {
			t.Fatalf("duplicate events: %d %d %v", len(before.Items), len(after.Items), err)
		}
		if err = s.Transact(ctx, func(tx optimization.Transaction) error {
			_, e := tx.ClaimWork(optimization.ClaimRequest{ExperimentID: created.ID, Owner: "worker", Duration: time.Minute})
			return e
		}); err != nil {
			t.Fatal(err)
		}
		current, err := s.GetExperiment(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		paused, err := op.Pause(ctx, current.ID, current.Revision)
		if err != nil {
			t.Fatal(err)
		}
		resume := optimization.CommandRequest{ExperimentID: created.ID, ExpectedRevision: paused.Revision, IdempotencyKey: "resume-1"}
		if _, err = op.StartCommand(ctx, resume); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("start resumed paused experiment: %v", err)
		}
		resumed, err := op.ResumeCommand(ctx, resume)
		if err != nil {
			t.Fatal(err)
		}
		repeated, err := op.ResumeCommand(ctx, resume)
		if err != nil || repeated.Revision != resumed.Revision {
			t.Fatalf("resume repeat: %+v %v", repeated, err)
		}
		cancel := optimization.CommandRequest{ExperimentID: created.ID, ExpectedRevision: resumed.Revision, IdempotencyKey: "cancel-1"}
		cancelled, err := op.CancelCommand(ctx, cancel)
		if err != nil || cancelled.State != optimization.StateCancelled {
			t.Fatalf("cancel: %+v %v", cancelled, err)
		}
		repeated, err = op.CancelCommand(ctx, cancel)
		if err != nil || repeated.Revision != cancelled.Revision {
			t.Fatalf("cancel repeat: %+v %v", repeated, err)
		}
	})
}

// OperatorCommandRecovery proves that a lost command response can be retried
// after reopening storage without a second transition or audit event.
func OperatorCommandRecovery(t *testing.T, open func(*testing.T, func() time.Time) store.Store) {
	t.Helper()
	first := open(t, time.Now)
	bundle := Fixture(t, "command-recovery")
	ctx := scoped(bundle.Experiment.Spec.Scope)
	authority := &publicationAuthority{actor: bundle.Experiment.Spec.Requester}
	op := commandOptimizer(t, first, authority)
	created, err := op.Create(ctx, bundle)
	if err != nil {
		t.Fatal(err)
	}
	request := optimization.CommandRequest{ExperimentID: created.ID, ExpectedRevision: created.Revision, IdempotencyKey: "response-lost"}
	queued, err := op.StartCommand(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	before, err := first.ListEvents(ctx, created.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	second := open(t, time.Now)
	op = commandOptimizer(t, second, authority)
	again, err := op.StartCommand(ctx, request)
	if err != nil || again.Revision != queued.Revision || again.State != queued.State {
		t.Fatalf("reopen retry: %+v %v", again, err)
	}
	after, err := second.ListEvents(ctx, created.ID, 0, 100)
	if err != nil || len(after.Items) != len(before.Items) {
		t.Fatalf("reopen events: %d %d %v", len(before.Items), len(after.Items), err)
	}
	if _, err = op.CancelCommand(ctx, request); !errors.Is(err, optimization.ErrConflict) {
		t.Fatalf("reopen collision: %v", err)
	}
	cancel := optimization.CommandRequest{ExperimentID: created.ID, ExpectedRevision: queued.Revision, IdempotencyKey: "cancel-response-lost"}
	cancelled, err := op.CancelCommand(ctx, cancel)
	if err != nil {
		t.Fatal(err)
	}
	if err = second.Close(); err != nil {
		t.Fatal(err)
	}
	third := open(t, time.Now)
	op = commandOptimizer(t, third, authority)
	again, err = op.CancelCommand(ctx, cancel)
	if err != nil || again.Revision != cancelled.Revision || again.State != optimization.StateCancelled {
		t.Fatalf("reopen cancel retry: %+v %v", again, err)
	}
	again, err = op.StartCommand(ctx, request)
	if err != nil || again.Revision != cancelled.Revision || again.State != optimization.StateCancelled {
		t.Fatalf("old start retry revived cancelled work: %+v %v", again, err)
	}
	events, err := third.ListEvents(ctx, created.ID, 0, 100)
	if err != nil || len(events.Items) != len(before.Items)+1 {
		t.Fatalf("reopen cancel events: %d %v", len(events.Items), err)
	}
}
