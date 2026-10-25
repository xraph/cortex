package optimization

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/xraph/fabriq/core/agent"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
)

type recalledPack struct {
	Toolkit
	pack    agent.ContextPack
	request agent.RecallRequest
}

func (tk *recalledPack) Recall(_ context.Context, request agent.RecallRequest) (agent.ContextPack, error) {
	tk.request = request
	return tk.pack, nil
}

func TestRecallRejectsForeignExpiredRevokedEvidence(t *testing.T) {
	for _, scenario := range []string{"eligible", "foreign_scope", "wrong_domain", "wrong_schema", "expired", "revoked", "future", "missing_local", "corrupt", "warning", "omitted"} {
		t.Run(scenario, func(t *testing.T) {
			adapter, local, _, ctx := adapterFixture(t)
			now := time.Now().UTC()
			adapter.config.Now = func() time.Time { return now }
			row := rowFor(local.publication)
			row.ID = local.publication.RemoteAggregateID
			row.Tenant = "acme"
			row.Version = 1
			switch scenario {
			case "foreign_scope":
				row.ScopeKey = "project=foreign"
			case "wrong_domain":
				row.Domain = "other"
			case "wrong_schema":
				row.HarnessSchema = 2
			case "expired":
				expiry := now.Add(-time.Second)
				row.ExpiresAt = &expiry
			case "revoked":
				local.revoked = true
			case "future":
				row.CreatedAt = now.Add(time.Hour)
			case "missing_local":
				row.LocalRevision = 9
			case "corrupt":
				row.Digest = "wrong"
			}
			raw, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			tk := &recalledPack{Toolkit: adapter.toolkit, pack: agent.ContextPack{Items: []agent.ContextItem{{Entity: adapter.config.Entity, ID: row.ID, Row: raw}}}}
			if scenario == "warning" {
				tk.pack.Warnings = []string{"partial hydration"}
			}
			if scenario == "omitted" {
				tk.pack.Omitted = 1
			}
			adapter.toolkit = tk
			result, err := adapter.Recall(ctx, opt.RecallQuery{Scope: local.publication.Payload.Scope, Domain: "reasoning", HarnessSchema: 1, Query: "evidence", Cutoff: now, Limit: 10})
			if scenario == "warning" || scenario == "omitted" {
				if !errors.Is(err, opt.ErrIncompleteEvidence) {
					t.Fatalf("incomplete search: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			expected := 0
			if scenario == "eligible" {
				expected = 1
			}
			if len(result.Items) != expected {
				t.Fatalf("ineligible recall: %+v", result)
			}
			if len(tk.request.Filters) < 5 || len(tk.request.Entities) != 1 || tk.request.Entities[0] != adapter.config.Entity {
				t.Fatal("authoritative filters absent")
			}
		})
	}
}

func TestScopeMappingCannotWidenAuthority(t *testing.T) {
	adapter, local, _, ctx := adapterFixture(t)
	adapter.mapper = ScopeMapperFunc(func(context.Context, cortex.Scope) (ScopeBinding, error) {
		return ScopeBinding{Tenant: "acme", Scope: cortex.Scope{Levels: []cortex.Level{{Key: "project", Value: "other"}}}}, nil
	})
	if _, err := adapter.Deliver(ctx, local.publication); !errors.Is(err, opt.ErrPolicyDenied) {
		t.Fatalf("widened mapping: %v", err)
	}
}

func TestLessonSchemaIndexesEligibilityAndProvenance(t *testing.T) {
	spec := LessonSpec("optimization_lessons")
	indexed := map[string]bool{}
	for _, index := range spec.Schema.Indexes {
		for _, column := range index.Columns {
			indexed[column] = true
		}
	}
	for _, column := range []string{"scope_key", "domain", "harness_schema", "evidence_class", "active", "expires_at", "experiment_id", "payload_digest"} {
		if !indexed[column] {
			t.Errorf("missing index for %s", column)
		}
	}
}
