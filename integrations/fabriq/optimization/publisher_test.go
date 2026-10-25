package optimization

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/command"
	"github.com/xraph/fabriq/core/fabriqtest"
	"github.com/xraph/fabriq/core/query"
	"github.com/xraph/fabriq/core/registry"
	"github.com/xraph/fabriq/core/tenant"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	opt "github.com/xraph/cortex/optimization"
)

type localEvidence struct {
	opt.Repository
	publication opt.Publication
	previous    *opt.Publication
	revoked     bool
}

func (s *localEvidence) GetPublication(_ context.Context, key id.ID, revision int64) (opt.Publication, error) {
	if s.previous != nil && key == s.previous.LessonID && revision == s.previous.LocalRevision {
		return *s.previous, nil
	}
	if key != s.publication.LessonID || revision != s.publication.LocalRevision {
		return opt.Publication{}, opt.ErrNotFound
	}
	return s.publication, nil
}
func (s *localEvidence) GetExperiment(context.Context, id.ID) (opt.Experiment, error) {
	return opt.Experiment{ID: s.publication.ExperimentID, Spec: opt.ExperimentSpec{Scope: s.publication.Payload.Scope, Requester: opt.IdentityRef{Subject: "requester"}}}, nil
}
func (s *localEvidence) ResolveLesson(_ context.Context, ref opt.LessonRef) (opt.Lesson, error) {
	if s.revoked {
		return opt.Lesson{}, opt.ErrStaleVersion
	}
	if ref.ID != s.publication.LessonID || ref.Revision != s.publication.LocalRevision || ref.Digest != s.publication.PayloadDigest {
		return opt.Lesson{}, opt.ErrNotFound
	}
	return s.publication.Payload, nil
}

type allowAuthority struct{}

func (allowAuthority) Authorize(context.Context, opt.Action, opt.Resource) error { return nil }
func (allowAuthority) Resolve(ctx context.Context, _ opt.IdentityRef, _ cortex.Scope) (context.Context, error) {
	return ctx, nil
}

type lostReply struct {
	Toolkit
	lose bool
}

func (tk *lostReply) Remember(ctx context.Context, request agent.RememberRequest) (command.Result, error) {
	result, err := tk.Toolkit.Remember(ctx, request)
	if err == nil && tk.lose {
		tk.lose = false
		return command.Result{}, errors.New("acknowledgement lost")
	}
	return result, err
}

