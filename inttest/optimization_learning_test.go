//go:build integration

package inttest_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/command"
	"github.com/xraph/fabriq/core/query"
	"github.com/xraph/fabriq/core/tenant"
	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	optsqlite "github.com/xraph/cortex/optimization/store/sqlite"
	"github.com/xraph/cortex/optimization/store/storetest"

	learning "github.com/xraph/cortex/integrations/fabriq/optimization"
)

type learningAuthority struct{}

func (learningAuthority) Authorize(context.Context, opt.Action, opt.Resource) error { return nil }
func (learningAuthority) Resolve(ctx context.Context, _ opt.IdentityRef, _ cortex.Scope) (context.Context, error) {
	return ctx, nil
}
func (learningAuthority) Current(context.Context) (opt.IdentityRef, error) {
	return opt.IdentityRef{Subject: "learning-worker"}, nil
}

type lostLearningReply struct {
	learning.Toolkit
	lose   bool
	outage bool
}

func (tk *lostLearningReply) Remember(ctx context.Context, request agent.RememberRequest) (command.Result, error) {
	if tk.outage {
		return command.Result{}, errors.New("destination unavailable")
	}
	result, err := tk.Toolkit.Remember(ctx, request)
	if err == nil && tk.lose {
		tk.lose = false
		return command.Result{}, errors.New("lost write reply")
	}
	return result, err
}

func openOptimizationStore(t *testing.T, path string) *optsqlite.Store {
	t.Helper()
	driver := sqlitedriver.New()
	if err := driver.Open(context.Background(), path+"?_pragma=journal_mode(WAL)"); err != nil {
		t.Fatal(err)
	}
	db, err := grove.Open(driver)
	if err != nil {
		t.Fatal(err)
	}
	s := optsqlite.New(db)
	if err = s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestOptimizationLearningPersistsAndRevokes(t *testing.T) {
	const entity = "optimization_lessons"
	fabric := openLearningFabric(t, learning.LessonSpec(entity))
	path := filepath.Join(t.TempDir(), "optimization.db")
	local, ctx, request := storetest.PublishedLesson(t, func(t *testing.T, _ func() time.Time) store.Store { return openOptimizationStore(t, path) })
	toolkit, err := agent.NewToolkit(fabric, fabric.Registry(), hashEmbedder{}, agent.Config{VectorDims: 768, Write: learning.LessonWritePolicy(entity)})
	if err != nil {
		t.Fatal(err)
	}
	sink := &lostLearningReply{Toolkit: toolkit, lose: true}
	mapper := learning.ScopeMapperFunc(func(_ context.Context, scope cortex.Scope) (learning.ScopeBinding, error) {
		return learning.ScopeBinding{Tenant: "acme", Scope: scope}, nil
	})
	config := learning.Config{Entity: entity, Repository: local, Authorizer: learningAuthority{}, IdentityResolver: learningAuthority{}}
	adapter, err := learning.NewLessonStore(sink, fabric, mapper, config)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := opt.NewPublisher(opt.PublisherConfig{Repository: local, Authorizer: learningAuthority{}, IdentityResolver: learningAuthority{}, Caller: learningAuthority{}, Sink: adapter})
	if err != nil {
		t.Fatal(err)
	}
	if err = publisher.WorkOnce(ctx); err != nil {
		t.Fatal(err)
	}
	delivered, err := local.GetPublication(ctx, request.Lesson.ID, 1)
	if err != nil || delivered.CompletedAt.IsZero() || delivered.RemoteVersion != 1 {
		t.Fatalf("publication: %+v %v", delivered, err)
	}
	// Reopen local state before replaying the delivery through a new adapter.
	if err = local.Close(); err != nil {
		t.Fatal(err)
	}
	local = openOptimizationStore(t, path)
	config.Repository = local
	adapter, err = learning.NewLessonStore(sink, fabric, mapper, config)
	if err != nil {
		t.Fatal(err)
	}
	if receipt, deliverErr := adapter.Deliver(ctx, delivered); deliverErr != nil || receipt.Version != 1 {
		t.Fatalf("replayed create: %+v %v", receipt, deliverErr)
	}
	mapped, err := tenant.WithTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	indexer, err := agent.NewIndexer(fabric, fabric.Registry(), hashEmbedder{})
	if err != nil {
		t.Fatal(err)
	}
	if n, indexErr := indexer.Reindex(mapped, entity); indexErr != nil || n != 1 {
		t.Fatalf("index: %d %v", n, indexErr)
	}
	recall := opt.RecallQuery{Scope: request.Lesson.Scope, Domain: request.Lesson.Domain, HarnessSchema: 1, Query: request.Lesson.Statement, Cutoff: time.Now().UTC(), Limit: 10}
	page, err := adapter.Recall(ctx, recall)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("recall: %+v %v", page, err)
	}
	before, err := local.GetExperiment(ctx, request.ExperimentID)
	if err != nil {
		t.Fatal(err)
	}
	if err = local.Transact(ctx, func(tx store.Tx) error {
		return tx.RevokeLesson(opt.LessonRevocation{LessonID: request.Lesson.ID, ExpectedRevision: 1, Actor: opt.IdentityRef{Subject: "reviewer"}, Reason: "source withdrawn"})
	}); err != nil {
		t.Fatal(err)
	}
	// The vector index and destination still contain the old active payload.
	sink.outage = true
	publisher, err = opt.NewPublisher(opt.PublisherConfig{Repository: local, Authorizer: learningAuthority{}, IdentityResolver: learningAuthority{}, Caller: learningAuthority{}, Sink: adapter})
	if err != nil {
		t.Fatal(err)
	}
	if err = publisher.WorkOnce(ctx); !errors.Is(err, opt.ErrUnavailable) {
		t.Fatalf("revocation outage: %v", err)
	}
	retry, err := local.GetPublication(ctx, request.Lesson.ID, 2)
	if err != nil || retry.NextRetry.IsZero() || retry.TerminalError != "" {
		t.Fatalf("revocation lost retry: %+v %v", retry, err)
	}
	page, err = adapter.Recall(ctx, recall)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("local revocation did not fence stale recall: %+v %v", page, err)
	}
	after, err := local.GetExperiment(ctx, request.ExperimentID)
	if err != nil || after.DecisionID != before.DecisionID {
		t.Fatal("publication outage changed decision")
	}
	sink.outage = false
	time.Sleep(time.Until(retry.NextRetry))
	if err = publisher.WorkOnce(ctx); err != nil {
		t.Fatal(err)
	}
	withdrawn, err := local.GetPublication(ctx, request.Lesson.ID, 2)
	if err != nil || withdrawn.CompletedAt.IsZero() || withdrawn.RemoteVersion != 2 {
		t.Fatalf("withdrawal not acknowledged: %+v %v", withdrawn, err)
	}
	if _, err = adapter.Deliver(ctx, delivered); !errors.Is(err, opt.ErrStaleVersion) {
		t.Fatalf("late publish revived withdrawn lesson: %v", err)
	}
	var rows []map[string]any
	if err = fabric.Relational().List(mapped, entity, query.ListQuery{}, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["active"] != false {
		t.Fatalf("remote withdrawal: %+v", rows)
	}
	foreign, err := tenant.WithTenant(ctx, "foreign")
	if err != nil {
		t.Fatal(err)
	}
	rows = nil
	if err = fabric.Relational().List(foreign, entity, query.ListQuery{}, &rows); err != nil || len(rows) != 0 {
		t.Fatalf("tenant isolation: %+v %v", rows, err)
	}
}
