package fabriqbrain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/command"
	"github.com/xraph/fabriq/core/query"
	"github.com/xraph/fabriq/core/registry"
	"github.com/xraph/fabriq/core/tenant"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/fabriq/internal/lessontransport"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/learning"
)

// LessonToolkit owns the same typed write policy and strict recall used by optimization.
type LessonToolkit = lessontransport.Toolkit

// LessonSpec supplies the shared physically provisioned lesson entity schema.
func LessonSpec(entity string) registry.EntitySpec { return lessontransport.Spec(entity) }

// NewLessonToolkit verifies the schema and configures strict semantic retrieval.
func NewLessonToolkit(fabric query.Fabric, reg *registry.Registry, embedder agent.Embedder, entity string) (*LessonToolkit, error) {
	return lessontransport.NewToolkit(fabric, reg, embedder, entity)
}

// LessonTransport is the installed Fabriq toolkit boundary. Decorators must embed
// LessonToolkit so its private write-policy binding remains verifiable.
type LessonTransport interface {
	Remember(context.Context, agent.RememberRequest) (command.Result, error)
	Recall(context.Context, agent.RecallRequest) (agent.ContextPack, error)
}

// LessonConfig requires local accepted state and explicit current source authority.
type LessonConfig struct {
	Entity     string
	Repository learning.Repository
	Guard      *learning.Guard
	Tenant     func(context.Context, cortex.Scope) (string, error)
	Now        func() time.Time
}

// LessonAdapter publishes and recalls accepted ordinary lessons through Fabriq.
type LessonAdapter struct {
	toolkit LessonTransport
	fabric  query.Fabric
	config  LessonConfig
}

var _ learning.Store = (*LessonAdapter)(nil)

