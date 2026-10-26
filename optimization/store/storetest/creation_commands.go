package storetest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex/id"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

type creationCatalog struct {
	bundle               opt.ExperimentBundle
	calls                int
	prepareErr, grantErr error
	onPrepare            func()
}

func (c *creationCatalog) Authorize(_ context.Context, selection opt.CreationSelection) error {
	if selection.Agent.Ref != c.bundle.Experiment.Spec.AgentID.String() {
		return opt.ErrPolicyDenied
	}
	return c.grantErr
}
func (c *creationCatalog) Prepare(_ context.Context, target opt.CreationTarget) (opt.ExperimentBundle, error) {
	c.calls++
	if c.onPrepare != nil {
		c.onPrepare()
	}
	if c.prepareErr != nil {
		return opt.ExperimentBundle{}, c.prepareErr
	}
	result := c.bundle
	result.Experiment.ID = target.ExperimentID
	result.Experiment.Spec.Requester = target.Actor
	return result, nil
}
func creationSelection(bundle opt.ExperimentBundle) opt.CreationSelection {
	ref := opt.ContentRef{Ref: "approved", Revision: "1", Digest: strings.Repeat("a", 64)}
	agent := ref
	agent.Ref = bundle.Experiment.Spec.AgentID.String()
	return opt.CreationSelection{Agent: agent, Evaluation: bundle.Experiment.Spec.Evaluation, Controller: opt.ContentRef{Ref: bundle.Experiment.Spec.Controller.ID.String(), Revision: "1", Digest: bundle.Experiment.Spec.Controller.Digest}, Environment: ref, Mode: bundle.Experiment.Spec.Mode}
}
func creationOptimizer(t *testing.T, repo store.Store, auth *publicationAuthority, catalog *creationCatalog) *opt.Optimizer {
	t.Helper()
	service, err := opt.NewOptimizer(opt.OptimizerConfig{Repository: repo, Caller: auth, Authorizer: auth, IdentityResolver: auth, Handler: unusedPhase{}, Worker: opt.DefaultWorkerConfig(), CreationCatalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

type lostCreationAcknowledgment struct {
	store.Store
	lost bool
}

func (s *lostCreationAcknowledgment) Transact(ctx context.Context, fn func(store.Tx) error) error {
	err := s.Store.Transact(ctx, fn)
	if err == nil && s.lost {
		s.lost = false
		return opt.ErrUnavailable
	}
	return err
}
func creationCommands(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("CreationCommandAtomicity", func(t *testing.T) { creationCommandAtomicity(t, factory) })
	t.Run("CreationCommandDurableIdentity", func(t *testing.T) { creationCommandRecovery(t, factory, false) })
	t.Run("CreationCommandAuthorityAndPreparation", func(t *testing.T) {
		repo := factory(t, time.Now)
		bundle := Fixture(t, "create-authority")
		ctx := scoped(bundle.Experiment.Spec.Scope)
		auth := &publicationAuthority{actor: bundle.Experiment.Spec.Requester}
		catalog := &creationCatalog{bundle: bundle}
		service := creationOptimizer(t, repo, auth, catalog)
		request := opt.CreationRequest{Selection: creationSelection(bundle), IdempotencyKey: "create"}
		auth.denied = true
		if _, err := service.CreateCommand(ctx, request); !errors.Is(err, opt.ErrPolicyDenied) || catalog.calls != 0 {
			t.Fatalf("denied create: %v calls=%d", err, catalog.calls)
		}
		auth.denied = false
		if _, err := service.CreateCommand(context.Background(), request); !errors.Is(err, cortex.ErrNoScope) {
			t.Fatalf("unscoped: %v", err)
		}
		catalog.onPrepare = func() { auth.actor.Subject = "changed" }
		if _, err := service.CreateCommand(ctx, request); !errors.Is(err, opt.ErrPolicyDenied) {
			t.Fatalf("changed actor: %v", err)
		}
		auth.actor = bundle.Experiment.Spec.Requester
		catalog.onPrepare = func() { catalog.grantErr = opt.ErrPolicyDenied }
		if _, err := service.CreateCommand(ctx, request); !errors.Is(err, opt.ErrPolicyDenied) {
			t.Fatalf("catalog grant revoked during preparation: %v", err)
		}
		catalog.grantErr = nil
		catalog.onPrepare = nil
		bad := request
		limits := bundle.Experiment.Spec.Limits
		limits.MaxRounds++
		bad.Selection.Limits = &limits
		if _, err := service.CreateCommand(ctx, bad); !errors.Is(err, opt.ErrConflict) {
			t.Fatalf("ignored requested limits: %v", err)
		}
		bad = request
		bad.IdempotencyKey = ""
		if _, err := service.CreateCommand(ctx, bad); !errors.Is(err, opt.ErrInvalidSpec) {
			t.Fatalf("missing key: %v", err)
		}
		page, err := repo.ListExperiments(ctx, opt.ListQuery{})
		if err != nil || len(page.Items) != 0 {
			t.Fatalf("rejected preparation left draft: %+v %v", page, err)
		}
	})
}

// CreationCommandRecovery verifies receipt recovery on a reopened native store.
func CreationCommandRecovery(t *testing.T, factory func(*testing.T, func() time.Time) store.Store) {
	t.Helper()
	creationCommandRecovery(t, factory, true)
}
func creationCommandRecovery(t *testing.T, factory Factory, reopen bool) {
	t.Helper()
	repo := factory(t, time.Now)
	bundle := Fixture(t, "create-recovery")
	ctx := scoped(bundle.Experiment.Spec.Scope)
	auth := &publicationAuthority{actor: bundle.Experiment.Spec.Requester}
	catalog := &creationCatalog{bundle: bundle}
	lost := &lostCreationAcknowledgment{Store: repo, lost: true}
	service := creationOptimizer(t, lost, auth, catalog)
	request := opt.CreationRequest{Selection: creationSelection(bundle), IdempotencyKey: "SECRET-transport-key"}
	if _, err := service.CreateCommand(ctx, request); !errors.Is(err, opt.ErrUnavailable) {
		t.Fatalf("lost commit acknowledgment: %v", err)
	}
	receiptKey := fmt.Sprintf("operator/%x", sha256.Sum256([]byte(request.IdempotencyKey)))
	receipt, err := repo.GetCreationReceipt(ctx, receiptKey)
	if err != nil || receipt.Key == request.IdempotencyKey || receipt.SpecDigest == "" {
		t.Fatalf("durable creation receipt: %+v %v", receipt, err)
	}
	created, err := repo.GetExperiment(ctx, receipt.ExperimentID)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == bundle.Experiment.ID || created.State != opt.StateDraft || created.Revision != 1 {
		t.Fatalf("creation identity: %+v", created)
	}
	if _, err = service.Start(ctx, created.ID, 1); err != nil {
		t.Fatal(err)
	}
	if reopen {
		if err = repo.Close(); err != nil {
			t.Fatal(err)
		}
		repo = factory(t, time.Now)
	}
	catalog.prepareErr = opt.ErrUnavailable
	service = creationOptimizer(t, repo, auth, catalog)
	recovered, err := service.CreateCommand(ctx, request)
	if err != nil || recovered.ID != created.ID || recovered.State != opt.StateQueued || catalog.calls != 1 {
		t.Fatalf("recovery prepared again: %+v %v calls=%d", recovered, err, catalog.calls)
	}
	changed := request
	changed.Selection.Environment.Revision = "2"
	if _, err = service.CreateCommand(ctx, changed); !errors.Is(err, opt.ErrConflict) {
		t.Fatalf("changed selection: %v", err)
	}
	auth.actor.Subject = "other-operator"
	if _, err = service.CreateCommand(ctx, request); !errors.Is(err, opt.ErrConflict) {
		t.Fatalf("changed actor: %v", err)
	}
	auth.actor = bundle.Experiment.Spec.Requester
	auth.denied = true
	if _, err = service.CreateCommand(ctx, request); !errors.Is(err, opt.ErrPolicyDenied) {
		t.Fatalf("revoked create grant: %v", err)
	}
	auth.denied = false
	catalog.grantErr = opt.ErrPolicyDenied
	if _, err = service.CreateCommand(ctx, request); !errors.Is(err, opt.ErrPolicyDenied) {
		t.Fatalf("revoked catalog grant: %v", err)
	}
	catalog.grantErr = nil
	foreign := cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "project", Value: "foreign"}}})
	if _, err = service.CreateCommand(foreign, request); err == nil {
		t.Fatal("foreign scope reused receipt")
	}
	events, err := repo.ListEvents(ctx, created.ID, 0, 100)
	if err != nil || len(events.Items) != 2 {
		t.Fatalf("duplicate draft or queue event: %+v %v", events, err)
	}
	page, err := repo.ListExperiments(ctx, opt.ListQuery{})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != created.ID {
		t.Fatalf("duplicate experiment: %+v %v", page, err)
	}
}

