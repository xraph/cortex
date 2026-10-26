package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestOptimizationLessonHTTPRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lessons.db")
	repo, ctx, p := storetest.PublishedLessonFixture(t, func(t *testing.T, _ func() time.Time) store.Store { return openOptimizationHTTPStore(t, path) })
	t.Cleanup(func() { _ = repo.Close() })
	var claim opt.Publication
	if err := repo.Transact(ctx, func(tx store.Tx) error {
		var e error
		claim, e = tx.ClaimPublication(opt.PublicationClaim{LessonID: p.Lesson.ID, LocalRevision: 1, Owner: "worker", Actor: p.Actor, Duration: time.Minute})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Transact(ctx, func(tx store.Tx) error { return tx.FailPublication(claim.DeliveryLease, "conflict", time.Time{}, true) }); err != nil {
		t.Fatal(err)
	}
	auth := &optimizationAuthority{}
	newHandler := func() http.Handler {
		service, e := opt.NewOptimizer(opt.OptimizerConfig{Repository: repo, Caller: auth, Authorizer: auth, IdentityResolver: auth, Handler: auth, Worker: opt.DefaultWorkerConfig()})
		if e != nil {
			t.Fatal(e)
		}
		return New(nil, nil, WithOptimization(service)).Handler()
	}
	handler := newHandler()
	producer, loadErr := repo.GetExperiment(ctx, p.ExperimentID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	consumer := storetest.Fixture(t, "lesson-consumer")
	consumer.Experiment.Spec = producer.Spec
	consumer.Experiment.Spec.LessonCutoff = time.Now().Add(time.Second)
	consumer.Experiment.EvaluationPlan = producer.EvaluationPlan
	consumer.Experiment.LessonIDs = []id.ID{p.Lesson.ID}
	consumer.Versions = nil
	if err := repo.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(consumer) }); err != nil {
		t.Fatal(err)
	}
	start := optimizationPOST(ctx, t, handler, "/experiments/"+consumer.Experiment.ID.String()+"/start", []string{"consumer-start"}, map[string]any{"expected_revision": 1})
	if start.Code != 200 {
		t.Fatalf("queue consumer: %d %s", start.Code, start.Body)
	}

	// The URL owns lesson identity even when a body supplies a different ID.
	retry := map[string]any{"id": p.ExperimentID.String(), "local_revision": 1, "expected_generation": claim.DeliveryLease.Generation, "reason": "Destination repaired", "idempotency_key": "forged"}
	revoke := map[string]any{"id": p.ExperimentID.String(), "expected_revision": 1, "reason": "Evidence withdrawn", "idempotency_key": "forged"}
	call := func(c context.Context, action, key string, input any, status int) []byte {
		response := optimizationPOST(c, t, handler, "/lessons/"+p.Lesson.ID.String()+"/"+action, []string{key}, input)
		if response.Code != status {
			t.Fatalf("%s: %d want %d: %s", action, response.Code, status, response.Body)
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("command response can be cached")
		}
		return response.Body.Bytes()
	}
	for action, input := range map[string]any{"retry": retry, "revoke": revoke} {
		call(ctx, action, "", input, 400)
		call(context.Background(), action, "key", input, 403)
	}
	foreign := cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "foreign"}}})
	call(foreign, "retry", "retry", retry, 404)
	call(foreign, "revoke", "revoke", revoke, 404)
	auth.denied = true
	call(ctx, "retry", "retry", retry, 403)
	auth.denied = false
	retry["expected_generation"] = claim.DeliveryLease.Generation + 1
	call(ctx, "retry", "retry", retry, 409)
	retry["expected_generation"] = claim.DeliveryLease.Generation
	bad := optimizationPOST(ctx, t, handler, "/lessons/"+p.ExperimentID.String()+"/retry", []string{"retry"}, retry)
	if bad.Code != 400 {
		t.Fatalf("wrong URL identity: %d %s", bad.Code, bad.Body)
	}
	var retried opt.Publication
	if err := json.Unmarshal(call(ctx, "retry", "retry", retry, 200), &retried); err != nil {
		t.Fatal(err)
	}
	if retried.LastRetry == nil || retried.Attempts != 1 || retried.TerminalError != "" || retried.Receipt != nil {
		t.Fatalf("retry did not queue pending publication: %+v", retried)
	}
	reopen := func() {
		if err := repo.Close(); err != nil {
			t.Fatal(err)
		}
		repo = openOptimizationHTTPStore(t, path)
		handler = newHandler()
	}
	reopen()
	var recovered opt.Publication
	if err := json.Unmarshal(call(ctx, "retry", "retry", retry, 200), &recovered); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recovered, retried) {
		t.Fatal("restart changed retry receipt")
	}
	retry["reason"] = "changed reason"
	call(ctx, "retry", "retry", retry, 409)
	retry["reason"] = "Destination repaired"
	call(ctx, "revoke", "retry", revoke, 409)
	var revoked opt.Lesson
	if err := json.Unmarshal(call(ctx, "revoke", "revoke", revoke, 200), &revoked); err != nil {
		t.Fatal(err)
	}
	if revoked.Revision != 2 || revoked.RevokedAt.IsZero() {
		t.Fatalf("local lesson not withdrawn: %+v", revoked)
	}
	reopen()
	var again opt.Lesson
	if err := json.Unmarshal(call(ctx, "revoke", "revoke", revoke, 200), &again); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, revoked) {
		t.Fatal("restart changed revocation")
	}
	if err := json.Unmarshal(call(ctx, "retry", "retry", retry, 200), &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.SupersededAt.IsZero() || recovered.Attempts != 1 {
		t.Fatal("old retry revived publication")
	}
	auth.denied = true
	call(ctx, "retry", "retry", retry, 403)
	call(ctx, "revoke", "revoke", revoke, 403)
	auth.denied = false
	response := optimizationGET(ctx, handler, "/lessons/"+p.Lesson.ID.String())
	if response.Code != 200 {
		t.Fatalf("lesson detail: %d %s", response.Code, response.Body)
	}
	if err := json.Unmarshal(response.Body.Bytes(), &again); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, revoked) {
		t.Fatal("lesson detail lost withdrawal")
	}
	consumerResponse := optimizationGET(ctx, handler, "/experiments/"+consumer.Experiment.ID.String())
	if consumerResponse.Code != 200 {
		t.Fatalf("consumer detail: %d %s", consumerResponse.Code, consumerResponse.Body)
	}
	var paused opt.Experiment
	if err := json.Unmarshal(consumerResponse.Body.Bytes(), &paused); err != nil {
		t.Fatal(err)
	}
	if paused.State != opt.StatePaused || paused.Failure != "lesson_revoked" || len(paused.FrozenLessons) != 1 || paused.FrozenLessons[0].ID != p.Lesson.ID {
		t.Fatalf("revocation lost consumer invalidation: %+v", paused)
	}
	withdrawal, err := repo.GetPublication(ctx, p.Lesson.ID, 2)
	if err != nil || withdrawal.Operation != opt.PublicationRevoke || withdrawal.Attempts != 0 || withdrawal.Receipt != nil {
		t.Fatalf("remote withdrawal was not left pending: %+v %v", withdrawal, err)
	}
	events, err := repo.ListEvents(ctx, p.ExperimentID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	retries, revokes := 0, 0
	for _, event := range events.Items {
		if event.Type == "lesson_publication_retry_requested" {
			retries++
		}
		if event.Type == "lesson_revoked" {
			revokes++
		}
	}
	if retries != 1 || revokes != 1 {
		t.Fatalf("duplicate commands: retries=%d revokes=%d", retries, revokes)
	}
}
