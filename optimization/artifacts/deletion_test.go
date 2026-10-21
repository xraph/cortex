package artifacts_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/artifacts"
	"github.com/xraph/cortex/optimization/store/memory"
	"github.com/xraph/cortex/optimization/store/sqlite"
	"github.com/xraph/cortex/optimization/store/storetest"
)

type deleteFunc func(context.Context, optimization.ArtifactDeletion) (artifacts.DeletionReceipt, error)

func (f deleteFunc) Tombstone(ctx context.Context, req optimization.ArtifactDeletion) (artifacts.DeletionReceipt, error) {
	return f(ctx, req)
}

type authorizeFunc func(context.Context, optimization.Action, optimization.Resource) error

func (f authorizeFunc) Authorize(ctx context.Context, action optimization.Action, resource optimization.Resource) error {
	return f(ctx, action, resource)
}

type resolveFunc func(context.Context, optimization.IdentityRef, cortex.Scope) (context.Context, error)

func (f resolveFunc) Resolve(ctx context.Context, actor optimization.IdentityRef, scope cortex.Scope) (context.Context, error) {
	return f(ctx, actor, scope)
}

func fixture(t *testing.T) (*memory.Store, context.Context, optimization.ArtifactDeletion) {
	t.Helper()
	s, err := memory.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := s.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	ctx, request := seed(t, s)
	return s, ctx, request
}

