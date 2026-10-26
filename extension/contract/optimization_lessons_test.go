package contract

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	dashauth "github.com/xraph/forge/extensions/dashboard/auth"
	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/memory"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestOptimizationLessonContractRecovery(t *testing.T) {
	for _, action := range []string{"retry", "revoke"} {
		t.Run(action, func(t *testing.T) {
			repo, ctx, published := storetest.PublishedLessonFixture(t, func(t *testing.T, now func() time.Time) store.Store {
				s, e := memory.New(memory.WithClock(now))
				if e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() { _ = s.Close() })
				return s
			})
			var claim opt.Publication
			if err := repo.Transact(ctx, func(tx store.Tx) error {
				var e error
				claim, e = tx.ClaimPublication(opt.PublicationClaim{LessonID: published.Lesson.ID, LocalRevision: 1, Owner: "worker", Actor: published.Actor, Duration: time.Minute})
				return e
			}); err != nil {
				t.Fatal(err)
			}
			if err := repo.Transact(ctx, func(tx store.Tx) error { return tx.FailPublication(claim.DeliveryLease, "conflict", time.Time{}, true) }); err != nil {
				t.Fatal(err)
			}
			auth := &optimizationTestAuthority{}
			service, err := opt.NewOptimizer(opt.OptimizerConfig{Repository: repo, Caller: auth, Authorizer: auth, IdentityResolver: auth, Handler: auth, Worker: opt.DefaultWorkerConfig()})
			if err != nil {
				t.Fatal(err)
			}
			scope := cortex.ScopeFromContext(ctx)
			principal := dc.Principal{User: &dashauth.UserInfo{Subject: "fixture", Scopes: []string{"cortex.optimization.manage"}}, Claims: map[string]any{"cortex_scope": scope}}
			auditFailure := false
			audit := func(_ context.Context, event AuditEvent) error {
				if auditFailure && event.Outcome == "succeeded" {
					return errors.New("audit unavailable")
				}
				return nil
			}
			d, registry, wardens, cache := optimizationCommandDispatcher(t, service, scope, audit)
			input := map[string]any{"id": published.Lesson.ID.String(), "reason": "Operator reviewed lesson", "idempotency_key": "forged"}
			if action == "retry" {
				input["local_revision"] = 1
				input["expected_generation"] = claim.DeliveryLease.Generation
			} else {
				input["expected_revision"] = 1
			}
			intentName := "optimization.lessons." + action
			invoke := func(disp *dispatcher.Dispatcher, p dc.Principal, key string) (json.RawMessage, error) {
				payload, e := json.Marshal(input)
				if e != nil {
					t.Fatal(e)
				}
				data, _, e := disp.Dispatch(context.Background(), dc.Request{Contributor: ContributorName, Intent: intentName, IntentVersion: 1, Kind: dc.KindCommand, IdempotencyKey: key, Payload: payload}, p)
				return data, e
			}
			for _, scopes := range [][]string{nil, {"cortex.manage"}, {"cortex.run"}, {"cortex.optimization.read"}, {"cortex.optimization.run"}, {"cortex.optimization.activate"}} {
				denied := principal
				denied.User = &dashauth.UserInfo{Subject: "fixture", Scopes: scopes}
				if _, err = invoke(d, denied, "command"); !errors.Is(err, dc.ErrPermissionDenied) {
					t.Fatalf("permission %v: %v", scopes, err)
				}
			}
			if _, err = invoke(d, principal, ""); !errors.Is(err, dc.ErrBadRequest) {
				t.Fatalf("payload supplied key: %v", err)
			}
			missingAudit, _, _, _ := optimizationCommandDispatcher(t, service, scope, nil)
			if _, err = invoke(missingAudit, principal, "command"); !errors.Is(err, dc.ErrUnavailable) {
				t.Fatalf("missing audit: %v", err)
			}
			unavailable, _, _, _ := optimizationCommandDispatcher(t, nil, scope, audit)
			if _, err = invoke(unavailable, principal, "command"); !errors.Is(err, dc.ErrUnavailable) {
				t.Fatalf("missing optimizer: %v", err)
			}
			foreign := principal
			foreign.Claims = map[string]any{"cortex_scope": cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "foreign"}}}}
			if _, err = invoke(d, foreign, "command"); !errors.Is(err, dc.ErrNotFound) {
				t.Fatalf("foreign lesson: %v", err)
			}
			input["id"] = published.ExperimentID.String()
			if _, err = invoke(d, principal, "command"); !errors.Is(err, dc.ErrBadRequest) {
				t.Fatalf("wrong identity kind: %v", err)
			}
			input["id"] = published.Lesson.ID.String()
			auth.denied = true
			if _, err = invoke(d, principal, "command"); !errors.Is(err, dc.ErrPermissionDenied) {
				t.Fatalf("host denial: %v", err)
			}
			auth.denied = false
			auditFailure = true
			_, err = invoke(d, principal, "command")
			var failure *dc.Error
			if !errors.As(err, &failure) || failure.Code != dc.CodeUnavailable || failure.Details["command_attempted"] != true {
				t.Fatalf("lost audit outcome: %v", err)
			}
			auditFailure = false
			recovered, err := invoke(d, principal, "command")
			if err != nil {
				t.Fatal(err)
			}
			auth.denied = true
			if _, err = invoke(d, principal, "command"); !errors.Is(err, dc.ErrPermissionDenied) {
				t.Fatalf("retry skipped current permission: %v", err)
			}
			auth.denied = false
			input["reason"] = "Changed request"
			if _, err = invoke(d, principal, "command"); !errors.Is(err, dc.ErrConflict) {
				t.Fatalf("changed payload: %v", err)
			}
			input["reason"] = "Operator reviewed lesson"
			expected := []string{"optimization.lessons.list", "optimization.lessons.detail", "optimization.publications.list", "optimization.capabilities", "optimization.events.list"}
			if action == "revoke" {
				expected = append(expected, "optimization.experiments.list", "optimization.experiments.detail", "optimization.comparison")
			}
			intent, ok := registry.Intent(ContributorName, intentName, 1)
			if !ok || !reflect.DeepEqual(intent.Invalidates, expected) {
				t.Fatalf("invalidations: %+v", intent)
			}
			receipt := optimizationHTTPReceipt[json.RawMessage](t, d, registry, wardens, principal, intentName, "command", input, expected)
			if !reflect.DeepEqual(receipt, recovered) {
				t.Fatal("HTTP replay changed command result")
			}
			if cache.calls != 0 {
				t.Fatalf("used generic replay cache: %d", cache.calls)
			}
			events, err := repo.ListEvents(ctx, published.ExperimentID, 0, 500)
			if err != nil {
				t.Fatal(err)
			}
			eventType := "lesson_publication_retry_requested"
			if action == "revoke" {
				eventType = "lesson_revoked"
			}
			count := 0
			for _, event := range events.Items {
				if event.Type == eventType {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("committed command events: %d", count)
			}
			if action == "retry" {
				var publication opt.Publication
				if err = json.Unmarshal(recovered, &publication); err != nil {
					t.Fatal(err)
				}
				if publication.LastRetry == nil || publication.Attempts != 1 || publication.Receipt != nil {
					t.Fatalf("retry did not leave delivery pending: %+v", publication)
				}
			} else {
				var lesson opt.Lesson
				if err = json.Unmarshal(recovered, &lesson); err != nil {
					t.Fatal(err)
				}
				if lesson.Revision != 2 || lesson.RevokedAt.IsZero() {
					t.Fatalf("lesson not withdrawn: %+v", lesson)
				}
			}
		})
	}
}
