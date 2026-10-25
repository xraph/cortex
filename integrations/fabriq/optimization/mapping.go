package optimization

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/fabriq/core/tenant"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
)

type lessonRow struct {
	ID            string            `json:"id,omitempty"`
	Tenant        string            `json:"tenant_id,omitempty"`
	Version       int64             `json:"version,omitempty"`
	ScopeKey      string            `json:"scope_key"`
	Domain        string            `json:"domain"`
	HarnessSchema int               `json:"harness_schema"`
	EvidenceClass opt.EvidenceClass `json:"evidence_class"`
	LocalRevision int64             `json:"local_revision"`
	Digest        string            `json:"payload_digest"`
	ExperimentID  string            `json:"experiment_id"`
	Content       string            `json:"content"`
	Active        bool              `json:"active"`
	CreatedAt     time.Time         `json:"created_at"`
	ExpiresAt     *time.Time        `json:"expires_at"`
	Lesson        opt.Lesson        `json:"lesson"`
}

func rowFor(p opt.Publication) lessonRow {
	row := lessonRow{ScopeKey: p.Payload.Scope.Canonical(), Domain: p.Payload.Domain, HarnessSchema: p.Payload.HarnessSchema, EvidenceClass: p.Payload.EvidenceClass, LocalRevision: p.LocalRevision, Digest: p.PayloadDigest, ExperimentID: p.ExperimentID.String(), Content: p.Payload.Statement, Active: p.Operation == opt.PublicationPublish, CreatedAt: p.Payload.CreatedAt, Lesson: p.Payload}
	if !p.Payload.ExpiresAt.IsZero() {
		expiry := p.Payload.ExpiresAt
		row.ExpiresAt = &expiry
	}
	return row
}

func decodeRow(raw map[string]any) (lessonRow, error) {
	// SQL drivers may expose JSON columns as bytes; preserve JSON rather than
	// encoding those bytes as a base64 string.
	if data, ok := raw["lesson"].([]byte); ok {
		raw["lesson"] = json.RawMessage(data)
	}
	var row lessonRow
	encoded, err := json.Marshal(raw)
	if err != nil {
		return row, opt.ErrIncompleteEvidence
	}
	if err = json.Unmarshal(encoded, &row); err != nil {
		return row, opt.ErrIncompleteEvidence
	}
	return row, nil
}

func (row lessonRow) matches(p opt.Publication, tenantID string) bool {
	digest, err := opt.LessonDigest(row.Lesson)
	expected := rowFor(p)
	sameExpiry := row.ExpiresAt == nil && expected.ExpiresAt == nil
	if row.ExpiresAt != nil && expected.ExpiresAt != nil {
		sameExpiry = row.ExpiresAt.Truncate(time.Microsecond).Equal(expected.ExpiresAt.Truncate(time.Microsecond))
	}
	return err == nil && digest == p.PayloadDigest && row.Digest == digest && row.ID == p.RemoteAggregateID && row.Tenant == tenantID && row.Version > 0 && row.ScopeKey == expected.ScopeKey && row.Domain == expected.Domain && row.HarnessSchema == expected.HarnessSchema && row.EvidenceClass == expected.EvidenceClass && row.LocalRevision == expected.LocalRevision && row.ExperimentID == expected.ExperimentID && row.Content == expected.Content && row.Active == expected.Active && row.CreatedAt.Truncate(time.Microsecond).Equal(expected.CreatedAt.Truncate(time.Microsecond)) && sameExpiry
}

func (a *Adapter) mapped(ctx context.Context) (context.Context, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	key, err := opt.ScopeKey(scope)
	if err != nil {
		return nil, err
	}
	detached := cortex.Scope{Levels: append([]cortex.Level(nil), scope.Levels...)}
	binding, err := a.mapper.Map(ctx, detached)
	if err != nil {
		return nil, err
	}
	mappedKey, err := opt.ScopeKey(binding.Scope)
	if err != nil || mappedKey != key {
		return nil, opt.ErrPolicyDenied
	}
	if current, currentErr := tenant.FromContext(ctx); currentErr == nil && current != binding.Tenant {
		return nil, opt.ErrPolicyDenied
	}
	if !tenant.Valid(binding.Tenant) {
		return nil, opt.ErrPolicyDenied
	}
	return tenant.WithTenant(ctx, binding.Tenant)
}

func (a *Adapter) authorize(ctx context.Context, p opt.Publication, action opt.Action) (context.Context, context.CancelFunc, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	scope := cortex.ScopeFromContext(ctx)
	key, err := opt.ScopeKey(scope)
	if err != nil {
		return nil, nil, err
	}
	if key != p.Payload.Scope.Canonical() {
		return nil, nil, opt.ErrPolicyDenied
	}
	experiment, err := a.config.Repository.GetExperiment(ctx, p.ExperimentID)
	if err != nil {
		return nil, nil, err
	}
	if experiment.ID != p.ExperimentID || experiment.Spec.Scope.Canonical() != key {
		return nil, nil, opt.ErrPolicyDenied
	}
	resource := opt.Resource{Scope: scope, AgentID: experiment.Spec.AgentID, ID: p.LessonID, Kind: "lesson"}
	if authErr := a.config.Authorizer.Authorize(ctx, action, resource); authErr != nil {
		return nil, nil, authErr
	}
	resolved, cleanup, err := opt.ResolveIdentity(ctx, a.config.IdentityResolver, experiment.Spec.Requester, scope)
	if err != nil {
		return nil, nil, err
	}
	if err = a.config.Authorizer.Authorize(resolved, action, resource); err != nil {
		cleanup()
		return nil, nil, err
	}
	mapped, err := a.mapped(resolved)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return mapped, cleanup, nil
}