func seed(t *testing.T, s optimization.Repository) (context.Context, optimization.ArtifactDeletion) {
	t.Helper()
	bundle := storetest.Fixture(t, "deletion-service")
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	var lease optimization.WorkLease
	err := s.Transact(ctx, func(tx optimization.Transaction) error {
		if createErr := tx.CreateExperiment(bundle); createErr != nil {
			return createErr
		}
		return tx.CommitTransition(optimization.Transition{ExperimentID: bundle.Experiment.ID, ExpectedRevision: 1, NextState: optimization.StateQueued, IdempotencyKey: "start", Actor: bundle.Experiment.Spec.Requester})
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Transact(ctx, func(tx optimization.Transaction) error {
		var claimErr error
		lease, claimErr = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: bundle.Experiment.ID, Owner: "worker", Duration: time.Minute})
		return claimErr
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("trace data"))
	ref := optimization.ArtifactRef{ExperimentID: bundle.Experiment.ID, Scope: bundle.Experiment.Spec.Scope, Digest: hex.EncodeToString(sum[:]), Size: 10, MediaType: "text/plain", Kind: "trace", AccessPolicy: "private"}
	err = s.Transact(ctx, func(tx optimization.Transaction) error {
		return tx.RegisterArtifact(optimization.ArtifactRegistration{Ref: ref, CreatedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour), Actor: bundle.Experiment.Spec.Requester, Lease: lease})
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, optimization.ArtifactDeletion{Ref: ref, RequestID: "remove-1", Actor: bundle.Experiment.Spec.Requester, Reason: "source removed"}
}

func receipt(req optimization.ArtifactDeletion) artifacts.DeletionReceipt {
	binding, _ := req.Ref.BindingDigest()
	return artifacts.DeletionReceipt{BindingDigest: binding, RequestID: req.RequestID, Actor: req.Actor, Reason: req.Reason}
}
func service(t *testing.T, s artifacts.DeletionMetadata, provider deleteFunc, auth authorizeFunc, resolver resolveFunc) *artifacts.DeletionService {
	t.Helper()
	if auth == nil {
		auth = func(context.Context, optimization.Action, optimization.Resource) error { return nil }
	}
	if resolver == nil {
		resolver = func(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
			return ctx, nil
		}
	}
	service, err := artifacts.NewDeletionService(s, provider, auth, resolver)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestDeletionRecoveryAfterDatabaseReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "optimizer.db")
	open := func() *sqlite.Store {
		driver := sqlitedriver.New()
		if err := driver.Open(context.Background(), path); err != nil {
			t.Fatal(err)
		}
		db, err := grove.Open(driver)
		if err != nil {
			t.Fatal(err)
		}
		s := sqlite.New(db)
		if err = s.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if closeErr := s.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
		return s
	}
	first := open()
	ctx, req := seed(t, first)
	provider := deleteFunc(func(context.Context, optimization.ArtifactDeletion) (artifacts.DeletionReceipt, error) {
		return artifacts.DeletionReceipt{}, optimization.ErrUnknownAttempt
	})
	if err := service(t, first, provider, nil, nil).Delete(ctx, req); !errors.Is(err, optimization.ErrUnknownAttempt) {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := open()
	provider = func(_ context.Context, saved optimization.ArtifactDeletion) (artifacts.DeletionReceipt, error) {
		return receipt(saved), nil
	}
	if err := service(t, second, provider, nil, nil).Recover(ctx, req.Ref); err != nil {
		t.Fatal(err)
	}
	got, err := second.GetArtifact(ctx, req.Ref)
	if err != nil || got.Revision != 3 || got.DeletedAt.IsZero() {
		t.Fatalf("reopened state not completed: %+v %v", got, err)
	}
}

func TestDeletionRecoversSavedRequestAfterUnknownOutcome(t *testing.T) {
	s, ctx, req := fixture(t)
	calls := 0
	provider := deleteFunc(func(ctx context.Context, actual optimization.ArtifactDeletion) (artifacts.DeletionReceipt, error) {
		calls++
		stored, err := s.GetArtifact(ctx, req.Ref)
		if err != nil || stored.Deletion == nil || stored.Revision != 2 {
			t.Fatalf("provider ran before durable invalidation: %+v %v", stored, err)
		}
		if actual.RequestID != req.RequestID || actual.Actor != req.Actor || actual.Reason != req.Reason {
			t.Fatal("retry changed deletion authority")
		}
		if calls == 1 {
			return artifacts.DeletionReceipt{}, optimization.ErrUnknownAttempt
		}
		return receipt(actual), nil
	})
	first := service(t, s, provider, nil, nil)
	if err := first.Delete(ctx, req); !errors.Is(err, optimization.ErrUnknownAttempt) {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("automatic side effect retry")
	}
	stored, err := s.GetArtifact(ctx, req.Ref)
	if err != nil || !stored.DeletedAt.IsZero() || stored.Deletion == nil {
		t.Fatal("uncertain outcome lost")
	}
	second := service(t, s, provider, nil, nil)
	if err = second.Recover(ctx, req.Ref); err != nil {
		t.Fatal(err)
	}
	if err = second.Recover(ctx, req.Ref); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("completed deletion repeated: %d", calls)
	}
	stored, err = s.GetArtifact(ctx, req.Ref)
	if err != nil || stored.Revision != 3 || stored.DeletedAt.IsZero() {
		t.Fatal("missing completed tombstone")
	}
}

func TestDeletionRejectsUnprovenAndRevokedCompletion(t *testing.T) {
	for _, mode := range []string{"binding", "request", "actor", "reason", "revoked", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			s, parent, req := fixture(t)
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			denied := false
			provider := deleteFunc(func(context.Context, optimization.ArtifactDeletion) (artifacts.DeletionReceipt, error) {
				got := receipt(req)
				switch mode {
				case "binding":
					got.BindingDigest = strings.Repeat("b", 64)
				case "request":
					got.RequestID = "other"
				case "actor":
					got.Actor.Subject = "other"
				case "reason":
					got.Reason = "other"
				case "revoked":
					denied = true
				case "cancelled":
					cancel()
				}
				return got, nil
			})
			auth := authorizeFunc(func(context.Context, optimization.Action, optimization.Resource) error {
				if denied {
					return optimization.ErrPolicyDenied
				}
				return nil
			})
			svc := service(t, s, provider, auth, nil)
			if err := svc.Delete(ctx, req); err == nil {
				t.Fatal("unproven deletion completed")
			}
			got, err := s.GetArtifact(parent, req.Ref)
			if err != nil || got.Deletion == nil || !got.DeletedAt.IsZero() {
				t.Fatalf("pending state lost: %+v %v", got, err)
			}
		})
	}
}