func adapterFixture(t *testing.T) (*Adapter, *localEvidence, query.Fabric, context.Context) {
	t.Helper()
	reg := registry.New()
	reg.MustRegister(LessonSpec("optimization_lessons"))
	if err := reg.Validate(); err != nil {
		t.Fatal(err)
	}
	world := fabriqtest.NewWorld(reg)
	fabric := fabriqtest.NewFabric(world)
	tk, err := agent.NewToolkit(fabric, reg, nil, agent.Config{Write: LessonWritePolicy("optimization_lessons")})
	if err != nil {
		t.Fatal(err)
	}
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "project", Value: "project-one"}}}
	lesson := opt.Lesson{ID: id.New(opt.PrefixLesson), Scope: scope, Domain: "reasoning", HarnessSchema: 1, Statement: "Check source evidence", Revision: 1, CreatedAt: time.Now().UTC().Truncate(time.Microsecond), EvidenceClass: opt.EvidenceExploratory, DecisionIDs: []id.ID{id.New(opt.PrefixDecision)}}
	digest, err := opt.LessonDigest(lesson)
	if err != nil {
		t.Fatal(err)
	}
	local := &localEvidence{publication: opt.Publication{LessonID: lesson.ID, LocalRevision: 1, ExperimentID: id.New(opt.PrefixExperiment), Operation: opt.PublicationPublish, Payload: lesson, PayloadDigest: digest, RemoteAggregateID: "optimization/" + lesson.ID.String()}}
	mapper := ScopeMapperFunc(func(_ context.Context, scope cortex.Scope) (ScopeBinding, error) {
		return ScopeBinding{Tenant: "acme", Scope: scope}, nil
	})
	adapter, err := NewLessonStore(&lostReply{Toolkit: tk, lose: true}, fabric, mapper, Config{Entity: "optimization_lessons", Repository: local, Authorizer: allowAuthority{}, IdentityResolver: allowAuthority{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := cortex.WithScope(context.Background(), scope)
	return adapter, local, fabric, ctx
}

func TestPublishAmbiguousCreateReadsDigest(t *testing.T) {
	adapter, local, fabric, ctx := adapterFixture(t)
	for range 2 {
		receipt, err := adapter.Deliver(ctx, local.publication)
		if err != nil {
			t.Fatal(err)
		}
		if receipt.AggregateID != local.publication.RemoteAggregateID || receipt.PayloadDigest != local.publication.PayloadDigest || receipt.Version != 1 {
			t.Fatalf("wrong receipt: %+v", receipt)
		}
	}
	mapped, err := tenant.WithTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err = fabric.Relational().GetMany(mapped, "optimization_lessons", []string{local.publication.RemoteAggregateID}, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["content"] != local.publication.Payload.Statement {
		t.Fatalf("lost content: %+v", rows)
	}
}

func TestConflictingDigestRefused(t *testing.T) {
	adapter, local, _, ctx := adapterFixture(t)
	if _, err := adapter.Deliver(ctx, local.publication); err != nil {
		t.Fatal(err)
	}
	local.publication.Payload.Statement = "different lesson"
	digest, err := opt.LessonDigest(local.publication.Payload)
	if err != nil {
		t.Fatal(err)
	}
	local.publication.PayloadDigest = digest
	if _, err = adapter.Deliver(ctx, local.publication); !errors.Is(err, opt.ErrConflict) {
		t.Fatalf("conflicting payload: %v", err)
	}
}

func revokeFixture(t *testing.T, local *localEvidence) {
	t.Helper()
	prior := local.publication
	local.previous = &prior
	local.revoked = true
	local.publication.LocalRevision = 2
	local.publication.Operation = opt.PublicationRevoke
	local.publication.Payload.Revision = 2
	local.publication.Payload.RevokedAt = time.Now().UTC()
	local.publication.Payload.RevocationReason = "source withdrawn"
	digest, err := opt.LessonDigest(local.publication.Payload)
	if err != nil {
		t.Fatal(err)
	}
	local.publication.PayloadDigest = digest
}

func TestPublishRetryCannotUndoRevocation(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "revoke_before_create", true: "revoke_after_create"}[published], func(t *testing.T) {
			adapter, local, _, ctx := adapterFixture(t)
			if published {
				if _, err := adapter.Deliver(ctx, local.publication); err != nil {
					t.Fatal(err)
				}
			}
			revokeFixture(t, local)
			for range 2 {
				receipt, err := adapter.Deliver(ctx, local.publication)
				if err != nil {
					t.Fatal(err)
				}
				expected := int64(1)
				if published {
					expected = 2
				}
				if receipt.Version != expected {
					t.Fatalf("revocation created duplicate version: %+v", receipt)
				}
			}
			if _, err := adapter.Deliver(ctx, *local.previous); !errors.Is(err, opt.ErrStaleVersion) {
				t.Fatalf("revoked publish replay: %v", err)
			}
		})
	}
}

type unavailableWrites struct{ Toolkit }

func (unavailableWrites) Remember(context.Context, agent.RememberRequest) (command.Result, error) {
	return command.Result{}, errors.New("destination unavailable")
}

func TestRevocationOutageRemainsRetryable(t *testing.T) {
	adapter, local, _, ctx := adapterFixture(t)
	if _, err := adapter.Deliver(ctx, local.publication); err != nil {
		t.Fatal(err)
	}
	revokeFixture(t, local)
	adapter.toolkit = unavailableWrites{Toolkit: adapter.toolkit}
	if _, err := adapter.Deliver(ctx, local.publication); !errors.Is(err, opt.ErrUnavailable) {
		t.Fatalf("transient withdrawal failure became terminal: %v", err)
	}
}