// NewLessonStore accepts only a policy-bound toolkit; CheckReady verifies the
// physical destination and configured semantic channels before serving calls.
func NewLessonStore(toolkit LessonTransport, fabric query.Fabric, config LessonConfig) (*LessonAdapter, error) {
	if hostport.IsNil(toolkit) || hostport.IsNil(fabric) || hostport.IsNil(config.Repository) || config.Guard == nil || config.Tenant == nil || !lessontransport.ValidEntity(config.Entity) || !lessontransport.BoundPolicy(toolkit, config.Entity) {
		return nil, learning.ErrInvalid
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &LessonAdapter{toolkit: toolkit, fabric: fabric, config: config}, nil
}

func (a *LessonAdapter) mapped(ctx context.Context) (context.Context, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	copyScope := cortex.Scope{Levels: append([]cortex.Level(nil), scope.Levels...)}
	value, err := a.config.Tenant(ctx, copyScope)
	if err != nil {
		return nil, err
	}
	if previous, priorErr := tenant.FromContext(ctx); priorErr == nil && previous != value {
		return nil, learning.ErrDenied
	}
	if !tenant.Valid(value) {
		return nil, learning.ErrDenied
	}
	return tenant.WithTenant(ctx, value)
}

// CheckReady checks every physical column and strict recall without writing a lesson.
func (a *LessonAdapter) CheckReady(ctx context.Context) error {
	for _, action := range []learning.Action{learning.ActionRead, learning.ActionPublish, learning.ActionRevoke} {
		if err := a.config.Guard.Authorize(ctx, action, id.Nil, ""); err != nil {
			return err
		}
	}
	mapped, err := a.mapped(ctx)
	if err != nil {
		return err
	}
	filters := query.Where{query.Eq("scope_key", cortex.ScopeFromContext(ctx).Canonical())}
	for _, column := range LessonSpec(a.config.Entity).Schema.Columns {
		filters = append(filters, query.Or(query.IsNull(column.Name), query.IsNotNull(column.Name)))
	}
	var rows []map[string]any
	if err = a.fabric.Relational().List(mapped, a.config.Entity, query.ListQuery{Where: filters, Limit: 1}, &rows); err != nil {
		return learning.ErrUnavailable
	}
	filters = append(filters, query.Eq("id", id.New(learning.PrefixLesson).String()))
	pack, err := a.toolkit.Recall(mapped, agent.RecallRequest{Query: "lesson readiness", Budget: 128000, K: 1, Entities: []string{a.config.Entity}, Filters: filters})
	if err != nil || len(pack.Items) != 0 || len(pack.Warnings) != 0 || pack.Omitted != 0 {
		return learning.ErrUnavailable
	}
	return ctx.Err()
}

// Publish reconciles only a matching accepted local publication.
func (a *LessonAdapter) Publish(ctx context.Context, lesson learning.Lesson) (learning.Receipt, error) {
	p, err := a.config.Repository.GetLessonPublication(ctx, lesson.ID, lesson.Revision)
	if err != nil {
		return learning.Receipt{}, err
	}
	digest, err := learning.Digest(lesson)
	if err != nil || digest != p.PayloadDigest || p.Operation != "publish" {
		return learning.Receipt{}, learning.ErrConflict
	}
	return a.Deliver(ctx, p)
}

// Deliver reconciles an immutable outbox payload, including withdrawal after a
// source is gone. The publisher retains the receipt in local durable storage.
func (a *LessonAdapter) Deliver(ctx context.Context, request learning.Publication) (learning.Receipt, error) {
	p, err := a.config.Repository.GetLessonPublication(ctx, request.LessonID, request.Revision)
	if err != nil {
		return learning.Receipt{}, err
	}
	if p.PayloadDigest != request.PayloadDigest || p.AggregateID != request.AggregateID || p.Operation != request.Operation || p.SourceID != request.SourceID {
		return learning.Receipt{}, learning.ErrConflict
	}
	action := learning.ActionPublish
	if p.Operation == "revoke" {
		action = learning.ActionRevoke
	} else if p.Operation != "publish" {
		return learning.Receipt{}, learning.ErrInvalid
	}
	if authErr := a.config.Guard.Authorize(ctx, action, p.LessonID, p.Payload.Domain); authErr != nil {
		return learning.Receipt{}, authErr
	}
	var receipt learning.Receipt
	deliver := func(operation context.Context, _ learning.Lesson) error {
		mapped, mapErr := a.mapped(operation)
		if mapErr != nil {
			return mapErr
		}
		tenantID, tenantErr := tenant.FromContext(mapped)
		if tenantErr != nil {
			return tenantErr
		}
		row, reconcileErr := lessontransport.Reconcile(mapped, a.toolkit, a.fabric, a.config.Entity, tenantID, p, func(c context.Context) (learning.Publication, error) {
			return a.config.Repository.GetLessonPublication(c, p.LessonID, p.Revision-1)
		})
		if reconcileErr != nil {
			return reconcileErr
		}
		if authErr := a.config.Guard.Authorize(operation, action, p.LessonID, p.Payload.Domain); authErr != nil {
			return authErr
		}
		receipt = learning.Receipt{LessonID: p.LessonID, PayloadDigest: p.PayloadDigest, AggregateID: p.AggregateID, Version: row.Version, AcknowledgedAt: a.config.Now().UTC()}
		return operation.Err()
	}
	if p.Operation == "revoke" {
		current, readErr := a.config.Repository.GetLesson(ctx, p.LessonID)
		if readErr != nil || current.Revision != p.Revision || current.RevokedAt.IsZero() {
			return learning.Receipt{}, learning.ErrIneligible
		}
		if deliverErr := deliver(ctx, current); deliverErr != nil {
			return learning.Receipt{}, deliverErr
		}
	} else {
		if _, err = a.config.Guard.WithLesson(ctx, learning.Ref{ID: p.LessonID, Revision: p.Revision, Digest: p.PayloadDigest}, action, deliver); err != nil {
			return learning.Receipt{}, err
		}
	}
	return receipt, ctx.Err()
}

// Revoke commits local withdrawal before attempting remote delivery.
func (a *LessonAdapter) Revoke(ctx context.Context, request learning.RevokeRequest) error {
	p, err := a.config.Guard.Revoke(ctx, request)
	if err != nil {
		return err
	}
	_, err = a.Deliver(ctx, p)
	return err
}

// Recall applies exact eligibility predicates in Fabriq's authoritative hydration,
// then checks current source evidence and host grants before returning content.
func (a *LessonAdapter) Recall(ctx context.Context, request learning.Query) (learning.Page, error) {
	empty := learning.Page{Items: []learning.Lesson{}}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return empty, err
	}
	now := a.config.Now().UTC()
	if request.Scope.Canonical() != scope.Canonical() || strings.TrimSpace(request.Domain) == "" || len(request.Domain) > 256 || strings.TrimSpace(request.Query) == "" || len(request.Query) > 32768 || request.HarnessSchema != 1 || request.Limit < 1 || request.Limit > 100 || request.Cutoff.IsZero() || request.Cutoff.After(now) || request.Cursor != "" {
		return empty, learning.ErrInvalid
	}
	if authErr := a.config.Guard.Authorize(ctx, learning.ActionRead, id.Nil, request.Domain); authErr != nil {
		return empty, authErr
	}
	mapped, err := a.mapped(ctx)
	if err != nil {
		return empty, err
	}
	tenantID, err := tenant.FromContext(mapped)
	if err != nil {
		return empty, err
	}
	pack, err := a.toolkit.Recall(mapped, agent.RecallRequest{Query: request.Query, Budget: 128000, K: request.Limit, Entities: []string{a.config.Entity}, Filters: query.Where{query.Eq("scope_key", scope.Canonical()), query.Eq("domain", request.Domain), query.Eq("harness_schema", request.HarnessSchema), query.Eq("evidence_class", string(learning.EvidenceValidated)), query.Eq("active", true), query.Lte("created_at", request.Cutoff), query.Gt("expires_at", now)}})
	if err != nil {
		return empty, learning.ErrUnavailable
	}
	if len(pack.Warnings) != 0 || pack.Omitted != 0 || len(pack.Items) > request.Limit {
		return empty, learning.ErrIneligible
	}
	result, seen := learning.Page{Items: []learning.Lesson{}}, make(map[string]bool)
	for _, item := range pack.Items {
		if item.Entity != a.config.Entity || seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		var raw map[string]any
		if json.Unmarshal(item.Row, &raw) != nil {
			continue
		}
		row, decodeErr := lessontransport.Decode(raw)
		if decodeErr != nil || row.ID != item.ID || row.ScopeKey != scope.Canonical() || row.Domain != request.Domain || row.HarnessSchema != 1 || row.EvidenceClass != learning.EvidenceValidated || !row.Active || row.CreatedAt.After(request.Cutoff) || row.ExpiresAt == nil || !now.Before(*row.ExpiresAt) {
			continue
		}
		p, readErr := a.config.Repository.GetLessonPublication(ctx, row.Lesson.ID, row.LocalRevision)
		if errors.Is(readErr, learning.ErrNotFound) || errors.Is(readErr, learning.ErrIneligible) {
			continue
		}
		if readErr != nil {
			return empty, readErr
		}
		if !row.Matches(p, tenantID) {
			continue
		}
		lesson, resolveErr := a.config.Guard.WithLesson(ctx, learning.Ref{ID: p.LessonID, Revision: p.Revision, Digest: p.PayloadDigest}, learning.ActionRead, func(operation context.Context, _ learning.Lesson) error {
			hydration, mapErr := a.mapped(operation)
			if mapErr != nil {
				return mapErr
			}
			authoritative, lookupErr := lessontransport.Read(hydration, a.fabric, a.config.Entity, p.AggregateID)
			if lookupErr != nil {
				return lookupErr
			}
			if !authoritative.Matches(p, tenantID) {
				return learning.ErrIneligible
			}
			return nil
		})
		if errors.Is(resolveErr, learning.ErrIneligible) || errors.Is(resolveErr, learning.ErrNotFound) {
			continue
		}
		if resolveErr != nil {
			return empty, resolveErr
		}
		result.Items = append(result.Items, lesson)
	}
	return result, ctx.Err()
}