func TestDeletionDenialPrecedesInvalidation(t *testing.T) {
	s, ctx, req := fixture(t)
	provider := deleteFunc(func(context.Context, optimization.ArtifactDeletion) (artifacts.DeletionReceipt, error) {
		t.Fatal("denied request reached provider")
		return artifacts.DeletionReceipt{}, nil
	})
	auth := authorizeFunc(func(context.Context, optimization.Action, optimization.Resource) error {
		return optimization.ErrPolicyDenied
	})
	svc := service(t, s, provider, auth, nil)
	if err := svc.Delete(ctx, req); !errors.Is(err, optimization.ErrPolicyDenied) {
		t.Fatal(err)
	}
	got, err := s.GetArtifact(ctx, req.Ref)
	if err != nil || got.Deletion != nil {
		t.Fatal("denied deletion mutated metadata")
	}
}

type completionFailure struct {
	artifacts.DeletionMetadata
	fail bool
}
type completionTransaction struct{ optimization.Transaction }

func (completionTransaction) CompleteArtifactDeletion(optimization.ArtifactRef, string) error {
	return optimization.ErrUnavailable
}
func (s *completionFailure) Transact(ctx context.Context, fn func(optimization.Transaction) error) error {
	return s.DeletionMetadata.Transact(ctx, func(tx optimization.Transaction) error {
		if s.fail {
			return fn(completionTransaction{tx})
		}
		return fn(tx)
	})
}

func TestDeletionProviderSuccessSurvivesMetadataFailure(t *testing.T) {
	s, ctx, req := fixture(t)
	metadata := &completionFailure{DeletionMetadata: s, fail: true}
	calls := 0
	provider := deleteFunc(func(context.Context, optimization.ArtifactDeletion) (artifacts.DeletionReceipt, error) {
		calls++
		return receipt(req), nil
	})
	auth := authorizeFunc(func(context.Context, optimization.Action, optimization.Resource) error { return nil })
	resolver := resolveFunc(func(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
		return ctx, nil
	})
	svc, err := artifacts.NewDeletionService(metadata, provider, auth, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Delete(ctx, req); !errors.Is(err, optimization.ErrUnavailable) {
		t.Fatal(err)
	}
	got, err := s.GetArtifact(ctx, req.Ref)
	if err != nil || got.Deletion == nil || !got.DeletedAt.IsZero() {
		t.Fatal("metadata failure discarded pending intent")
	}
	metadata.fail = false
	if err = svc.Recover(ctx, req.Ref); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("unexpected provider attempts: %d", calls)
	}
}

func TestDeletionRecoveryRequiresOriginalCurrentIdentity(t *testing.T) {
	for _, mode := range []string{"revoked", "nil", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			s, ctx, req := fixture(t)
			if err := s.Transact(ctx, func(tx optimization.Transaction) error { return tx.BeginArtifactDeletion(req) }); err != nil {
				t.Fatal(err)
			}
			provider := deleteFunc(func(context.Context, optimization.ArtifactDeletion) (artifacts.DeletionReceipt, error) {
				t.Fatal("invalid identity reached content")
				return artifacts.DeletionReceipt{}, nil
			})
			resolver := resolveFunc(func(ctx context.Context, actor optimization.IdentityRef, scope cortex.Scope) (context.Context, error) {
				if actor != req.Actor || scope.Canonical() != req.Ref.Scope.Canonical() {
					t.Fatal("saved identity changed")
				}
				switch mode {
				case "revoked":
					return nil, optimization.ErrPolicyDenied
				case "nil":
					return nil, nil
				default:
					scope.Levels[0].Value = "foreign"
					return cortex.WithScope(ctx, scope), nil
				}
			})
			if err := service(t, s, provider, nil, resolver).Recover(ctx, req.Ref); !errors.Is(err, optimization.ErrPolicyDenied) {
				t.Fatal(err)
			}
		})
	}
}

func TestDeletionResolverCannotDropDeadline(t *testing.T) {
	s, parent, req := fixture(t)
	ctx, cancel := context.WithTimeout(parent, time.Minute)
	defer cancel()
	want, _ := ctx.Deadline()
	provider := deleteFunc(func(ctx context.Context, _ optimization.ArtifactDeletion) (artifacts.DeletionReceipt, error) {
		got, ok := ctx.Deadline()
		if !ok || got.After(want) {
			t.Fatal("resolver extended caller deadline")
		}
		return receipt(req), nil
	})
	resolver := resolveFunc(func(_ context.Context, _ optimization.IdentityRef, scope cortex.Scope) (context.Context, error) {
		return cortex.WithScope(context.Background(), scope), nil
	})
	if err := service(t, s, provider, nil, resolver).Delete(ctx, req); err != nil {
		t.Fatal(err)
	}
}