func creationCommandAtomicity(t *testing.T, factory Factory) {
	repo := factory(t, time.Now)
	bundle := Fixture(t, "create-atomic")
	ctx := scoped(bundle.Experiment.Spec.Scope)
	command := opt.CreationCommand{Key: "operator/" + strings.Repeat("b", 64), Actor: bundle.Experiment.Spec.Requester, Selection: creationSelection(bundle), Bundle: bundle}
	aborted := errors.New("abort")
	if err := repo.Transact(ctx, func(tx store.Tx) error {
		if _, e := tx.CreateExperimentCommand(command); e != nil {
			return e
		}
		return aborted
	}); !errors.Is(err, aborted) {
		t.Fatal(err)
	}
	if _, err := repo.GetCreationReceipt(ctx, command.Key); !errors.Is(err, opt.ErrNotFound) {
		t.Fatalf("aborted receipt persisted: %v", err)
	}
	if _, err := repo.GetExperiment(ctx, bundle.Experiment.ID); !errors.Is(err, opt.ErrNotFound) {
		t.Fatalf("aborted draft persisted: %v", err)
	}
	var wg sync.WaitGroup
	outcomes := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			competing := command
			competing.Bundle.Experiment.ID = id.New(opt.PrefixExperiment)
			outcomes <- repo.Transact(ctx, func(tx store.Tx) error { _, e := tx.CreateExperimentCommand(competing); return e })
		})
	}
	wg.Wait()
	close(outcomes)
	for err := range outcomes {
		if err != nil && !errors.Is(err, opt.ErrConflict) {
			t.Fatal(err)
		}
	}
	var recovered opt.CreationReceipt
	if err := repo.Transact(ctx, func(tx store.Tx) error { var e error; recovered, e = tx.CreateExperimentCommand(command); return e }); err != nil {
		t.Fatal(err)
	}
	page, err := repo.ListExperiments(ctx, opt.ListQuery{})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != recovered.ExperimentID || page.Items[0].SpecDigest != recovered.SpecDigest {
		t.Fatalf("competing creation forked: %+v %+v %v", page, recovered, err)
	}
	command.Selection.Environment.Revision = "different"
	if err = repo.Transact(ctx, func(tx store.Tx) error { _, e := tx.CreateExperimentCommand(command); return e }); !errors.Is(err, opt.ErrConflict) {
		t.Fatalf("changed atomic payload: %v", err)
	}
	foreign := cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "project", Value: "foreign"}}})
	if _, err = repo.GetCreationReceipt(foreign, command.Key); !errors.Is(err, opt.ErrNotFound) {
		t.Fatalf("foreign receipt: %v", err)
	}
	if _, err = repo.GetCreationReceipt(context.Background(), command.Key); !errors.Is(err, cortex.ErrNoScope) {
		t.Fatalf("unscoped receipt: %v", err)
	}
}