func TestDeletionCallbacksCannotMutateCallerScope(t *testing.T) {
	s, ctx, req := fixture(t)
	want := req.Ref.Scope.Canonical()
	auth := authorizeFunc(func(ctx context.Context, _ optimization.Action, resource optimization.Resource) error {
		scope := cortex.ScopeFromContext(ctx)
		scope.Levels[0].Value = "changed"
		resource.Scope.Levels[0].Value = "changed resource"
		return optimization.ErrPolicyDenied
	})
	provider := deleteFunc(func(context.Context, optimization.ArtifactDeletion) (artifacts.DeletionReceipt, error) {
		t.Fatal("denied call reached provider")
		return artifacts.DeletionReceipt{}, nil
	})
	if err := service(t, s, provider, auth, nil).Delete(ctx, req); !errors.Is(err, optimization.ErrPolicyDenied) {
		t.Fatal(err)
	}
	if cortex.ScopeFromContext(ctx).Canonical() != want || req.Ref.Scope.Canonical() != want {
		t.Fatal("callback mutated caller scope")
	}
}

func TestDeletionRecoverNeverCreatesRequest(t *testing.T) {
	s, ctx, req := fixture(t)
	provider := deleteFunc(func(context.Context, optimization.ArtifactDeletion) (artifacts.DeletionReceipt, error) {
		t.Fatal("recovery invented deletion")
		return artifacts.DeletionReceipt{}, nil
	})
	svc := service(t, s, provider, nil, nil)
	if err := svc.Recover(ctx, req.Ref); !errors.Is(err, optimization.ErrConflict) {
		t.Fatal(err)
	}
	if err := svc.Delete(context.Background(), req); !errors.Is(err, cortex.ErrNoScope) {
		t.Fatal(err)
	}
	foreign := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "foreign"}}})
	if err := svc.Delete(foreign, req); !errors.Is(err, optimization.ErrPolicyDenied) {
		t.Fatal(err)
	}
	got, err := s.GetArtifact(ctx, req.Ref)
	if err != nil || got.Revision != 1 {
		t.Fatal("preflight mutated retention")
	}
}

func TestDeletionRechecksGrantsOnCompletedRetry(t *testing.T) {
	s, ctx, req := fixture(t)
	denied := false
	calls := 0
	auth := authorizeFunc(func(context.Context, optimization.Action, optimization.Resource) error {
		if denied {
			return optimization.ErrPolicyDenied
		}
		return nil
	})
	provider := deleteFunc(func(context.Context, optimization.ArtifactDeletion) (artifacts.DeletionReceipt, error) {
		calls++
		return receipt(req), nil
	})
	svc := service(t, s, provider, auth, nil)
	if err := svc.Delete(ctx, req); err != nil {
		t.Fatal(err)
	}
	denied = true
	if err := svc.Recover(ctx, req.Ref); !errors.Is(err, optimization.ErrPolicyDenied) {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("completed retry called provider")
	}
}

func TestDeletionRequiresAllServices(t *testing.T) {
	s, _, _ := fixture(t)
	provider := deleteFunc(func(context.Context, optimization.ArtifactDeletion) (artifacts.DeletionReceipt, error) {
		return artifacts.DeletionReceipt{}, nil
	})
	auth := authorizeFunc(func(context.Context, optimization.Action, optimization.Resource) error { return nil })
	resolver := resolveFunc(func(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
		return ctx, nil
	})
	for _, test := range []struct {
		metadata artifacts.DeletionMetadata
		provider artifacts.Tombstoner
		auth     optimization.Authorizer
		resolver optimization.IdentityResolver
	}{
		{nil, provider, auth, resolver}, {s, nil, auth, resolver}, {s, provider, nil, resolver}, {s, provider, auth, nil},
	} {
		if _, err := artifacts.NewDeletionService(test.metadata, test.provider, test.auth, test.resolver); !errors.Is(err, optimization.ErrInvalidSpec) {
			t.Fatal(err)
		}
	}
}
